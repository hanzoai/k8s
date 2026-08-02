# hanzoai/k8s

The one Hanzo Kubernetes service. `/v1/k8s` is the whole API, and it is a **cluster
registry** plus a scoped manager over whichever cluster an org's resources live on.

```
GET    /v1/k8s/clusters              every cluster the calling org may reach
POST   /v1/k8s/clusters              register one (kubeconfig -> validated -> sealed in KMS)
GET    /v1/k8s/clusters/:cluster     one registration
DELETE /v1/k8s/clusters/:cluster     deregister, and destroy the sealed credential
GET    /v1/k8s/clusters/:cluster/status
GET    /v1/k8s/nodes?cluster=…       and 30 more named, typed operations
```

## Multi-cluster is the shape, not a feature

lux runs `lux-k8s`. zoo runs `zoo-k8s`. Customers bring their own. So an org's resources
may live on a different cluster from Hanzo's, and "our cluster" is not a thing this
program can mean.

**Every operation names its cluster.** The name is resolved through the registry against
the tenant the call acts for, and `registry.Reach` is the only constructor of a Kubernetes
client in the module — so no function can talk to a cluster it did not name, and there is
no ambient in-cluster fallback to inherit. `hanzo-k8s`, `lux-k8s` and a customer's own
cluster are three rows and one code path.

## The credential lives in KMS and is resolved at use

A registration validates its kubeconfig ([`registry.SafeRESTConfig`](registry/config.go) —
no exec credential plugins, no non-routable apiserver), uses it once to prove the cluster
answers, seals it in KMS under `orgs/<org>/k8s/clusters/<name>/kubeconfig`, and drops it.
Nothing is written to disk, nothing is put in an environment variable, nothing is logged,
and no operation returns it.

The ref is **derived** from the owning org and the cluster name rather than stored, which
is what lets the KMS service apply its own tenant rule to every resolution — a second
refusal, in a second process, of a cross-org read.

## Cross-org access is impossible, and there is a test that proves it

- The registry index is **per-org**. A cluster another org registered answers **404**, not
  403: a distinguishable refusal would be an oracle for enumerating other tenants' cluster
  names.
- Two orgs may register the same NAME. They resolve to different endpoints with different
  credentials.
- A registration on a shared cluster carries a **namespace bound**. A namespace outside it
  is refused before the apiserver is dialed, and refused again by the credential's RBAC.
- Cluster-scoped reads (nodes, PersistentVolumes, the kubelet sweep) are refused outright
  for a bounded registration.

[`registry/authz_test.go`](registry/authz_test.go) is written from the attacker's side, and
each guard was verified by REMOVING it and observing the red:

| Guard removed | Tests that fail |
|---|---|
| the org segment in `indexRef` | `TestForeignClusterNameIsNotFound`, `TestSameNameInTwoOrgsAreTwoClusters`, `TestRefsAreOrgPrefixed`, `TestDeregisterCannotReachAcrossOrgs` |
| the bound check in `Bound.Scope` | `TestForeignNamespaceIsRefused`, `TestBoundedListDoesNotWiden` |
| the validated-user test in `Tenant` | `TestOrgHeaderWithoutPrincipalIsRefused` |

[`ops/ops_test.go`](ops/ops_test.go) proves the same refusals arrive on the real wire, with
the headers a gateway attaches.

## No generic passthrough

Every operation is named and typed, including the custom-resource quartet, whose kinds come
from a **closed table** in [`ops/resources.go`](ops/resources.go). The function this
replaces decoded arbitrary YAML, resolved its kind through a discovery `RESTMapper`, and
create-or-updated any kind the cluster served — with a kubeconfig read out of a database
row. With that, no ClusterRole smaller than "everything" could be written.

With this set, the union of privilege is a finite list, and it is in
[`rbac.yaml`](rbac.yaml) — the first written statement of it anywhere in the fleet.
`""/secrets` is deliberately absent: material reaches a namespace as a `KMSSecret` the
kms-operator resolves, and nothing here reads a Secret back.

## Consumers ask; they do not link

[`github.com/hanzoai/k8s/plane`](plane/plane.go) is the call contract: op tokens and their
In/Out types, **one package, stdlib only**. A consumer links it and calls
`zip.Ask[plane.PodsIn, plane.Pods](ctx, "k8s", plane.PodsList, in)`. It never links
client-go, and it never holds a cluster credential.

```
this binary          630 packages   (zip 257 + the k8s.io/sigs.k8s.io family 286)
plane contract         1 package
```

630 is above the ~260 a conforming plugin measures, and the reason is not this repo's
code: client-go's typed clientset alone is ~279 packages, and the typed client is required
— `EvictV1` for a PDB-respecting drain, `GetLogs`, and the kubelet `nodes/proxy` read have
no dynamic-client equivalent. What this repo does control is that **nobody else pays it**:
the same 286 packages used to be linked into four binaries, and the consumer-facing cost is
now one package.

## Build

```
make build      # the binary
make generate   # zipdoc + k8s.plugin.json + openapi.json, all from the live router
make test       # go vet + every gate HIP-0106 §8 requires
```

Conforms to [HIP-0106](../hips/HIPs/hip-0106-hanzo-plugin-contract.md) and HIP-0119.
Deep documentation for agents: [`LLM.md`](LLM.md).

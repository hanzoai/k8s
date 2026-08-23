# hanzoai/k8s — deep notes

The one Kubernetes owner in the fleet. Read this before changing anything here; the README
is the product surface, this is the reasoning.

## Why the repo exists (and why a library would not have worked)

Nine places in `hanzoai/cloud` built a Kubernetes client. Measured, not estimated:

| Holder | Credential | Cluster it assumed |
|---|---|---|
| `apps/platform` ×2, `apps/deploy` ×2, `apps/provisioning`, `apps/validators`, `apps/cron` | in-cluster ServiceAccount → `KUBECONFIG` | one ambient |
| `apps/ml` | in-cluster SA, re-scoped to a token file; federated via `fleet.DynForOrg` | ambient **or** the org's BYO |
| `apps/fleet` | **KMS-sealed kubeconfig** per org+project | N registered BYO |
| `apps/admin/infra` | **`DO_API_TOKEN` → a cluster-admin kubeconfig per cluster** | N DOKS clusters |
| `hanzoai/ai/cluster` | **kubeconfig in a database row**, fed to an apply-any-YAML function | one global mutable singleton |

Two of those are the reason this is a service and not a shared package:

1. **`ai/cluster/client.go:210 deployResource`** decoded arbitrary YAML, resolved the GVK
   through a discovery `RESTMapper`, and create-or-updated **any kind the cluster served**.
   With a DB-sourced kubeconfig, one edited row reached any object on any cluster. It is
   DELETED, not ported. Its callers go through `resources.apply` with a named kind.
2. **`admin/infra`** held cluster-admin on every cluster on the house account to perform
   nine operations. `nodes/proxy get` alone is arbitrary kubelet access.

A library cannot fix either, because the defect is not the code — it is that N binaries
hold cluster credentials. One owner, named ops, credentials in KMS, consumers ASK.

The **deciding** argument, though, is neither of those. It is that **lux runs its own
`lux-k8s`**: an org's resources may live on a different cluster from Hanzo's, plus BYO for
customers. A single-cluster client, however cleanly factored, cannot express that and would
have been rewritten. Hence multi-cluster from the first line.

## The model

```
Cluster{Name, Org, Provider, Endpoint, Nodes, NvidiaGPU, AmdGPU, Namespaces, Registered, Default}
```

- **No Kubeconfig field and no Credential field.** The ref is DERIVED —
  `orgs/<org>/k8s/clusters/<name>/kubeconfig` — because a ref stored as data could be
  edited to name another org's material. That is the same defect as letting a caller name
  its own org, one level down.
- **`Org` is set from the validated caller**, never from an argument, and appears only on
  replies (where it is a property of the thing described).
- **`Namespaces`** bounds a registration made with a scoped credential on a shared cluster.
  Empty means the whole cluster, which is what an org registering its own cluster gets.

`registry.Reach(ctx, name) (*Bound, error)` is the only entry point:

1. `Tenant(ctx)` — zip's full rule (validated user claim, non-empty org, under
   `MaxOrgLen`) plus one thing zip cannot know: the org becomes a KMS ref segment, so a
   value containing `/` or `..` is refused here.
2. the name is looked up in **that org's index only** → `ErrNoCluster` (404) otherwise.
3. the credential is resolved from KMS over the call plane, carrying the caller, so KMS
   re-applies its own `orgs/<org>/` rule.
4. `SafeRESTConfig` → typed + dynamic + metrics clients, cached per `org/name`.

`Bound.Scope(ns)` / `Bound.One(ns)` / `Bound.WholeCluster()` are the namespace gate, and
every namespaced op goes through one of them. A named namespace outside the bound is
REFUSED rather than silently narrowed — a caller that asked for the wrong thing must be
told, or it will believe an empty answer.

## What a "grant" is

An org may reach a cluster it **owns**. There is no grant table, and that is a decision
rather than an omission: cross-org credential sharing is refused by KMS by construction (a
call acting for B asking for `orgs/A/…` is forbidden), so a grant table would have been a
second authority that could only ever disagree with the first.

Granting org B access to a cluster A owns is therefore: seal a scoped ServiceAccount
kubeconfig **under B**, and register it in B's own scope with a `namespaces` bound. That is
one existing verb, no new concept, and it is honest — to reach a cluster you must hold a
credential for it, sealed in your own org's KMS.

The platform's own path for a tenant on the shared cluster is the same call, made by the
provisioning job with `zip.WithCaller` stating the tenant it acts for.

## The 36 ops

`plane/plane.go` is the whole contract. Groups: `clusters` (5), `nodes` (3), `namespaces`
(4), `pods` (2), `volumes` (4), `workloads` (8), `jobs` (3), `resources` (5), `access` (1),
`metrics` (1).

Each is registered TWICE from ONE handler: on the app (HTTP, which projects OpenAPI + an
MCP tool + a CLI command) and on `app.Peer()` (the fleet's 0600 socket, invoked by op
token through `zip.Ask`). The registrations are written out as direct `zip.Get` /
`zip.Post` calls with literal paths **deliberately** — zipdoc reads the AST for exactly
that shape, so a registration wrapped in a helper is a registration whose doc comment
reaches neither the OpenAPI description nor the MCP tool description.

Design notes worth keeping:

- **`resources.watch` is a bounded read, not a stream.** A stream cannot cross the call
  plane as a value; rendering one (SSE, a websocket) is the edge's concern. It returns the
  changes within `waitSeconds` and the cursor to resume from, and `expired` is the field
  that matters — a too-old cursor rendered as an empty page is how a watch-backed board
  silently stops updating.
- **`nodes.cordon` is one op over three calls** (patch `spec.unschedulable`, list the
  node's pods, evict each). There is no generic node patch, because one would let a caller
  write any field of any node's spec. The drain evicts rather than deletes, so a
  PodDisruptionBudget refusal is the system working.
- **`volumes.expandClaim` takes `gi`, not a patch.** Growing the claim is the one way that
  leaves claim, PV, device and filesystem agreeing. Nothing deletes a claim.
- **`namespaces.delete` is separately gated** and refused for a bounded registration: it
  garbage-collects every object in the namespace, claims included.
- **`nodes.volumeStats` is its own op** so `nodes/proxy` can be dropped from every other
  caller's grant. It is the only source of volume fill we have (no cloud provider exposes
  it; metrics-server is absent from most of our clusters) and it reports `nodesRead` /
  `nodesTotal`, because a claim with no row is UNMEASURED, which is a different fact from
  empty.
- **`metrics.pods` reports `available`.** A cluster with no metrics-server rendered as
  zeroes tells an operator a busy namespace is idle.
- **`clusters.status` probes the version endpoint**, not a namespace list: a registration
  bounded to one namespace would fail a namespace list and look dead while being healthy.

## Secrets — the decision

There is **no secrets op**, and the absence is the answer to the question the measurement
report left open.

Three consumers wrote cluster Secrets (`platform/secrets.go`,
`provisioning/addon_inject.go`, `ai/cluster.ensureHfTokenSecret`) and one read one back.
An RPC that reads a tenant Secret is a credential-exfiltration endpoint however carefully
it is gated, and the standing rule is that credentials live in KMS.

The mechanism already exists in-cluster: **`KMSSecret`** (`secrets.lux.network/v1alpha1`)
names a KMS ref, and the kms-operator resolves it into a mounted Secret. It is in the
closed kind set, so `resources.apply` with `kind: KMSSecret` is the write-only projection
the report asked for — no new op, nothing returned, and the material never enters this
process. `platform/secrets.go`'s read-back must be re-sourced from KMS.

## Package weight, stated plainly

```
binary            630 packages   zip 257 · k8s.io 276 · sigs.k8s.io 10 · own 4 · rest stdlib/shared
plane contract      1 package    stdlib only
goja / esbuild      0            zip >= v1.18.7 makes the JS toolchain opt-in
OTLP                0            telemetry rides middleware.Telemetry, nil-safe
kustomize           0            rendering is a deploy concern; it never enters here
```

**630 is not ~260 and never could be.** client-go's typed clientset alone is ~279 packages,
and the typed client is required: `EvictV1` (a PDB-respecting drain), `GetLogs`, and the
kubelet `nodes/proxy` read have no dynamic-client equivalent. The measurement report
predicted 560–600 and said to say so up front rather than discover it at the end; the
measured answer is 630.

What this repo does control is that **nobody else pays it**. The same ~286 packages were
linked into four binaries; the consumer-facing cost is now one package.

## Operational notes

- **RBAC**: `rbac.yaml` is the union ceiling, gated against `ops.Kinds()` by the suite. On
  a shared cluster do NOT bind it to a tenant — bind a namespaced Role and register the
  cluster with a `namespaces` bound.
- **Migration from `apps/fleet`**: fleet sealed BYO kubeconfigs at
  `<org>/fleet/clusters/<name>/kubeconfig` (no `orgs/` prefix, sharded by project). This
  repo derives `orgs/<org>/k8s/clusters/<name>/kubeconfig` and does not read the old
  shape — deriving the ref is what makes a row unable to name foreign material, so a
  compatibility branch would give that property up. Migration is therefore a one-time
  re-registration per BYO cluster (or a one-shot copy old-ref → new-ref inside KMS), and it
  must happen in the change that removes fleet's registry.
- **Project sub-scope is gone on purpose.** fleet sharded by org+project with a
  backward-compatibility seam that kept default-project keys byte-identical. This is a new
  ref namespace with no existing key to stay identical with, and a cluster belongs to an
  org, so there is one key shape and no seam.
- **`K8S_ALLOW_PRIVATE_HOSTS`** disables the non-routable-apiserver rejection. Tests only.
- **`plane.KMS`/`KMSGet`/`KMSPut`** are the only tokens here this repo does not own. They
  are declared identically in cloud's own plane package; when HIP-0106 migration step 2
  extracts `github.com/hanzoai/plane`, both collapse into that module and these three
  declarations are deleted in the same change. One place to move.

## Open, and needing z's call

1. **DOKS provision/destroy stays with visor.** `POST`/`DELETE /v1/k8s/clusters` in visor
   today provisions and destroys DigitalOcean clusters on the house account — a
   cloud-provider spend action, and providers are visor's job. So `/v1/k8s/clusters` here
   is the REGISTRY (list/get/register/deregister); visor keeps provider-side creation under
   `/v1/compute/…` and calls `clusters.register` when a cluster comes up. This is the one
   place "`/v1/k8s/clusters` survives and moves" and "cloud PROVIDERS are visor's job"
   pull in opposite directions, and it is flagged rather than guessed.
2. **`/v1/clusters` removal is six routes, not a rename.** visor registers `GET`, `POST`
   (BYO attach), `DELETE /:id` (BYO detach), `POST /:clusterId/pools`,
   `POST /:clusterId/pools/:poolId/scale`, `DELETE /:clusterId/pools/:poolId`. The node-pool
   trio is provider work and follows (1). `POST /v1/clusters` — the BYO kubeconfig attach —
   is the single most important operation in the whole set and lands here as
   `clusters.register`. Every caller (console, `hanzo clusters …`, SDKs, docs) moves in the
   same change; forward-only, no alias.

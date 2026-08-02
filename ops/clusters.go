package ops

import (
	"context"
	"errors"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/plane"
)

// listClusters returns every Kubernetes cluster the calling org may reach.
//
// This is the fleet's ONE cluster list. A row is a real registration — a name, the
// provider it came from, its apiserver endpoint and the node and accelerator counts
// measured when it was attached — and there is no such thing as an unregistered
// cluster this API can see. The credential is never part of a row: it is sealed in
// KMS and resolved only at the moment an op uses it.
//
// The list is scoped by the caller's org and by nothing the caller can type, so a
// tenant sees its own clusters and no others.
//
// Response: {"clusters":[{"name":"hanzo-k8s","org":"hanzo","provider":"doks","endpoint":"https://k8s.example.com","nodes":6,"nvidiaGpu":8,"amdGpu":0,"registered":"2026-07-30T00:00:00Z","default":true}]}
func (o Ops) listClusters(ctx context.Context, _ *plane.NoArgs) (*plane.Clusters, error) {
	list, err := o.Reg.List(ctx)
	if err != nil {
		return nil, fail(err)
	}
	out := make([]plane.Registered, 0, len(list))
	for _, c := range list {
		out = append(out, c.View())
	}
	return &plane.Clusters{Clusters: out}, nil
}

// getCluster returns one registered cluster.
//
// A name the calling org has not registered answers 404 — including a name another
// org HAS registered, which is indistinguishable from one that does not exist. That
// is deliberate: a distinguishable refusal would let a tenant enumerate other
// tenants' cluster names one guess at a time.
//
// Response: {"name":"lux-k8s","org":"lux","provider":"byo","endpoint":"https://k8s.lux.example","nodes":12,"nvidiaGpu":32,"amdGpu":0,"registered":"2026-07-30T00:00:00Z","default":false}
func (o Ops) getCluster(ctx context.Context, in *plane.ClusterRef) (*plane.Registered, error) {
	if err := need("cluster", in.Cluster); err != nil {
		return nil, err
	}
	c, err := o.Reg.Get(ctx, in.Cluster)
	if err != nil {
		return nil, fail(err)
	}
	v := c.View()
	return &v, nil
}

// registerCluster attaches a Kubernetes cluster to the calling org and answers 201.
//
// This is the cluster-registry primitive and the most important operation in this
// API: everything else takes a cluster name, and this is where a name comes to
// exist. It is also the whole of BYO-Kubernetes — an org running its own cluster,
// lux running lux-k8s, a customer bringing their own — because a registered cluster
// is a registered cluster whatever provisioned it.
//
// The kubeconfig is refused if it carries an exec or auth-provider credential
// plugin (either would run a local binary with this process's environment), and
// refused if its apiserver is not a routable https host. It is then USED, once, to
// prove the cluster answers and to measure its nodes and accelerators; a cluster
// that cannot be reached is not registered, because the alternative is a row that
// fails at first use when nobody is holding the credential any more. Finally it is
// sealed in KMS under a ref derived from the owning org and the name, and dropped.
// It is never written to disk, never logged, and no operation returns it.
//
// Registration is idempotent on name within the org, so re-registering rotates the
// credential and refreshes the inventory without a second verb.
//
// Set `namespaces` when the credential is a scoped ServiceAccount on a shared
// cluster: the registration is then bounded to those namespaces and every later op
// is refused outside them, before the apiserver is dialed. Omit it when the org owns
// the whole cluster.
//
// Example: {"name":"lux-k8s","kubeconfig":"apiVersion: v1\nkind: Config\n...","provider":"byo","default":true}
// Response: {"name":"lux-k8s","org":"lux","provider":"byo","endpoint":"https://k8s.lux.example","nodes":12,"nvidiaGpu":32,"amdGpu":0,"registered":"2026-07-30T00:00:00Z","default":true}
func (o Ops) registerCluster(ctx context.Context, in *plane.RegisterIn) (*plane.Registered, error) {
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	if err := need("kubeconfig", in.Kubeconfig); err != nil {
		return nil, err
	}
	rec, err := o.Reg.Register(ctx, in)
	if err != nil {
		return nil, fail(err)
	}
	v := rec.View()
	return &v, nil
}

// deregisterCluster detaches a cluster from the calling org and destroys its sealed
// credential.
//
// Nothing in the cluster is touched: this removes Hanzo's ability to reach it, not
// the cluster itself. Deleting a name that is not registered reports removed=false
// rather than failing, so a repeated deregistration is safe.
//
// Response: {"removed":true}
func (o Ops) deregisterCluster(ctx context.Context, in *plane.ClusterRef) (*plane.Deregistered, error) {
	if err := need("cluster", in.Cluster); err != nil {
		return nil, err
	}
	removed, err := o.Reg.Deregister(ctx, in.Cluster)
	if err != nil {
		return nil, fail(err)
	}
	return &plane.Deregistered{Removed: removed}, nil
}

// clusterStatus reports whether a registered cluster answers right now.
//
// The probe is the apiserver's version endpoint, because that is the one read every
// credential can make regardless of what RBAC it was granted: a registration bounded
// to one namespace would fail a namespace list and look dead while being perfectly
// healthy. So this answers "is the credential live and the apiserver reachable",
// which is the question a board asks, and the ops that need a permission ask for
// that permission themselves.
//
// A cluster that does not answer is connected=false with the reason verbatim — never
// an error, because "this cluster is down" is a successful answer to this question.
//
// Response: {"connected":true,"version":"v1.33.1"}
func (o Ops) clusterStatus(ctx context.Context, in *plane.ClusterRef) (*plane.Reachable, error) {
	if err := need("cluster", in.Cluster); err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		// A cluster that is not registered, or that a tenant may not name, is a
		// refusal about the REQUEST and must not be dressed up as a down cluster.
		var he *zip.HTTPError
		if errors.As(err, &he) && he.Status < 500 {
			return nil, err
		}
		return &plane.Reachable{Reason: err.Error()}, nil
	}
	v, verr := b.Typed.Discovery().ServerVersion()
	if verr != nil {
		return &plane.Reachable{Reason: verr.Error()}, nil
	}
	return &plane.Reachable{Connected: true, Version: v.GitVersion}, nil
}

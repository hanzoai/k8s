package ops

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hanzoai/k8s/plane"
)

// managedBy labels everything this app creates, so a cluster can be read for what
// Hanzo put on it without inferring ownership from a naming convention.
const managedBy = "hanzo-cloud"

// listNamespaces returns the namespaces the calling org's registration may address.
//
// For a registration that owns the whole cluster this is every namespace on it. For
// one bounded to a namespace set it is that set, read one namespace at a time — so a
// scoped tenant learns nothing about its neighbours, not even how many there are.
//
// Response: {"namespaces":[{"name":"acme-prod","phase":"Active","labels":{"managed-by":"hanzo-cloud"}}]}
func (o Ops) listNamespaces(ctx context.Context, in *plane.ClusterRef) (*plane.Namespaces, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	if bound := b.Cluster.Namespaces; len(bound) > 0 {
		out := make([]plane.Namespace, 0, len(bound))
		for _, name := range bound {
			ns, err := b.Typed.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, fail(err)
			}
			out = append(out, namespaceView(*ns))
		}
		return &plane.Namespaces{Namespaces: out}, nil
	}
	list, err := b.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fail(err)
	}
	out := make([]plane.Namespace, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, namespaceView(list.Items[i]))
	}
	return &plane.Namespaces{Namespaces: out}, nil
}

// getNamespace returns one namespace.
//
// Response: {"name":"acme-prod","phase":"Active","labels":{"managed-by":"hanzo-cloud"}}
func (o Ops) getNamespace(ctx context.Context, in *plane.NamespaceRef) (*plane.Namespace, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	got, err := b.Typed.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return nil, fail(err)
	}
	v := namespaceView(*got)
	return &v, nil
}

// ensureNamespace creates a namespace if it is absent, and reports which it did.
//
// Four separate implementations of get-then-create-if-missing collapse into this one
// name. That matters beyond tidiness: each of those four wrote the ladder itself, and
// a ladder is a race — "get said absent" and "create" are two calls, and two callers
// starting a tenant at once both take the create branch. Here an AlreadyExists from
// the apiserver is the successful "it was already there" answer, so the race has one
// outcome instead of one winner and one error.
//
// `created` is reported because creating a tenant's namespace and finding it are
// different events, and a caller that cannot tell them apart cannot log the first.
//
// Example: {"cluster":"hanzo-k8s","namespace":"acme-prod","labels":{"tier":"pro"}}
// Response: {"name":"acme-prod","created":true}
func (o Ops) ensureNamespace(ctx context.Context, in *plane.EnsureNamespaceIn) (*plane.Ensured, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	if _, err := b.Typed.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err == nil {
		return &plane.Ensured{Name: ns}, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fail(err)
	}
	labels := map[string]string{"managed-by": managedBy}
	for k, v := range in.Labels {
		labels[k] = v
	}
	_, err = b.Typed.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return &plane.Ensured{Name: ns}, nil
	}
	if err != nil {
		return nil, fail(err)
	}
	return &plane.Ensured{Name: ns, Created: true}, nil
}

// deleteNamespace deletes a namespace and everything in it.
//
// This is the highest blast radius in the whole API: Kubernetes deletes a namespace
// by garbage-collecting every object inside it, including PersistentVolumeClaims,
// which for a Delete-reclaim StorageClass destroys the tenant's data. There is no
// undo and no confirmation this API can offer that would make one.
//
// It is therefore its own named operation, gated separately from every other write,
// and it is refused outright for a registration bounded to a namespace set: a scoped
// credential exists precisely because the cluster is shared, and "delete a namespace
// on a shared cluster" is not an authority a tenant holds.
//
// Response: {"removed":true}
func (o Ops) deleteNamespace(ctx context.Context, in *plane.NamespaceRef) (*plane.Deleted, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	if err := b.WholeCluster(); err != nil {
		return nil, fail(err)
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	err = b.Typed.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return &plane.Deleted{}, nil
	}
	if err != nil {
		return nil, fail(err)
	}
	return &plane.Deleted{Removed: true}, nil
}

func namespaceView(n corev1.Namespace) plane.Namespace {
	return plane.Namespace{Name: n.Name, Phase: string(n.Status.Phase), Labels: n.Labels}
}

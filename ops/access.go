package ops

import (
	"context"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hanzoai/k8s/plane"
)

// can asks a cluster whether this app's own credential may perform one act.
//
// It is a SelfSubjectAccessReview, which means the answer comes from the cluster's
// RBAC rather than from a grant this app asserts about itself. That distinction is the
// reason the op exists: the union ClusterRole this deployment needs is granted OUT of
// tree, so a drifted or half-applied grant is a real and undetectable condition — and
// asking beats asserting, because a refusal a caller can read is a feature it can
// degrade gracefully on instead of a 403 arriving in the middle of a mutation.
//
// Example: {"cluster":"hanzo-k8s","verb":"get","resource":"resourcequotas","namespace":"acme-prod"}
// Response: {"allowed":true}
func (o Ops) can(ctx context.Context, in *plane.CanIn) (*plane.Can, error) {
	if err := need("verb", in.Verb); err != nil {
		return nil, err
	}
	if err := need("resource", in.Resource); err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	// A namespace, when given, is bounded like any other: asking about a namespace a
	// registration may not address would let a tenant probe another tenant's RBAC.
	ns := ""
	if in.Namespace != "" {
		ns, err = b.One(in.Namespace)
		if err != nil {
			return nil, fail(err)
		}
	} else if err := b.WholeCluster(); err != nil {
		return nil, fail(err)
	}
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Verb: in.Verb, Group: in.Group, Resource: in.Resource, Namespace: ns,
			},
		},
	}
	got, err := b.Typed.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return nil, fail(err)
	}
	return &plane.Can{Allowed: got.Status.Allowed, Reason: got.Status.Reason}, nil
}

package ops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/plane"
)

// kinds is the CLOSED set of custom resources this app addresses.
//
// This table is the whole difference between a bounded surface and a passthrough.
// The function it replaces decoded arbitrary YAML, resolved its kind through a
// discovery RESTMapper and create-or-updated ANY kind the cluster served — so one
// edited database row reached any object on any cluster, and no ClusterRole could
// be written that was smaller than "everything". Here the kind is a key in a map
// that only a code change can grow, which is what makes the union of privilege in
// LLM.md a finite list a reviewer can read.
//
// A group is never taken from a caller. `LuxNetwork` is the clearest case: cloud's
// validator path made the group configurable and then had to add a runtime guard
// refusing the two legacy groups that reconcile against a live hand-managed
// StatefulSet. Pinning the group here means those groups cannot be named at all,
// so the guard is a property of the table rather than a check that could be
// deleted.
//
// Every kind here is namespaced. That is not an assumption: it is why there is no
// cluster-scoped branch below, and adding a cluster-scoped kind requires adding
// one — deliberately, because a cluster-scoped write is a different blast radius.
var kinds = map[string]schema.GroupVersionResource{
	// The operator's App CR — the ONE workload kind the fleet runs on.
	"App": {Group: "hanzo.ai", Version: "v1", Resource: "apps"},
	// Hanzo CD's own record of a tracked git source: the revision it last applied,
	// its sync verdict, its deploy history. A different fact from an App.
	"Application": {Group: "apps.hanzo.ai", Version: "v1alpha1", Resource: "applications"},
	// The GitOps plane's project grouping.
	"AppProject": {Group: "argoproj.io", Version: "v1alpha1", Resource: "appprojects"},
	// A dedicated datastore the provisioning path materialises.
	"Datastore": {Group: "hanzo.ai", Version: "v1", Resource: "datastores"},
	// Ingress routing and its middleware chain.
	"IngressRoute": {Group: "hanzo.ai", Version: "v1alpha1", Resource: "ingressroutes"},
	"Middleware":   {Group: "hanzo.ai", Version: "v1alpha1", Resource: "middlewares"},
	// The validator node CR, under the DEDICATED group. Never "lux.network" and
	// never "lux.cloud" — both reconcile against the live hand-managed StatefulSet.
	"LuxNetwork": {Group: "node.lux.cloud", Version: "v1", Resource: "luxnetworks"},
	// Training and serving.
	"TrainJob":         {Group: "trainer.kubeflow.org", Version: "v1alpha1", Resource: "trainjobs"},
	"InferenceService": {Group: "serving.kserve.io", Version: "v1beta1", Resource: "inferenceservices"},
	// The KMS→Secret bridge. This is how material reaches a namespace: a KMSSecret
	// names a KMS ref and the kms-operator resolves it into a mounted Secret inside
	// the cluster. This app therefore never reads or writes a core Secret — there
	// is no op that does, because an RPC that returns a tenant's Secret is a
	// credential-exfiltration endpoint however carefully it is gated.
	"KMSSecret": {Group: "secrets.lux.network", Version: "v1alpha1", Resource: "kmssecrets"},
}

// maxWatch bounds one watch call. A caller cannot pin a connection open past this,
// and the bound is this app's rather than the caller's for exactly that reason.
const maxWatch = 55 * time.Second

// Kinds returns the registered kind names, sorted. Exported so a gate can assert the
// set is what the documentation says it is.
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// gvr resolves a kind name against the closed table.
func gvr(kind string) (schema.GroupVersionResource, error) {
	g, ok := kinds[strings.TrimSpace(kind)]
	if !ok {
		return schema.GroupVersionResource{}, zip.ErrBadRequest(fmt.Sprintf(
			"kind %q is not one this API addresses; the registered kinds are %s",
			kind, strings.Join(Kinds(), ", ")))
	}
	return g, nil
}

// listResources returns custom resources of one registered kind.
//
// Example: {"cluster":"hanzo-k8s","kind":"App","namespace":"acme-prod"}
// Response: {"resources":[{"kind":"App","namespace":"acme-prod","name":"api","spec":{"image":"registry.hanzo.ai/acme/api:1.4.2"},"status":{"phase":"Running"},"version":"918273"}],"version":"918273"}
func (o Ops) listResources(ctx context.Context, in *plane.ResourceIn) (*plane.Resources, error) {
	g, err := gvr(in.Kind)
	if err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := &plane.Resources{Resources: []plane.Resource{}}
	for _, ns := range scope {
		list, err := b.Dyn.Resource(g).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out.Resources = append(out.Resources, resourceView(in.Kind, list.Items[i]))
		}
		// The list's own resourceVersion is the cursor a watch resumes from. With a
		// multi-namespace scope the last one wins, which is why a watch asks for a
		// single namespace rather than inheriting a scope's cursor.
		out.Version = list.GetResourceVersion()
	}
	return out, nil
}

// getResource returns one custom resource.
//
// Response: {"kind":"App","namespace":"acme-prod","name":"api","spec":{"image":"registry.hanzo.ai/acme/api:1.4.2"},"status":{"phase":"Running"},"version":"918273"}
func (o Ops) getResource(ctx context.Context, in *plane.ResourceRef) (*plane.Resource, error) {
	g, err := gvr(in.Kind)
	if err != nil {
		return nil, err
	}
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	got, err := b.Dyn.Resource(g).Namespace(ns).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fail(err)
	}
	v := resourceView(in.Kind, *got)
	return &v, nil
}

// watchResources returns what changed to one kind since a cursor.
//
// It is a BOUNDED read rather than a stream, and that is a design decision worth
// stating: a stream cannot cross the call plane as a value, and rendering one —
// Server-Sent Events, a websocket — is the EDGE's concern. A consumer builds its own
// stream by calling this in a loop with the cursor it got back, so the streaming
// shape exists once, in the consumer that renders it, instead of once per transport.
//
// `expired` is the field that matters most. When the cursor is older than the
// apiserver's history the watch cannot be resumed, and an empty page would look
// exactly like "nothing changed" — which is how a watch-backed board silently stops
// updating for hours. On `expired` a caller must LIST again and take the new cursor.
//
// Example: {"cluster":"hanzo-k8s","kind":"App","namespace":"acme-prod","version":"918273","waitSeconds":30}
// Response: {"changes":[{"type":"MODIFIED","resource":{"kind":"App","namespace":"acme-prod","name":"api","status":{"phase":"Running"},"version":"918280"}}],"version":"918280"}
func (o Ops) watchResources(ctx context.Context, in *plane.WatchIn) (*plane.Changes, error) {
	g, err := gvr(in.Kind)
	if err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	if len(scope) != 1 {
		return nil, zip.ErrBadRequest("watch takes one namespace: a cursor belongs to one watch, so a multi-namespace watch would return a cursor that resumes none of them")
	}
	wait := maxWatch
	if in.WaitSeconds > 0 && time.Duration(in.WaitSeconds)*time.Second < maxWatch {
		wait = time.Duration(in.WaitSeconds) * time.Second
	}
	secs := int64(wait / time.Second)
	w, err := b.Dyn.Resource(g).Namespace(scope[0]).Watch(ctx, metav1.ListOptions{
		ResourceVersion: in.Version,
		TimeoutSeconds:  &secs,
	})
	if err != nil {
		if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
			return &plane.Changes{Changes: []plane.Change{}, Expired: true}, nil
		}
		return nil, fail(err)
	}
	defer w.Stop()
	out := &plane.Changes{Changes: []plane.Change{}, Version: in.Version}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return out, nil
		case <-deadline.C:
			return out, nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return out, nil
			}
			if ev.Type == watch.Error {
				// A watch error arrives as an event, not as a call failure. A too-old
				// cursor is the one a caller must act on differently, so it is a bit
				// rather than a message they would have to match on.
				if s, isStatus := ev.Object.(*metav1.Status); isStatus && (s.Reason == metav1.StatusReasonExpired || s.Reason == metav1.StatusReasonGone) {
					out.Expired = true
				}
				return out, nil
			}
			u, isU := ev.Object.(*unstructured.Unstructured)
			if !isU {
				continue
			}
			r := resourceView(in.Kind, *u)
			out.Changes = append(out.Changes, plane.Change{Type: string(ev.Type), Resource: r})
			out.Version = r.Version
		}
	}
}

// applyResource creates one custom resource of a registered kind, or patches the one
// already there.
//
// Five separate get-then-create-else-patch ladders in the fleet collapse into this one
// name. Each of those wrote the ladder itself and each was a race; here the apiserver
// resolves the race, because a create that loses falls through to the patch.
//
// The `spec` is the caller's domain payload and is applied whole — its SHAPE belongs to
// whoever authored the CRD, not to this app, which would otherwise have to be edited
// every time any kind grew a field. What this app owns is the KIND and the VERB, and
// both are named and typed. That is the line: `kind` is a key in a closed table, so
// this cannot become "apply any object the cluster has".
//
// Example: {"cluster":"hanzo-k8s","kind":"App","namespace":"acme-prod","name":"api","spec":{"image":"registry.hanzo.ai/acme/api:1.4.2","replicas":3}}
// Response: {"kind":"App","namespace":"acme-prod","name":"api","spec":{"image":"registry.hanzo.ai/acme/api:1.4.2","replicas":3},"version":"918281"}
func (o Ops) applyResource(ctx context.Context, in *plane.ApplyIn) (*plane.Resource, error) {
	g, err := gvr(in.Kind)
	if err != nil {
		return nil, err
	}
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	if in.Spec == nil {
		return nil, zip.ErrBadRequest("'spec' is required")
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	labels := map[string]any{"managed-by": managedBy}
	for k, v := range in.Labels {
		labels[k] = v
	}
	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion(g),
		"kind":       in.Kind,
		"metadata": map[string]any{
			"name": in.Name, "namespace": ns, "labels": labels,
		},
		"spec": in.Spec,
	}}
	res := b.Dyn.Resource(g).Namespace(ns)
	existing, err := res.Get(ctx, in.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		created, cerr := res.Create(ctx, desired, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(cerr) {
			// Lost the create race. The other writer's object is now the one to
			// update, so read it and fall through rather than reporting a conflict
			// the caller can do nothing useful with.
			existing, err = res.Get(ctx, in.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fail(err)
			}
		} else if cerr != nil {
			return nil, fail(cerr)
		} else {
			v := resourceView(in.Kind, *created)
			return &v, nil
		}
	case err != nil:
		return nil, fail(err)
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	updated, err := res.Update(ctx, desired, metav1.UpdateOptions{})
	if err != nil {
		return nil, fail(err)
	}
	v := resourceView(in.Kind, *updated)
	return &v, nil
}

// deleteResource deletes one custom resource of a registered kind.
//
// Deleting an object is not deleting its data: a workload's App CR is desired state and
// can be recreated freely, while the PersistentVolumeClaim it mounted holds the only
// copy of the tenant's data and is a different lifetime. Nothing here deletes a claim.
//
// Response: {"removed":true}
func (o Ops) deleteResource(ctx context.Context, in *plane.ResourceRef) (*plane.Deleted, error) {
	g, err := gvr(in.Kind)
	if err != nil {
		return nil, err
	}
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	err = b.Dyn.Resource(g).Namespace(ns).Delete(ctx, in.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return &plane.Deleted{}, nil
	}
	if err != nil {
		return nil, fail(err)
	}
	return &plane.Deleted{Removed: true}, nil
}

func apiVersion(g schema.GroupVersionResource) string {
	if g.Group == "" {
		return g.Version
	}
	return g.Group + "/" + g.Version
}

func resourceView(kind string, u unstructured.Unstructured) plane.Resource {
	r := plane.Resource{
		Kind: kind, Namespace: u.GetNamespace(), Name: u.GetName(),
		Labels: u.GetLabels(), Version: u.GetResourceVersion(),
	}
	if spec, found, _ := unstructured.NestedMap(u.Object, "spec"); found {
		r.Spec = spec
	}
	if status, found, _ := unstructured.NestedMap(u.Object, "status"); found {
		r.Status = status
	}
	return r
}

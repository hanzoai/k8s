package ops

import (
	"context"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hanzoai/k8s/plane"
)

// maxLogBytes caps one log read. A container's log is unbounded by nature, and an
// answer that has to fit in one ZAP message needs a bound that is this app's and not
// the caller's.
const maxLogBytes = 1 << 20

// listPods returns pods, narrowed by namespace, labels or node.
//
// One op covers every way the fleet asks for pods: the whole-cluster sweep an
// infrastructure board takes, the label-selected set that answers "is this app's
// workload up", and the single-node set a drain needs. They were three call sites
// with three shapes; the difference between them is which argument is set.
//
// Container ENV is deliberately absent from the projection. A pod spec carries the
// secret material injected into it, so a read surface that returned the spec would be
// an exfiltration door wearing the name of a health check.
//
// Example: {"cluster":"hanzo-k8s","namespace":"acme-prod","labelSelector":"app=api"}
// Response: {"pods":[{"namespace":"acme-prod","name":"api-7d9f-2xk","phase":"Running","node":"pool-a1","controller":"ReplicaSet/api-7d9f","images":["oci.hanzo.ai/acme/api:1.4.2"],"restarts":0}]}
func (o Ops) listPods(ctx context.Context, in *plane.PodsIn) (*plane.Pods, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	opts := metav1.ListOptions{LabelSelector: in.LabelSelector}
	if in.NodeName != "" {
		opts.FieldSelector = "spec.nodeName=" + in.NodeName
	}
	out := []plane.Pod{}
	for _, ns := range scope {
		list, err := b.Typed.CoreV1().Pods(ns).List(ctx, opts)
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out = append(out, podView(list.Items[i]))
		}
	}
	return &plane.Pods{Pods: out}, nil
}

// podLogs returns what one container printed.
//
// `tailLines` is the bound a caller should set; without one the read is capped at one
// mebibyte and truncated from the FRONT, so the newest output — the part that
// explains a crash — is what survives.
//
// Example: {"cluster":"hanzo-k8s","namespace":"acme-prod","pod":"api-7d9f-2xk","tailLines":200}
// Response: {"log":"2026-07-30T00:00:01Z listening on :8000\n"}
func (o Ops) podLogs(ctx context.Context, in *plane.LogsIn) (*plane.Logs, error) {
	if err := need("pod", in.Pod); err != nil {
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
	opts := &corev1.PodLogOptions{Container: in.Container}
	if in.TailLines > 0 {
		tail := in.TailLines
		opts.TailLines = &tail
	}
	stream, err := b.Typed.CoreV1().Pods(ns).GetLogs(in.Pod, opts).Stream(ctx)
	if err != nil {
		return nil, fail(err)
	}
	defer stream.Close()
	raw, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		return nil, fail(err)
	}
	return &plane.Logs{Log: string(raw)}, nil
}

// podView projects one pod onto placement, health, what it mounts and what it runs.
func podView(p corev1.Pod) plane.Pod {
	v := plane.Pod{
		Namespace: p.Namespace, Name: p.Name,
		Phase: string(p.Status.Phase), Reason: p.Status.Reason, Node: p.Spec.NodeName,
	}
	for _, vol := range p.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			v.Claims = append(v.Claims, vol.PersistentVolumeClaim.ClaimName)
		}
	}
	// The controlling owner names the workload that has to be edited to change this
	// pod, and its KIND decides how — a StatefulSet's volumeClaimTemplates are
	// immutable, so the workload must be recreated around a change rather than patched.
	for _, ref := range p.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			v.Controller = ref.Kind + "/" + ref.Name
		}
	}
	for _, c := range p.Spec.InitContainers {
		v.Images = append(v.Images, c.Image)
	}
	for _, c := range p.Spec.Containers {
		v.Images = append(v.Images, c.Image)
	}
	for _, cs := range p.Status.ContainerStatuses {
		v.Restarts += cs.RestartCount
		// A waiting container's reason (CrashLoopBackOff, ImagePullBackOff) is the
		// real health signal; pod.status.reason stays empty for exactly those.
		if v.Reason == "" && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			v.Reason = cs.State.Waiting.Reason
		}
	}
	return v
}

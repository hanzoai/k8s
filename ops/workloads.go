package ops

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hanzoai/k8s/plane"
)

// lbIDAnnotation is the annotation a cloud-controller stamps on a Service once it has
// provisioned a load balancer for it — the strongest link between the two, and what
// lets a reconciliation tell an orphaned balancer from a live one.
const lbIDAnnotation = "kubernetes.digitalocean.com/load-balancer-id"

// listDeployments returns Deployments with their live replica counts and the images
// they are ACTUALLY running.
//
// The running image is why this op exists rather than a read of the App CR: a CR's
// status does not surface the tag the pods are on, so the live Deployment is the only
// place "declared v1.4.2, running v1.4.1" can be seen at all — which is the whole of
// drift detection.
//
// Example: {"cluster":"hanzo-k8s","namespace":"acme-prod"}
// Response: {"workloads":[{"namespace":"acme-prod","name":"api","kind":"Deployment","replicas":3,"ready":3,"updated":3,"images":["oci.hanzo.ai/acme/api:1.4.2"]}]}
func (o Ops) listDeployments(ctx context.Context, in *plane.Selector) (*plane.Workloads, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Workload{}
	for _, ns := range scope {
		list, err := b.Typed.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out = append(out, deploymentView(list.Items[i]))
		}
	}
	return &plane.Workloads{Workloads: out}, nil
}

// getDeployment returns one Deployment.
//
// Response: {"namespace":"acme-prod","name":"api","kind":"Deployment","replicas":3,"ready":3,"updated":3,"images":["oci.hanzo.ai/acme/api:1.4.2"]}
func (o Ops) getDeployment(ctx context.Context, in *plane.NamedIn) (*plane.Workload, error) {
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
	d, err := b.Typed.AppsV1().Deployments(ns).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fail(err)
	}
	v := deploymentView(*d)
	return &v, nil
}

// listReplicaSets returns ReplicaSets.
//
// A deploy tree needs them because they are the layer between a Deployment and its
// pods: during a rollout two exist, and which pods belong to which is the difference
// between "the new version is up" and "the old one still is".
//
// Response: {"workloads":[{"namespace":"acme-prod","name":"api-7d9f","kind":"ReplicaSet","replicas":3,"ready":3,"owner":"Deployment/api"}]}
func (o Ops) listReplicaSets(ctx context.Context, in *plane.Selector) (*plane.Workloads, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Workload{}
	for _, ns := range scope {
		list, err := b.Typed.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			r := list.Items[i]
			w := plane.Workload{
				Namespace: r.Namespace, Name: r.Name, Kind: "ReplicaSet",
				Ready: r.Status.ReadyReplicas, Owner: controllerOf(r.OwnerReferences),
			}
			if r.Spec.Replicas != nil {
				w.Replicas = *r.Spec.Replicas
			}
			w.Images = images(r.Spec.Template.Spec)
			out = append(out, w)
		}
	}
	return &plane.Workloads{Workloads: out}, nil
}

// listStatefulSets returns StatefulSets.
//
// They are read separately from Deployments because they are a different lifetime: a
// StatefulSet's volumeClaimTemplates are immutable, so a storage change means
// recreating the workload around the claims rather than patching it — and a board that
// folded the two kinds together would offer an operation that cannot work.
//
// Response: {"workloads":[{"namespace":"acme-prod","name":"pg","kind":"StatefulSet","replicas":3,"ready":3,"images":["postgres:17"]}]}
func (o Ops) listStatefulSets(ctx context.Context, in *plane.Selector) (*plane.Workloads, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Workload{}
	for _, ns := range scope {
		list, err := b.Typed.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			s := list.Items[i]
			w := plane.Workload{
				Namespace: s.Namespace, Name: s.Name, Kind: "StatefulSet",
				Ready: s.Status.ReadyReplicas, Updated: s.Status.UpdatedReplicas,
				Owner: controllerOf(s.OwnerReferences), Images: images(s.Spec.Template.Spec),
			}
			if s.Spec.Replicas != nil {
				w.Replicas = *s.Spec.Replicas
			}
			out = append(out, w)
		}
	}
	return &plane.Workloads{Workloads: out}, nil
}

// listServices returns Services with every identity by which one can be matched to the
// load balancer in front of it.
//
// Both the cloud-controller's own annotation and the assigned addresses travel, and
// either match is enough. That breadth is the safe direction: matching MORE Services to
// a balancer means fewer balancers look orphaned, and the consequence of a wrong match
// in the other direction is deleting a live one.
//
// Response: {"services":[{"namespace":"acme-prod","name":"api","type":"LoadBalancer","clusterIp":"10.3.0.7","ips":["203.0.113.10"],"lbId":"a1b2","ports":[443]}]}
func (o Ops) listServices(ctx context.Context, in *plane.Selector) (*plane.Services, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Service{}
	for _, ns := range scope {
		list, err := b.Typed.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out = append(out, serviceView(list.Items[i]))
		}
	}
	return &plane.Services{Services: out}, nil
}

// listIngresses returns Ingresses with the hosts they answer and which of those are
// covered by TLS.
//
// The two host lists are separate because a host in `hosts` but not in `tlsHosts` is a
// public URL served over plain HTTP, which is a finding rather than a detail.
//
// Response: {"ingresses":[{"namespace":"acme-prod","name":"api","hosts":["api.acme.com"],"tlsHosts":["api.acme.com"],"addresses":["203.0.113.10"]}]}
func (o Ops) listIngresses(ctx context.Context, in *plane.Selector) (*plane.Ingresses, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Ingress{}
	for _, ns := range scope {
		list, err := b.Typed.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out = append(out, ingressView(list.Items[i]))
		}
	}
	return &plane.Ingresses{Ingresses: out}, nil
}

// listEvents returns cluster events, oldest first.
//
// This is the only place a failure with no pod behind it can be explained — an
// unschedulable workload, a quota refusal, a volume that never bound — because those
// produce an event and nothing else. Events are ordered by last-seen so the tail is
// the current situation.
//
// Response: {"events":[{"namespace":"acme-prod","name":"api.17f","type":"Warning","reason":"FailedScheduling","message":"0/6 nodes are available: insufficient nvidia.com/gpu","object":"Pod/api-7d9f-2xk","count":4,"lastSeen":1785110400}]}
func (o Ops) listEvents(ctx context.Context, in *plane.Selector) (*plane.Events, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Event{}
	for _, ns := range scope {
		list, err := b.Typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out = append(out, eventView(list.Items[i]))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen < out[j].LastSeen })
	return &plane.Events{Events: out}, nil
}

// listConfigMaps returns ConfigMaps, with their data.
//
// The data travels because a ConfigMap is by definition not secret material. Secrets
// are a different kind and this API does not read one back, at all: an RPC that returns
// a tenant's Secret is a credential-exfiltration endpoint however carefully it is
// gated, and material that needs protecting belongs in KMS.
//
// Response: {"configMaps":[{"namespace":"hanzo","name":"cron-schedules","data":{"nightly":"0 3 * * *"}}]}
func (o Ops) listConfigMaps(ctx context.Context, in *plane.Selector) (*plane.ConfigMaps, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.ConfigMap{}
	for _, ns := range scope {
		list, err := b.Typed.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			c := list.Items[i]
			out = append(out, plane.ConfigMap{Namespace: c.Namespace, Name: c.Name, Data: c.Data})
		}
	}
	return &plane.ConfigMaps{ConfigMaps: out}, nil
}

func deploymentView(d appsv1.Deployment) plane.Workload {
	w := plane.Workload{
		Namespace: d.Namespace, Name: d.Name, Kind: "Deployment",
		Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas,
		Owner: controllerOf(d.OwnerReferences), Images: images(d.Spec.Template.Spec),
	}
	if d.Spec.Replicas != nil {
		w.Replicas = *d.Spec.Replicas
	}
	return w
}

func serviceView(s corev1.Service) plane.Service {
	v := plane.Service{
		Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
		ClusterIP: s.Spec.ClusterIP,
		LBID:      strings.TrimSpace(s.Annotations[lbIDAnnotation]),
	}
	if ip := strings.TrimSpace(s.Spec.LoadBalancerIP); ip != "" {
		v.IPs = append(v.IPs, ip)
	}
	for _, in := range s.Status.LoadBalancer.Ingress {
		if ip := strings.TrimSpace(in.IP); ip != "" {
			v.IPs = append(v.IPs, ip)
		}
	}
	for _, p := range s.Spec.Ports {
		v.Ports = append(v.Ports, p.Port)
	}
	return v
}

func ingressView(i networkingv1.Ingress) plane.Ingress {
	v := plane.Ingress{Namespace: i.Namespace, Name: i.Name}
	for _, r := range i.Spec.Rules {
		if r.Host != "" {
			v.Hosts = append(v.Hosts, r.Host)
		}
	}
	for _, t := range i.Spec.TLS {
		v.TLSHosts = append(v.TLSHosts, t.Hosts...)
	}
	for _, lb := range i.Status.LoadBalancer.Ingress {
		switch {
		case lb.IP != "":
			v.Addresses = append(v.Addresses, lb.IP)
		case lb.Hostname != "":
			v.Addresses = append(v.Addresses, lb.Hostname)
		}
	}
	return v
}

func eventView(e corev1.Event) plane.Event {
	v := plane.Event{
		Namespace: e.Namespace, Name: e.Name, Type: e.Type,
		Reason: e.Reason, Message: e.Message, Count: e.Count,
	}
	if e.InvolvedObject.Kind != "" {
		v.Object = e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
	}
	// LastTimestamp is the field a legacy event carries; a modern one leaves it zero
	// and sets EventTime. Read both, or every event on a current cluster sorts to the
	// beginning of time.
	switch {
	case !e.LastTimestamp.IsZero():
		v.LastSeen = e.LastTimestamp.Unix()
	case !e.EventTime.IsZero():
		v.LastSeen = e.EventTime.Time.Unix()
	case !e.FirstTimestamp.IsZero():
		v.LastSeen = e.FirstTimestamp.Unix()
	}
	return v
}

func images(spec corev1.PodSpec) []string {
	out := make([]string, 0, len(spec.InitContainers)+len(spec.Containers))
	for _, c := range spec.InitContainers {
		out = append(out, c.Image)
	}
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return out
}

func controllerOf(refs []metav1.OwnerReference) string {
	for _, r := range refs {
		if r.Controller != nil && *r.Controller {
			return r.Kind + "/" + r.Name
		}
	}
	return ""
}

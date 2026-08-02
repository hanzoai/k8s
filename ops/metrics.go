package ops

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	"github.com/hanzoai/k8s/plane"
)

// podMetrics returns live CPU and memory use for pods, from metrics.k8s.io.
//
// `available` is the field that carries the honesty. metrics-server is absent from most
// of our clusters, and a cluster without it answers this call with an error — which,
// rendered as zeroes, tells an operator that a busy namespace is idle. So a missing
// metrics API is reported as available=false with no rows, and a caller must render
// "unmeasured" rather than "0m".
//
// Example: {"cluster":"hanzo-k8s","namespace":"acme-prod"}
// Response: {"pods":[{"namespace":"acme-prod","name":"api-7d9f-2xk","cpuMilli":250,"memoryBytes":536870912}],"available":true}
func (o Ops) podMetrics(ctx context.Context, in *plane.Selector) (*plane.PodMetrics, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := &plane.PodMetrics{Pods: []plane.PodMetric{}}
	for _, ns := range scope {
		list, err := b.Metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			// One namespace failing on a cluster that HAS metrics is a real error and
			// must surface; a cluster with no metrics API at all fails every namespace
			// the same way, and the caller needs that distinction. The apiserver
			// answers a missing aggregated API with NotFound or ServiceUnavailable,
			// so those two are the "no metrics here" answer and nothing else is.
			if noMetrics(err) {
				return out, nil
			}
			return nil, fail(err)
		}
		for i := range list.Items {
			m := list.Items[i]
			row := plane.PodMetric{Namespace: m.Namespace, Name: m.Name}
			for _, c := range m.Containers {
				row.CPUMilli += c.Usage.Cpu().MilliValue()
				row.MemoryBytes += c.Usage.Memory().Value()
			}
			out.Pods = append(out.Pods, row)
		}
	}
	out.Available = true
	return out, nil
}

// noMetrics reports whether an error means the cluster serves no metrics API, as
// distinct from a metrics API that refused this read.
func noMetrics(err error) bool {
	return apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) ||
		meta.IsNoMatchError(err) || discovery.IsGroupDiscoveryFailedError(err)
}

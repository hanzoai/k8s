package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/hanzoai/k8s/plane"
	"github.com/hanzoai/k8s/registry"
)

// kubeletFanout bounds the per-cluster kubelet sweep. Each read is one small
// document through the apiserver's node proxy, so this bounds pressure on the
// apiserver rather than local work.
const kubeletFanout = 16

// listNodes returns a cluster's worker nodes: whether each is up, whether it will
// take new work, and what accelerators it offers.
//
// This is the one node inventory in the fleet. The GPU counts come from each node's
// allocatable `nvidia.com/gpu` and `amd.com/gpu`, which is what the scheduler will
// actually honour — not a label somebody typed.
//
// Nodes are cluster-scoped, so a registration bounded to a namespace set cannot read
// them: answering would hand one tenant on a shared cluster the whole cluster's
// capacity.
//
// Response: {"nodes":[{"name":"pool-gpu-a1","ready":true,"schedulable":true,"region":"nyc3","instanceKind":"gpu-h100x8-640gb","cpu":"64","memory":"755Gi","nvidiaGpu":8,"amdGpu":0,"internalIp":"10.0.0.4"}]}
func (o Ops) listNodes(ctx context.Context, in *plane.ClusterRef) (*plane.Nodes, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	if err := b.WholeCluster(); err != nil {
		return nil, fail(err)
	}
	list, err := b.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fail(err)
	}
	out := make([]plane.Node, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, nodeView(list.Items[i]))
	}
	return &plane.Nodes{Nodes: out}, nil
}

// cordonNode takes a node out of service, or puts it back, and optionally drains it.
//
// This is ONE named operation over what is really three calls — patch
// `spec.unschedulable`, list the node's pods, evict each one — because the three are
// only ever wanted together and a generic "patch a node" op would let a caller write
// any field of any node's spec. There is deliberately no such op.
//
// The drain uses the EVICTION api, not delete: an eviction respects
// PodDisruptionBudgets, so a drain that would break a quorum is REFUSED by the
// apiserver and reported verbatim, with the node left cordoned so an operator can
// scale and retry. DaemonSet and static (mirror) pods are skipped — the node
// recreates them immediately, so evicting them is a loop — and pods already
// terminal are skipped too.
//
// `drain` is ignored when `schedulable` is true, because draining a node you just
// returned to service is incoherent rather than merely useless.
//
// Example: {"cluster":"hanzo-k8s","node":"pool-gpu-a1","schedulable":false,"drain":true}
// Response: {"evicted":11}
func (o Ops) cordonNode(ctx context.Context, in *plane.CordonIn) (*plane.Cordoned, error) {
	if err := need("node", in.Node); err != nil {
		return nil, err
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	if err := b.WholeCluster(); err != nil {
		return nil, fail(err)
	}
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, !in.Schedulable)
	if _, err := b.Typed.CoreV1().Nodes().Patch(
		ctx, in.Node, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return nil, fail(err)
	}
	if in.Schedulable || !in.Drain {
		return &plane.Cordoned{}, nil
	}
	pods, err := b.Typed.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + in.Node,
	})
	if err != nil {
		return nil, fail(err)
	}
	evicted := 0
	for i := range pods.Items {
		p := pods.Items[i]
		if skipEviction(p) {
			continue
		}
		ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name}}
		if err := b.Typed.CoreV1().Pods(p.Namespace).EvictV1(ctx, ev); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			// Report the count achieved so far alongside the refusal: an operator
			// needs to know how far the drain got, not just that it stopped.
			return &plane.Cordoned{Evicted: evicted}, fail(fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err))
		}
		evicted++
	}
	return &plane.Cordoned{Evicted: evicted}, nil
}

// nodeVolumeStats reports how full every mounted volume on a cluster is.
//
// It is the ONLY source of volume fill we have. Cloud providers do not expose how
// full a disk is, and metrics-server is absent from most of our clusters — but every
// kubelet serves `stats/summary` unconditionally, so this works everywhere. The read
// goes through the apiserver's node proxy, which is why this is a SEPARATE named op:
// `nodes/proxy get` is effectively arbitrary kubelet access, and naming it here is
// what lets every other operation's grant omit it.
//
// A kubelet that will not answer costs a reading and never the call: fill is
// advisory, no safety verdict depends on it, and failing the whole sweep over one
// silent node would block every volume decision on the fleet. `nodesRead` against
// `nodesTotal` is how a caller knows the difference between "empty" and
// "unmeasured" — a claim with no row is unmeasured, and treating that as empty is
// how live data gets condemned.
//
// Response: {"volumes":[{"namespace":"acme","name":"data","usedBytes":48318382080}],"nodesRead":6,"nodesTotal":6}
func (o Ops) nodeVolumeStats(ctx context.Context, in *plane.ClusterRef) (*plane.VolumeStats, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	if err := b.WholeCluster(); err != nil {
		return nil, fail(err)
	}
	nodes, err := b.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fail(err)
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		used = map[[2]string]int64{}
		read int
		sem  = make(chan struct{}, kubeletFanout)
	)
	for i := range nodes.Items {
		node := nodes.Items[i].Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			raw, err := b.Typed.CoreV1().RESTClient().Get().
				Resource("nodes").Name(node).SubResource("proxy").
				Suffix("stats", "summary").DoRaw(ctx)
			if err != nil {
				return
			}
			var sum struct {
				Pods []struct {
					Volume []struct {
						PVCRef *struct {
							Namespace string `json:"namespace"`
							Name      string `json:"name"`
						} `json:"pvcRef"`
						UsedBytes int64 `json:"usedBytes"`
					} `json:"volume"`
				} `json:"pods"`
			}
			if json.Unmarshal(raw, &sum) != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			read++
			for _, p := range sum.Pods {
				for _, v := range p.Volume {
					if v.PVCRef == nil {
						continue
					}
					k := [2]string{v.PVCRef.Namespace, v.PVCRef.Name}
					// Two kubelets can report one claim — ReadWriteMany, or a
					// rollout with both pods briefly alive. Keep the LARGEST
					// reading: more used means less waste claimed, which is the
					// conservative direction for every decision downstream.
					if v.UsedBytes > used[k] {
						used[k] = v.UsedBytes
					}
				}
			}
		}()
	}
	wg.Wait()
	out := make([]plane.VolumeUse, 0, len(used))
	for k, bytes := range used {
		out = append(out, plane.VolumeUse{Namespace: k[0], Name: k[1], UsedBytes: bytes})
	}
	// Map order is not stable and this answer is: a reproducible sweep is worth one
	// sort, and a diff between two sweeps is then a real change.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return &plane.VolumeStats{Volumes: out, NodesRead: read, NodesTotal: len(nodes.Items)}, nil
}

// nodeView projects one node onto the fields the boards read.
func nodeView(n corev1.Node) plane.Node {
	v := plane.Node{
		Name:        n.Name,
		Ready:       nodeReady(n),
		Schedulable: !n.Spec.Unschedulable,
		Region:      n.Labels[corev1.LabelTopologyRegion],
		InstanceKind: n.Labels[corev1.LabelInstanceTypeStable],
	}
	if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
		v.CPU = q.String()
	}
	if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
		v.Memory = q.String()
	}
	if q, ok := n.Status.Allocatable["nvidia.com/gpu"]; ok {
		v.NvidiaGPU = registry.QuantityInt(q.String())
	}
	if q, ok := n.Status.Allocatable["amd.com/gpu"]; ok {
		v.AmdGPU = registry.QuantityInt(q.String())
	}
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			v.InternalIP = a.Address
			break
		}
	}
	return v
}

// nodeReady reads the one condition that decides whether a node is up.
func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// skipEviction reports pods a drain must leave alone: DaemonSet-owned and static
// (mirror) pods, which the node recreates immediately, and pods already terminal.
func skipEviction(p corev1.Pod) bool {
	if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return true
	}
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

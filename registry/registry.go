// Package registry is the cluster registry — the core model of this app and the
// only door to a Kubernetes apiserver in this program.
//
// A cluster is a REGISTERED ENTITY: a name, a provider, an endpoint, a credential
// ref in KMS, and the org that owns it. Every operation NAMES its cluster, and
// the name is resolved through this registry against the tenant the call acts
// for. There is no ambient cluster and no in-cluster fallback: a function that
// could be called without saying which cluster would read whichever apiserver the
// process happened to be started next to, and that is precisely the assumption
// that cannot survive lux running lux-k8s while hanzo runs hanzo-k8s and a
// customer runs their own.
//
// # Multi-cluster is not a feature here, it is the shape
//
// [Registry.Reach] is the ONLY constructor of a Kubernetes client in this module,
// and it takes a name. That is the whole enforcement: no other function can
// obtain a clientset, so no other function can talk to a cluster it did not name.
// BYO-K8s, lux-k8s and hanzo-k8s are the same shape — three rows, one code path.
//
// # The credential never rests here
//
// A registration seals its kubeconfig in KMS under a ref DERIVED from the owning
// org and the cluster name, and stores no credential of its own. Reach resolves
// that ref at use, over the call plane, carrying the caller — so KMS re-enforces
// the tenant boundary independently of this package, and a registry row cannot
// name material it does not own because it does not name material at all. Nothing
// is written to disk, nothing is put in an environment variable, and nothing is
// logged.
//
// # Two independent gates, and neither is the apiserver's alone
//
//  1. The INDEX is per-org. A name is looked up only in the calling org's own
//     index, so a name another org registered is NOT FOUND — not forbidden, which
//     would confirm it exists.
//  2. The ROW bounds namespaces. A registration made with a scoped credential on a
//     shared cluster carries the namespace set it may address, and [Bound.Scope]
//     refuses anything outside it before the apiserver is dialed. The credential's
//     own RBAC refuses it a second time.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/hanzoai/k8s/plane"
)

// probeTimeout bounds the one read a registration makes to prove a cluster is
// reachable with the credential it was handed. A cluster that cannot answer a
// node list in this long is not registered — failing at attach time is the only
// place the operator is still holding the kubeconfig and can fix it.
const probeTimeout = 15 * time.Second

// MaxNameLen bounds a cluster name. It is a DNS label bound because the name
// becomes a KMS ref segment and appears in log lines; an unbounded name is a
// store key an attacker chooses the size of.
const MaxNameLen = 63

// nodesGVR is the one coordinate this package reads for itself: a registration
// proves reachability by counting nodes and accelerators, which is also the
// inventory every fleet board wants.
var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// Cluster is a registered cluster as the index holds it.
//
// There is no Kubeconfig field and no Credential field, deliberately. The
// material is in KMS and its ref is DERIVED from Org and Name (see credRef) —
// storing the ref as data would let one edited row name another org's material,
// which is the same defect as letting a caller name its own org.
type Cluster struct {
	Name string `json:"name"`
	// Org is the org that owns this registration. A property of the record, set
	// from the validated caller at register time and never from an argument.
	Org        string   `json:"org"`
	Provider   string   `json:"provider"`
	Endpoint   string   `json:"endpoint,omitempty"`
	Nodes      int      `json:"nodes"`
	NvidiaGPU  int      `json:"nvidiaGpu"`
	AmdGPU     int      `json:"amdGpu"`
	Namespaces []string `json:"namespaces,omitempty"`
	Registered string   `json:"registered"`
	Default    bool     `json:"default"`
}

// View projects a record onto the wire type. It exists so the plane contract and
// the stored record can differ without either leaking into the other.
func (c Cluster) View() plane.Registered {
	return plane.Registered{
		Name: c.Name, Org: c.Org, Provider: c.Provider, Endpoint: c.Endpoint,
		Nodes: c.Nodes, NvidiaGPU: c.NvidiaGPU, AmdGPU: c.AmdGPU,
		Namespaces: c.Namespaces, Registered: c.Registered, Default: c.Default,
	}
}

// Store is where sealed material and the index live. It is an interface for ONE
// reason: a test must be able to exercise the tenancy guards without a KMS, and
// the guards are the part that must never be mocked away.
//
// The production implementation is the KMS peer over the call plane ([Sealed]).
type Store interface {
	// Get returns the value at ref, or (nil, nil) when there is none. An absent
	// value and an unreachable store are DIFFERENT answers: the first is empty,
	// the second is an error, and collapsing them would make an outage look like
	// an org with no clusters.
	Get(ctx context.Context, ref string) ([]byte, error)
	// Put writes value at ref.
	Put(ctx context.Context, ref string, value []byte) error
}

// Registry is the per-deployment cluster registry.
type Registry struct {
	store Store

	mu     sync.Mutex
	bound  map[string]*Bound // "org/name" -> live clients
}

// New opens a registry over a store.
func New(store Store) *Registry {
	return &Registry{store: store, bound: map[string]*Bound{}}
}

// Bound is one cluster, resolved for one tenant: the record, the credential's
// clients, and the namespace bound the record carries.
//
// It is the only way to reach an apiserver in this module, and it cannot be
// constructed without a cluster name and a validated tenant.
type Bound struct {
	Cluster Cluster
	Typed   kubernetes.Interface
	Dyn     dynamic.Interface
	Metrics metricsv.Interface
	Rest    *rest.Config
}

// Scope resolves the namespaces an op may read, given the namespace it asked for.
//
// This is the ONE place the namespace boundary is enforced, and every namespaced
// op goes through it:
//
//   - a named namespace inside the bound resolves to itself;
//   - a named namespace outside the bound is REFUSED (not silently narrowed — a
//     caller that asked for the wrong thing must be told, or it will believe an
//     empty answer);
//   - no namespace, on a bounded registration, resolves to the whole bound;
//   - no namespace, on an unbounded registration, resolves to [""] — every
//     namespace, which only an org that registered its OWN cluster ever gets.
func (b *Bound) Scope(ns string) ([]string, error) {
	ns = strings.TrimSpace(ns)
	bound := b.Cluster.Namespaces
	if len(bound) == 0 {
		if ns == "" {
			return []string{metav1.NamespaceAll}, nil
		}
		return []string{ns}, nil
	}
	if ns == "" {
		return append([]string(nil), bound...), nil
	}
	for _, allowed := range bound {
		if allowed == ns {
			return []string{ns}, nil
		}
	}
	return nil, ErrNamespace{Cluster: b.Cluster.Name, Namespace: ns}
}

// One resolves exactly one namespace for an op that mutates or addresses a single
// object. An unbounded registration takes the caller's word; a bounded one must
// have the namespace named and inside its bound, because "act on all of them" is
// not a thing a mutation may mean.
func (b *Bound) One(ns string) (string, error) {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return "", ErrNamespace{Cluster: b.Cluster.Name, Namespace: ""}
	}
	got, err := b.Scope(ns)
	if err != nil {
		return "", err
	}
	return got[0], nil
}

// WholeCluster refuses a registration that does not address the whole cluster.
//
// Cluster-scoped reads — PersistentVolumes, Nodes, a cordon — cannot be narrowed
// to a namespace, so a scoped registration must not be able to make them at all.
// Answering them for a bounded row would hand a tenant on a shared cluster the
// node inventory of every other tenant on it.
func (b *Bound) WholeCluster() error {
	if len(b.Cluster.Namespaces) == 0 {
		return nil
	}
	return ErrScope{Cluster: b.Cluster.Name}
}

// ErrNoTenant is the refusal when no validated tenant rode along with the call.
//
// It is the FULL rule, not a non-empty check: zip.Tenant requires a validated
// user claim beside the org, refuses an empty org and refuses one over
// zip.MaxOrgLen. An org that arrived without a validated principal is a claim the
// caller made about itself.
type ErrNoTenant struct{}

func (ErrNoTenant) Error() string {
	return "no validated tenant on this call: an org header without a validated principal is a claim, not an identity"
}

// ErrNoCluster is the refusal when a name is not in the calling org's registry.
//
// It says NOT FOUND rather than forbidden on purpose. A cluster another org
// registered must be indistinguishable from one that does not exist, or the
// refusal itself becomes an oracle for enumerating other tenants' cluster names.
type ErrNoCluster struct{ Name string }

func (e ErrNoCluster) Error() string {
	return fmt.Sprintf("no cluster named %q is registered for this org", e.Name)
}

// ErrNamespace is the refusal when a call names a namespace outside the bound its
// registration carries.
type ErrNamespace struct{ Cluster, Namespace string }

func (e ErrNamespace) Error() string {
	if e.Namespace == "" {
		return fmt.Sprintf("cluster %q requires a namespace: this registration is bounded to a namespace set", e.Cluster)
	}
	return fmt.Sprintf("namespace %q is outside what this org's registration of cluster %q may address", e.Namespace, e.Cluster)
}

// ErrScope is the refusal when a cluster-scoped read is asked of a registration
// bounded to namespaces.
type ErrScope struct{ Cluster string }

func (e ErrScope) Error() string {
	return fmt.Sprintf("cluster %q is registered to this org with a namespace bound, so cluster-scoped reads are not available", e.Cluster)
}

// ErrName is the refusal when a cluster name could not be a KMS ref segment.
type ErrName struct{ Name string }

func (e ErrName) Error() string {
	return fmt.Sprintf("cluster name %q is not usable: 1-%d characters of a-z, 0-9 and '-', starting and ending alphanumeric", e.Name, MaxNameLen)
}

// List returns every cluster the tenant on this call may reach.
func (r *Registry) List(ctx context.Context) ([]Cluster, error) {
	org, err := Tenant(ctx)
	if err != nil {
		return nil, err
	}
	return r.index(ctx, org)
}

// Get returns one record by name, or ErrNoCluster.
func (r *Registry) Get(ctx context.Context, name string) (Cluster, error) {
	org, err := Tenant(ctx)
	if err != nil {
		return Cluster{}, err
	}
	list, err := r.index(ctx, org)
	if err != nil {
		return Cluster{}, err
	}
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
	}
	return Cluster{}, ErrNoCluster{Name: name}
}

// Register attaches a cluster to the calling org's registry.
//
// The kubeconfig is validated by [SafeRESTConfig], USED to prove the cluster
// answers, sealed in KMS, and then dropped. Registration is idempotent on name
// within the org: re-registering re-seals the credential and refreshes the
// inventory, which is how a rotated credential lands without a second verb.
//
// A cluster is registered into the CALLER's own scope and no other. There is no
// op that registers a cluster for a different org, which is what makes
// cross-org reach impossible rather than merely checked: to reach a cluster an
// org must hold a credential for it, sealed under its own KMS ref, and KMS
// refuses a peer asking for another org's material.
func (r *Registry) Register(ctx context.Context, in *plane.RegisterIn) (Cluster, error) {
	org, err := Tenant(ctx)
	if err != nil {
		return Cluster{}, err
	}
	name := strings.TrimSpace(in.Name)
	if !ValidName(name) {
		return Cluster{}, ErrName{Name: in.Name}
	}
	kube := []byte(in.Kubeconfig)
	cfg, err := SafeRESTConfig(kube)
	if err != nil {
		return Cluster{}, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Cluster{}, fmt.Errorf("kubeconfig unusable: %w", err)
	}
	inv, err := inventory(ctx, dyn)
	if err != nil {
		return Cluster{}, fmt.Errorf("cluster unreachable with this credential: %w", err)
	}
	if err := r.store.Put(ctx, credRef(org, name), kube); err != nil {
		return Cluster{}, fmt.Errorf("seal credential: %w", err)
	}
	rec := Cluster{
		Name: name, Org: org, Provider: firstNonEmpty(in.Provider, "byo"),
		Endpoint: cfg.Host, Nodes: inv.nodes, NvidiaGPU: inv.nvidia, AmdGPU: inv.amd,
		Namespaces: cleanNamespaces(in.Namespaces),
		Registered: time.Now().UTC().Format(time.RFC3339), Default: in.Default,
	}
	list, err := r.index(ctx, org)
	if err != nil {
		return Cluster{}, err
	}
	if err := r.writeIndex(ctx, org, upsert(list, rec)); err != nil {
		return Cluster{}, err
	}
	r.forget(org, name)
	return rec, nil
}

// Deregister detaches a cluster from the calling org's registry and destroys the
// sealed credential by overwriting it.
//
// Overwrite rather than delete because the store's contract is get/put: a
// credential nothing can read is gone for every purpose this app has, and adding
// a third verb to reach the same end would be a second way to do one thing.
func (r *Registry) Deregister(ctx context.Context, name string) (bool, error) {
	org, err := Tenant(ctx)
	if err != nil {
		return false, err
	}
	list, err := r.index(ctx, org)
	if err != nil {
		return false, err
	}
	kept := make([]Cluster, 0, len(list))
	found := false
	for _, c := range list {
		if c.Name == name {
			found = true
			continue
		}
		kept = append(kept, c)
	}
	if !found {
		return false, nil
	}
	if err := r.writeIndex(ctx, org, kept); err != nil {
		return false, err
	}
	if err := r.store.Put(ctx, credRef(org, name), nil); err != nil {
		return false, fmt.Errorf("destroy credential: %w", err)
	}
	r.forget(org, name)
	return true, nil
}

// Reach resolves a named cluster for the tenant on this call and returns its
// clients. It is the ONE constructor of a Kubernetes client in this module.
func (r *Registry) Reach(ctx context.Context, name string) (*Bound, error) {
	org, err := Tenant(ctx)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrNoCluster{Name: name}
	}
	rec, err := r.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	key := org + "/" + name
	r.mu.Lock()
	b, ok := r.bound[key]
	r.mu.Unlock()
	if ok {
		// The record may have moved (a namespace bound tightened, an inventory
		// refreshed) while the clients stayed valid. Take the fresh record and
		// keep the connections: a stale BOUND is a stale authorization.
		b2 := *b
		b2.Cluster = rec
		return &b2, nil
	}
	raw, err := r.store.Get(ctx, credRef(org, name))
	if err != nil {
		return nil, fmt.Errorf("resolve credential for cluster %q: %w", name, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("cluster %q is registered but its credential is gone from KMS; re-register it", name)
	}
	cfg, err := SafeRESTConfig(raw)
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "hanzo-k8s"
	// client-go's default 5 QPS / burst 10 throttles a per-node fan-out badly: a
	// 17-node kubelet sweep spent over a second queued, and at 100 nodes the
	// queue alone approaches the read timeout. The read count is bounded by the
	// op, so the real concurrency bound belongs there, not here.
	cfg.QPS, cfg.Burst = 50, 100
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster %q: %w", name, err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster %q: %w", name, err)
	}
	mx, err := metricsv.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster %q: %w", name, err)
	}
	b = &Bound{Cluster: rec, Typed: typed, Dyn: dyn, Metrics: mx, Rest: cfg}
	r.mu.Lock()
	r.bound[key] = b
	r.mu.Unlock()
	return b, nil
}

// forget drops a cached client so the next Reach rebuilds it from KMS. Called
// whenever the credential behind it may have changed.
func (r *Registry) forget(org, name string) {
	r.mu.Lock()
	delete(r.bound, org+"/"+name)
	r.mu.Unlock()
}

// index reads one org's records. An absent index is an empty registry, which is
// the correct answer for an org that has registered nothing.
func (r *Registry) index(ctx context.Context, org string) ([]Cluster, error) {
	raw, err := r.store.Get(ctx, indexRef(org))
	if err != nil {
		return nil, fmt.Errorf("read cluster registry: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var list []Cluster
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("cluster registry for this org is corrupt: %w", err)
	}
	// The stored Org is authoritative for display, but a row that disagrees with
	// the index it was read from is a defect: normalise to the index's own org so
	// a hand-edited row cannot make a record claim a different owner.
	for i := range list {
		list[i].Org = org
	}
	return list, nil
}

func (r *Registry) writeIndex(ctx context.Context, org string, list []Cluster) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return r.store.Put(ctx, indexRef(org), raw)
}

// indexRef is where an org's registry lives. The "orgs/<org>/" prefix is not
// decoration: the KMS app authorizes a ref that names a tenant against the
// tenant the call acts for, so this shape buys a second, independent refusal of
// a cross-org read from a process that is not this one.
func indexRef(org string) string { return "orgs/" + org + "/k8s/clusters" }

// credRef is where one cluster's kubeconfig is sealed. DERIVED, never stored: a
// ref that came out of a record could be edited to name another org's material.
func credRef(org, name string) string {
	return "orgs/" + org + "/k8s/clusters/" + name + "/kubeconfig"
}

// ValidName reports whether a cluster name may be used. It must be a DNS label:
// the name becomes a KMS ref segment, so a name containing '/' or ".." would
// address material outside the org's own prefix.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(name)-1:
		default:
			return false
		}
	}
	return true
}

// cleanNamespaces normalises a namespace bound: trimmed, deduplicated, order
// preserved, empties dropped. An empty result means "the whole cluster", so a
// bound of only blanks must not silently become one — a caller that meant to
// bound and typed nothing gets no bound, which is why every op that can widen is
// gated on WholeCluster as well.
func cleanNamespaces(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, ns := range in {
		ns = strings.TrimSpace(ns)
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type inv struct{ nodes, nvidia, amd int }

// inventory is the reachability proof AND the fleet inventory, in one read. It
// is deliberately the same call for both: a "registered but unreachable" row is
// a row that will fail at first use, so the attach refuses instead.
func inventory(ctx context.Context, dyn dynamic.Interface) (inv, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ul, err := dyn.Resource(nodesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return inv{}, err
	}
	out := inv{nodes: len(ul.Items)}
	for _, n := range ul.Items {
		alloc, found, _ := unstructured.NestedMap(n.Object, "status", "allocatable")
		if !found {
			continue
		}
		out.nvidia += QuantityInt(alloc["nvidia.com/gpu"])
		out.amd += QuantityInt(alloc["amd.com/gpu"])
	}
	return out, nil
}

// QuantityInt reads a Kubernetes quantity that is a plain count. An accelerator
// count is always an integer; anything else is zero rather than an error,
// because a node that reports a malformed GPU count must not fail the whole
// inventory.
func QuantityInt(v any) int {
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" || s == "<nil>" {
		return 0
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func upsert(list []Cluster, rec Cluster) []Cluster {
	for i, c := range list {
		if c.Name == rec.Name {
			list[i] = rec
			return list
		}
	}
	return append(list, rec)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

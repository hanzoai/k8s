package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zap-proto/zip"
)

// This file is the adversarial suite. Every test in it is written from the attacker's
// side: a tenant that knows another tenant's cluster name, a tenant that knows another
// tenant's namespace, a caller with a header and no principal behind it.
//
// Each test names the guard it defeats when the guard is removed. Removing the org
// segment from indexRef makes TestForeignClusterNameIsNotFound and
// TestSameNameInTwoOrgsAreTwoClusters fail; removing the bound check from Bound.Scope
// makes TestForeignNamespaceIsRefused and TestBoundedListDoesNotWiden fail; removing
// the user-claim test from Tenant makes TestOrgHeaderWithoutPrincipalIsRefused fail.
// That mapping was verified by making each edit and observing the red, not assumed.

// memStore is an in-memory [Store]. It records every ref touched, because the ref
// NAMESPACE is the boundary this suite is really testing: if a call for one org can
// name another org's ref, no later check matters.
type memStore struct {
	data map[string][]byte
	got  []string
	put  []string
}

func newStore() *memStore { return &memStore{data: map[string][]byte{}} }

func (m *memStore) Get(_ context.Context, ref string) ([]byte, error) {
	m.got = append(m.got, ref)
	return m.data[ref], nil
}

func (m *memStore) Put(_ context.Context, ref string, value []byte) error {
	m.put = append(m.put, ref)
	if value == nil {
		delete(m.data, ref)
		return nil
	}
	m.data[ref] = value
	return nil
}

// as is a context carrying a validated caller for one org. Both fields matter: an org
// with no user beside it is a claim the caller made about itself, which zip.Tenant
// refuses and this suite proves.
func as(org string) context.Context {
	return zip.WithCaller(context.Background(), zip.Caller{Org: org, User: "u@" + org})
}

// seed writes an org's index and a usable credential straight into the store, so the
// authorization path can be exercised with no apiserver anywhere.
func seed(t *testing.T, s *memStore, org, name string, namespaces []string) {
	t.Helper()
	var list []Cluster
	if raw := s.data[indexRef(org)]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	list = append(list, Cluster{
		Name: name, Org: org, Provider: "byo",
		Endpoint: "https://" + name + "." + org + ".invalid:6443", Namespaces: namespaces,
	})
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.data[indexRef(org)] = raw
	s.data[credRef(org, name)] = kubeconfig("https://" + name + "." + org + ".invalid:6443")
}

// kubeconfig is a minimal, valid kubeconfig for a host. It carries no credential
// plugin, so SafeRESTConfig accepts it; nothing in these tests dials it.
func kubeconfig(host string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: ` + host + `
    insecure-skip-tls-verify: true
contexts:
- name: c
  context:
    cluster: c
    user: u
current-context: c
users:
- name: u
  user:
    token: t
`)
}

func TestForeignClusterNameIsNotFound(t *testing.T) {
	t.Setenv(AllowPrivateHostsEnv, "1")
	s := newStore()
	seed(t, s, "alpha", "prod", nil)
	r := New(s)

	// beta knows the name. Knowing it must buy nothing.
	if _, err := r.Reach(as("beta"), "prod"); err == nil {
		t.Fatal("beta reached alpha's cluster by naming it — the org boundary is not being applied")
	} else if _, ok := err.(ErrNoCluster); !ok {
		t.Fatalf("want ErrNoCluster (a foreign cluster must be indistinguishable from an absent one), got %T: %v", err, err)
	}

	// And the refusal must not have touched alpha's material on the way.
	for _, ref := range append(s.got, s.put...) {
		if strings.Contains(ref, "orgs/alpha/") {
			t.Fatalf("a call acting for beta touched %q", ref)
		}
	}
}

func TestSameNameInTwoOrgsAreTwoClusters(t *testing.T) {
	t.Setenv(AllowPrivateHostsEnv, "1")
	s := newStore()
	seed(t, s, "alpha", "prod", nil)
	seed(t, s, "beta", "prod", nil)
	r := New(s)

	a, err := r.Reach(as("alpha"), "prod")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	b, err := r.Reach(as("beta"), "prod")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}
	if a.Cluster.Endpoint == b.Cluster.Endpoint {
		t.Fatalf("two orgs' clusters named %q resolved to one endpoint %q — a name is not a global identity",
			"prod", a.Cluster.Endpoint)
	}
	if a.Rest.Host == b.Rest.Host {
		t.Fatalf("both orgs got a client for %q — the credential is being shared across tenants", a.Rest.Host)
	}
}

func TestForeignNamespaceIsRefused(t *testing.T) {
	t.Setenv(AllowPrivateHostsEnv, "1")
	s := newStore()
	// beta is on the SHARED cluster with a scoped credential: it may address one
	// namespace and no other.
	seed(t, s, "beta", "shared", []string{"beta-prod"})
	r := New(s)

	b, err := r.Reach(as("beta"), "shared")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}
	for _, ns := range []string{"alpha-prod", "kube-system", "hanzo", ""} {
		if _, err := b.One(ns); err == nil {
			t.Fatalf("beta acted on namespace %q outside its bound", ns)
		} else if _, ok := err.(ErrNamespace); !ok {
			t.Fatalf("namespace %q: want ErrNamespace, got %T: %v", ns, err, err)
		}
	}
	if got, err := b.One("beta-prod"); err != nil || got != "beta-prod" {
		t.Fatalf("beta was refused its OWN namespace: %q %v", got, err)
	}
}

func TestBoundedListDoesNotWiden(t *testing.T) {
	t.Setenv(AllowPrivateHostsEnv, "1")
	s := newStore()
	seed(t, s, "beta", "shared", []string{"beta-prod", "beta-stage"})
	r := New(s)
	b, err := r.Reach(as("beta"), "shared")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}

	// An omitted namespace on a bounded registration means "my namespaces", never
	// "every namespace" — the empty string is what client-go reads as all of them, so
	// leaking it here would turn every list op into a whole-cluster read.
	scope, err := b.Scope("")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if len(scope) != 2 {
		t.Fatalf("want the bound, got %v", scope)
	}
	for _, ns := range scope {
		if ns == "" {
			t.Fatal(`Scope("") returned the all-namespaces sentinel for a BOUNDED registration`)
		}
		if !strings.HasPrefix(ns, "beta-") {
			t.Fatalf("scope leaked %q", ns)
		}
	}

	// Cluster-scoped reads cannot be narrowed at all, so a bounded registration must
	// not be able to make one.
	if err := b.WholeCluster(); err == nil {
		t.Fatal("a namespace-bounded registration was allowed a cluster-scoped read")
	}
}

func TestUnboundedRegistrationReadsWholeCluster(t *testing.T) {
	t.Setenv(AllowPrivateHostsEnv, "1")
	s := newStore()
	seed(t, s, "lux", "lux-k8s", nil)
	r := New(s)
	b, err := r.Reach(as("lux"), "lux-k8s")
	if err != nil {
		t.Fatalf("lux: %v", err)
	}
	// An org that registered its OWN cluster gave us the credential for all of it.
	// Refusing it here would be a guard inventing a boundary the owner did not draw.
	if err := b.WholeCluster(); err != nil {
		t.Fatalf("owner of lux-k8s refused a cluster-scoped read: %v", err)
	}
	scope, err := b.Scope("")
	if err != nil || len(scope) != 1 || scope[0] != "" {
		t.Fatalf("want the all-namespaces sentinel for an unbounded registration, got %v %v", scope, err)
	}
}

func TestOrgHeaderWithoutPrincipalIsRefused(t *testing.T) {
	s := newStore()
	seed(t, s, "alpha", "prod", nil)
	r := New(s)

	// The full rule, not a non-empty check. Each of these is a caller stating
	// something about itself that no validated principal backs.
	for name, c := range map[string]zip.Caller{
		"org header, no user": {Org: "alpha"},
		"user, no org":        {User: "u"},
		"nothing at all":      {},
		"over the org bound":  {Org: strings.Repeat("a", zip.MaxOrgLen+1), User: "u"},
		"org escapes its ref": {Org: "alpha/../beta", User: "u"},
		"org with a slash":    {Org: "alpha/beta", User: "u"},
	} {
		ctx := zip.WithCaller(context.Background(), c)
		if _, err := r.List(ctx); err == nil {
			t.Fatalf("%s: the registry answered a call with no validated tenant", name)
		} else if _, ok := err.(ErrNoTenant); !ok {
			t.Fatalf("%s: want ErrNoTenant, got %T: %v", name, err, err)
		}
	}
}

func TestClusterNameCannotEscapeItsRef(t *testing.T) {
	// A cluster name becomes a KMS ref segment, so a name containing a separator or a
	// parent reference would address material outside the org's own prefix. This is the
	// one place a name is checked, and Register is the only door it comes through.
	for _, bad := range []string{
		"../alpha/prod", "a/b", "prod/../../alpha", ".", "..", "-prod", "prod-",
		"Prod", "pro d", "", strings.Repeat("p", MaxNameLen+1),
	} {
		if ValidName(bad) {
			t.Fatalf("ValidName accepted %q, which can address another org's material", bad)
		}
	}
	for _, ok := range []string{"prod", "hanzo-k8s", "lux-k8s", "zoo-k8s", "bootnode-k8s", "a1"} {
		if !ValidName(ok) {
			t.Fatalf("ValidName rejected the legitimate name %q", ok)
		}
	}
}

func TestRefsAreOrgPrefixed(t *testing.T) {
	// The "orgs/<org>/" prefix is what makes the KMS app apply its OWN tenant rule to
	// every ref this app resolves — a second refusal, in a second process. A ref shape
	// that lost the prefix would silently drop that second gate, so it is asserted.
	if got := indexRef("alpha"); got != "orgs/alpha/k8s/clusters" {
		t.Fatalf("index ref %q is not under the org's KMS prefix", got)
	}
	if got := credRef("alpha", "prod"); got != "orgs/alpha/k8s/clusters/prod/kubeconfig" {
		t.Fatalf("credential ref %q is not under the org's KMS prefix", got)
	}
}

func TestDeregisterDestroysTheCredential(t *testing.T) {
	t.Setenv(AllowPrivateHostsEnv, "1")
	s := newStore()
	seed(t, s, "beta", "prod", nil)
	r := New(s)
	ctx := as("beta")

	removed, err := r.Deregister(ctx, "prod")
	if err != nil || !removed {
		t.Fatalf("deregister: %v %v", removed, err)
	}
	if v := s.data[credRef("beta", "prod")]; len(v) != 0 {
		t.Fatalf("the sealed credential survived deregistration: %d bytes still readable", len(v))
	}
	if _, err := r.Reach(ctx, "prod"); err == nil {
		t.Fatal("a deregistered cluster is still reachable")
	}
	// Twice is not an error: an operator retrying a deregistration must not be told
	// something went wrong.
	if removed, err := r.Deregister(ctx, "prod"); err != nil || removed {
		t.Fatalf("second deregister: %v %v", removed, err)
	}
}

func TestDeregisterCannotReachAcrossOrgs(t *testing.T) {
	t.Setenv(AllowPrivateHostsEnv, "1")
	s := newStore()
	seed(t, s, "alpha", "prod", nil)
	r := New(s)

	// beta naming alpha's cluster must not remove it, and must not touch its material.
	removed, err := r.Deregister(as("beta"), "prod")
	if err != nil || removed {
		t.Fatalf("beta deregistered alpha's cluster: %v %v", removed, err)
	}
	if len(s.data[credRef("alpha", "prod")]) == 0 {
		t.Fatal("beta destroyed alpha's sealed credential")
	}
	if _, err := r.Reach(as("alpha"), "prod"); err != nil {
		t.Fatalf("alpha lost its own cluster: %v", err)
	}
}

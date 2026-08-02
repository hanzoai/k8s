package ops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/plane"
	"github.com/hanzoai/k8s/registry"
)

// This suite drives the REAL router, because the guards are only worth what the wire
// enforces. A handler that refuses correctly behind a route nothing reaches is a guard
// in no path, which is the failure mode §8.9 of the plugin contract exists to catch.

// store is an in-memory registry store, seeded by org.
type store struct{ data map[string][]byte }

func (s store) Get(_ context.Context, ref string) ([]byte, error) { return s.data[ref], nil }
func (s store) Put(_ context.Context, ref string, v []byte) error {
	if v == nil {
		delete(s.data, ref)
		return nil
	}
	s.data[ref] = v
	return nil
}

// seeded builds an app whose registry holds one cluster per org given.
func seeded(t *testing.T, rows map[string][]registry.Cluster) *zip.App {
	t.Helper()
	t.Setenv(registry.AllowPrivateHostsEnv, "1")
	s := store{data: map[string][]byte{}}
	for org, list := range rows {
		for i := range list {
			list[i].Org = org
			if list[i].Endpoint == "" {
				list[i].Endpoint = "https://" + list[i].Name + "." + org + ".invalid:6443"
			}
			s.data["orgs/"+org+"/k8s/clusters/"+list[i].Name+"/kubeconfig"] = []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: ` + list[i].Endpoint + `, insecure-skip-tls-verify: true}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: t}}]
`)
		}
		raw, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		s.data["orgs/"+org+"/k8s/clusters"] = raw
	}
	app := zip.New(zip.Config{AppName: "k8s"})
	Ops{Reg: registry.New(s)}.Mount(app)
	if err := app.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return app
}

// ask sends one request through the composed router with the identity headers a
// gateway would have attached, and returns the status and body.
func ask(t *testing.T, app *zip.App, method, path, org string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if org != "" {
		req.Header.Set(zip.HeaderOrg, org)
		req.Header.Set(zip.HeaderUser, "u@"+org)
	}
	resp, err := app.Fiber().Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestNoIdentityIsRefusedOnTheWire(t *testing.T) {
	app := seeded(t, map[string][]registry.Cluster{"alpha": {{Name: "prod"}}})
	// An X-Org-Id with no validated user beside it is a claim the client made about
	// itself. This is the request an attacker actually sends.
	req := httptest.NewRequest("GET", "/v1/k8s/clusters", nil)
	req.Header.Set(zip.HeaderOrg, "alpha")
	resp, err := app.Fiber().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("a forged org header was served: %d %s", resp.StatusCode, body)
	}
	// And with nothing at all.
	if code, body := ask(t, app, "GET", "/v1/k8s/clusters", ""); code != 403 {
		t.Fatalf("an unauthenticated request was served: %d %s", code, body)
	}
}

func TestTenantSeesOnlyItsOwnClusters(t *testing.T) {
	app := seeded(t, map[string][]registry.Cluster{
		"alpha": {{Name: "prod"}, {Name: "hanzo-k8s"}},
		"beta":  {{Name: "prod"}},
	})
	code, body := ask(t, app, "GET", "/v1/k8s/clusters", "beta")
	if code != 200 {
		t.Fatalf("beta: %d %s", code, body)
	}
	var out plane.Clusters
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(out.Clusters) != 1 || out.Clusters[0].Name != "prod" || out.Clusters[0].Org != "beta" {
		t.Fatalf("beta's cluster list is not beta's: %s", body)
	}
	if strings.Contains(body, "hanzo-k8s") || strings.Contains(body, "alpha") {
		t.Fatalf("beta's list leaked alpha's fleet: %s", body)
	}
	// The kubeconfig must not be in the answer under any spelling.
	for _, leak := range []string{"kubeconfig", "apiVersion: v1", "token", "insecure"} {
		if strings.Contains(body, leak) {
			t.Fatalf("a cluster row carried %q: %s", leak, body)
		}
	}
}

func TestForeignClusterIs404OnTheWire(t *testing.T) {
	app := seeded(t, map[string][]registry.Cluster{"alpha": {{Name: "prod"}}})
	for _, path := range []string{
		"/v1/k8s/clusters/prod",
		"/v1/k8s/clusters/prod/status",
		"/v1/k8s/nodes?cluster=prod",
		"/v1/k8s/pods?cluster=prod",
		"/v1/k8s/namespaces?cluster=prod",
		"/v1/k8s/volumes/claims?cluster=prod",
		"/v1/k8s/workloads/deployments?cluster=prod",
		"/v1/k8s/jobs?cluster=prod",
		"/v1/k8s/resources?cluster=prod&kind=App",
		"/v1/k8s/metrics/pods?cluster=prod",
	} {
		code, body := ask(t, app, "GET", path, "beta")
		if code != 404 {
			t.Fatalf("%s answered %d for a foreign cluster (a 403 would confirm the name exists): %s", path, code, body)
		}
	}
}

func TestForeignNamespaceIs403OnTheWire(t *testing.T) {
	app := seeded(t, map[string][]registry.Cluster{
		"beta": {{Name: "shared", Namespaces: []string{"beta-prod"}}},
	})
	// beta knows alpha's namespace name. Knowing it must buy nothing.
	code, body := ask(t, app, "GET", "/v1/k8s/namespaces/alpha-prod?cluster=shared", "beta")
	if code != 403 {
		t.Fatalf("beta read a namespace outside its bound: %d %s", code, body)
	}
	if !strings.Contains(body, "outside what this org's registration") {
		t.Fatalf("the refusal does not explain itself: %s", body)
	}
	// And a cluster-scoped read on a bounded registration is refused, so a scoped
	// tenant cannot see the shared cluster's nodes or PersistentVolumes.
	for _, path := range []string{
		"/v1/k8s/nodes?cluster=shared",
		"/v1/k8s/volumes/persistent?cluster=shared",
		"/v1/k8s/nodes/volume-stats?cluster=shared",
	} {
		if code, body := ask(t, app, "GET", path, "beta"); code != 403 {
			t.Fatalf("%s answered %d for a namespace-bounded registration: %s", path, code, body)
		}
	}
}

func TestUnregisteredKindIsRefused(t *testing.T) {
	app := seeded(t, map[string][]registry.Cluster{"alpha": {{Name: "prod"}}})
	// The kinds a passthrough would have accepted. Each must be refused by NAME, before
	// any cluster is dialed, because the kind table is the bound on this app's authority.
	for _, kind := range []string{"Secret", "ClusterRole", "Pod", "Deployment", "Node", "CustomResourceDefinition", ""} {
		code, body := ask(t, app, "GET", "/v1/k8s/resources?cluster=prod&kind="+kind, "alpha")
		if code != 400 {
			t.Fatalf("kind %q answered %d — the closed kind set is not being applied: %s", kind, code, body)
		}
	}
	// And the refusal tells the caller what IS addressable, so a legitimate consumer
	// is not left guessing.
	_, body := ask(t, app, "GET", "/v1/k8s/resources?cluster=prod&kind=Secret", "alpha")
	for _, kind := range Kinds() {
		if !strings.Contains(body, kind) {
			t.Fatalf("the refusal omits the registered kind %q: %s", kind, body)
		}
	}
}

func TestKindSetIsClosedAndNamed(t *testing.T) {
	// The set is asserted here so growing it is a visible, reviewed diff rather than a
	// side effect of adding a line to a map. Every entry is one more thing the union
	// ClusterRole in LLM.md has to grant.
	want := []string{
		"App", "AppProject", "Application", "Datastore", "InferenceService",
		"IngressRoute", "KMSSecret", "LuxNetwork", "Middleware", "TrainJob",
	}
	got := Kinds()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the closed kind set changed\n got: %v\nwant: %v", got, want)
	}
	// A core-group kind must never appear here: a Secret or a Pod reachable through
	// resources.apply would be the passthrough this table exists to prevent.
	for _, kind := range got {
		if g := kinds[kind]; g.Group == "" {
			t.Fatalf("kind %q is in the CORE group — resources.* addresses custom resources only", kind)
		}
	}
}

func TestNoSecretOpExists(t *testing.T) {
	// The strongest statement this suite can make about secrets is that there is no
	// route to one. Material reaches a namespace as a KMSSecret the operator resolves;
	// nothing here reads a Secret back, so nothing here can exfiltrate one.
	app := seeded(t, map[string][]registry.Cluster{"alpha": {{Name: "prod"}}})
	for _, r := range app.Declaration().Routes {
		if strings.Contains(strings.ToLower(r.Pattern), "secret") {
			t.Fatalf("a route addresses secrets: %s %s", r.Method, r.Pattern)
		}
	}
	for _, op := range app.Declaration().Ops {
		if strings.Contains(op, "secret") {
			t.Fatalf("an op addresses secrets: %s", op)
		}
	}
}

func TestFailMapsRefusalsToStatuses(t *testing.T) {
	// The mapping is the wire contract for a refusal, so it is asserted directly as
	// well as through the routes above.
	for _, tc := range []struct {
		err  error
		want int
	}{
		{registry.ErrNoTenant{}, 403},
		{registry.ErrNoCluster{Name: "prod"}, 404},
		{registry.ErrNamespace{Cluster: "c", Namespace: "n"}, 403},
		{registry.ErrScope{Cluster: "c"}, 403},
		{registry.ErrName{Name: "../x"}, 400},
		{errors.New("boom"), 500},
	} {
		var he *zip.HTTPError
		if !errors.As(fail(tc.err), &he) {
			t.Fatalf("%T did not map to an HTTP error", tc.err)
		}
		if he.Status != tc.want {
			t.Fatalf("%T mapped to %d, want %d", tc.err, he.Status, tc.want)
		}
	}
}

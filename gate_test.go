package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/ops"
	"github.com/hanzoai/k8s/registry"
)

// The gates HIP-0106 §8 requires, each named for the failure it catches. Two properties
// hold throughout, because they are the two ways a green tick lies:
//
//   - NON-VACUITY. A gate that examined nothing FAILS and says how much it examined.
//     Every route to zero examinations is a defect elsewhere that would otherwise arrive
//     as a pass.
//   - A SOURCE ON ONE SIDE. Never two derived artifacts compared with each other. Here
//     the live router is the source: the declaration and the OpenAPI subset are both
//     projected from it inside the test, and the committed files are compared against
//     that projection rather than against one another.

// app builds the composition this binary serves, exactly as run() does. Nothing is
// stubbed: the registry is the real one over a store that is never reached, because a
// projection is a function of the code and must not need a peer.
func app(t *testing.T) *zip.App {
	t.Helper()
	a := zip.New(zip.Config{AppName: name})
	ops.Ops{Reg: registry.New(registry.Sealed{})}.Mount(a)
	if err := a.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return a
}

// sh executes a tool and returns its combined output, failing the test on error.
func sh(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// §8.1 — the plugin is a LEAF.
func TestPluginIsALeaf(t *testing.T) {
	// A grep over source is NOT this gate: the dangerous break is the one that still
	// compiles, so the check is over the import graph a build actually produces.
	out := sh(t, "go", "list", "-deps", "./...")
	deps := strings.Fields(out)
	if len(deps) < 100 {
		t.Fatalf("this gate proved nothing: the dependency graph came back as %d packages", len(deps))
	}
	for _, d := range deps {
		if strings.HasPrefix(d, "github.com/hanzoai/cloud") || strings.HasPrefix(d, "github.com/hanzoai/base") {
			t.Errorf("this plugin links the host package %s — that is the 316-package coupling the contract removes", d)
		}
	}
	t.Logf("%d packages, none of them a host's", len(deps))
}

// §1.2 — the measured floor, reported rather than asserted at a number.
func TestPackageWeightIsReported(t *testing.T) {
	binary := strings.Fields(sh(t, "go", "list", "-deps", "."))
	leaf := strings.Fields(sh(t, "go", "list", "-deps", "./plane"))
	var k8sFamily int
	for _, d := range binary {
		if strings.HasPrefix(d, "k8s.io/") || strings.HasPrefix(d, "sigs.k8s.io/") {
			k8sFamily++
		}
	}
	// The plane leaf is the number that matters to a CONSUMER: it is what an app links
	// to ask this service a question, and it must stay at one package. A consumer that
	// linked client-go to make an RPC would have gained nothing from the split.
	if len(leaf) != 1 {
		t.Fatalf("the plane contract is %d packages; a consumer must be able to link it without linking a Kubernetes client:\n%s",
			len(leaf), strings.Join(leaf, "\n"))
	}
	// kustomize is a RENDERING concern and belongs to whoever renders. Worth 74-84
	// packages on its own, and it has no business in a cluster client.
	for _, d := range binary {
		if strings.Contains(d, "kustomize") {
			t.Errorf("%s: rendering belongs to the deploy side, not to the cluster client", d)
		}
	}
	t.Logf("binary %d packages (k8s family %d), plane contract %d package", len(binary), k8sFamily, len(leaf))
}

// §8.3 — the committed declaration is current, and the OpenAPI subset is a subset of it.
func TestDeclarationIsCurrent(t *testing.T) {
	live := app(t).Declaration()
	committed := readJSON[zip.Declaration](t, "k8s.plugin.json")
	if diff := jsonDiff(t, live, committed); diff != "" {
		t.Fatalf("k8s.plugin.json is not what the router serves; run `make generate`:\n%s", diff)
	}
	if len(live.Routes) == 0 || len(live.Ops) == 0 {
		t.Fatal("this gate proved nothing: the declaration is empty")
	}

	// openapi ⊆ declaration. The relation is one-way on purpose: a served-but-unpublished
	// route must still be DECLARED or a host will not route it, which is the outage class
	// where every product beacon in the fleet answered 405 for want of a table row.
	spec := readJSON[struct {
		Paths map[string]map[string]any `json:"paths"`
	}](t, "openapi.json")
	declared := map[string]bool{}
	for _, r := range live.Routes {
		declared[openAPIPath(r.Pattern)] = true
	}
	for p := range spec.Paths {
		if !declared[p] {
			t.Errorf("the OpenAPI document publishes %s, which the declaration does not carry — a host will not route it", p)
		}
	}
	t.Logf("%d routes, %d ops, %d published paths", len(live.Routes), len(live.Ops), len(spec.Paths))
}

// §8.4 — the committed OpenAPI subset is current.
func TestOpenAPIIsCurrent(t *testing.T) {
	live := app(t).OpenAPISpec()
	committed := readJSON[map[string]any](t, "openapi.json")
	if diff := jsonDiff(t, live, committed); diff != "" {
		t.Fatalf("openapi.json is not what the router serves; run `make generate`:\n%s", diff)
	}
}

// §8.5 — zipdoc is wired, current, and every op actually carries prose.
func TestProseReachesBothProjections(t *testing.T) {
	// The directive is the ONLY way a doc comment reaches a document: Go drops comments
	// at compile time and reflection sees types, never prose. A package without it can
	// never get a description into the spec or into an MCP tool, however correct
	// everything else is.
	for _, dir := range typedOpDirs(t) {
		src, err := os.ReadFile(filepath.Join(dir, "zipdoc_gen.go"))
		if err != nil {
			t.Fatalf("%s registers typed ops but has no committed zipdoc_gen.go: %v", dir, err)
		}
		if len(src) == 0 {
			t.Fatalf("%s/zipdoc_gen.go is empty", dir)
		}
		if !strings.Contains(readFile(t, filepath.Join(dir, "ops.go")), "//go:generate go run github.com/zap-proto/zip/cmd/zipdoc") {
			t.Fatalf("%s is missing the zipdoc directive, so its prose reaches neither projection", dir)
		}
		sh(t, "go", "run", "github.com/zap-proto/zip/cmd/zipdoc", "-check", "./"+dir)
	}

	// And the prose is actually THERE, in both projections, for every op. A directive
	// that runs over handlers with no comments is a gate that proves nothing.
	a := app(t)
	spec, err := json.Marshal(a.OpenAPISpec())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Description string `json:"description"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	described := 0
	for p, methods := range doc.Paths {
		for m, op := range methods {
			if op.OperationID == "" {
				t.Errorf("%s %s has no operationId, so it is invisible to the plane, the MCP tool list and the CLI", m, p)
			}
			if strings.TrimSpace(op.Description) == "" {
				t.Errorf("%s %s has no description: an agent choosing whether to call it would read nothing", m, p)
				continue
			}
			described++
		}
	}
	tools := a.MCPTools()
	for _, tool := range tools {
		if d, _ := tool["description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("MCP tool %v has no description", tool["name"])
		}
	}
	if described == 0 || len(tools) == 0 {
		t.Fatal("this gate proved nothing: no described operations and no tools")
	}
	if len(tools) != described {
		t.Errorf("%d described operations but %d MCP tools — one projection is losing ops", described, len(tools))
	}
	t.Logf("%d operations carry prose in both the OpenAPI document and the MCP tool list", described)
}

// §3.4 — the shape a host will accept.
func TestEveryRouteIsRoutable(t *testing.T) {
	live := app(t).Declaration()
	if live.Name != name {
		t.Fatalf("the declaration names %q but the binary is %q — one name, no mapping table", live.Name, name)
	}
	if live.Eager {
		t.Error("this app is request-driven: it owns no listener, consumer or background loop, so a host must be free to start it lazily")
	}
	for _, r := range live.Routes {
		switch {
		case !strings.HasPrefix(r.Pattern, "/v1/k8s"):
			t.Errorf("%s %s is outside this app's own prefix, so it would claim a path another plugin owns", r.Method, r.Pattern)
		case strings.Contains(r.Pattern, ".."):
			t.Errorf("%s %s contains a parent reference", r.Method, r.Pattern)
		case strings.Contains(r.Pattern, "*"):
			t.Errorf("%s %s is a catch-all: it would claim paths other plugins own and paths that do not exist yet", r.Method, r.Pattern)
		case strings.Contains(r.Pattern, "/api/"):
			t.Errorf("%s %s carries an /api/ prefix", r.Method, r.Pattern)
		case strings.Contains(r.Pattern, "/v2"):
			t.Errorf("%s %s is a v2", r.Method, r.Pattern)
		}
	}
	if len(live.Routes) < 30 {
		t.Fatalf("this gate examined %d routes; the measured demand is 36 ops", len(live.Routes))
	}
}

// The op set and the plane contract are one set, read from two sources.
func TestEveryPlaneConstantIsAnOpAndViceVersa(t *testing.T) {
	// One side is the LIVE ROUTER, the other is the plane package's own const block. A
	// constant nobody registered is an op a consumer will call and get nothing from; an
	// op whose token is not declared is a name a consumer cannot spell without copying a
	// string. Both fail here rather than at the first call.
	registered := app(t).Declaration().Ops
	declared := planeConstants(t)
	if len(registered) == 0 || len(declared) == 0 {
		t.Fatal("this gate proved nothing: one of the two sets is empty")
	}
	in := func(set []string, v string) bool {
		for _, s := range set {
			if s == v {
				return true
			}
		}
		return false
	}
	for _, tok := range declared {
		if !in(registered, tok) {
			t.Errorf("plane declares the op token %q but nothing registers it", tok)
		}
	}
	for _, tok := range registered {
		if !in(declared, tok) {
			t.Errorf("the router serves the op token %q but plane does not declare it, so a consumer cannot spell it", tok)
		}
	}
	t.Logf("%d op tokens, declared and served", len(declared))
}

// §8.7 — tenancy, read off the published document so every input is covered.
func TestNoOperationTakesATenant(t *testing.T) {
	spec, err := json.Marshal(app(t).OpenAPISpec())
	if err != nil {
		t.Fatal(err)
	}
	// Walk the WHOLE document rather than a list of types: a parameter name, a schema
	// property, a $ref'd component — any of them naming a tenant is the defect, and
	// walking everything means a new one cannot slip in beside a check that only looked
	// at inputs it knew about.
	var doc any
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"org": true, "orgId": true, "organization": true, "tenant": true, "tenantId": true}
	seen := 0
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, sub := range t2 {
				seen++
				// An org on a RESPONSE schema is a property of the thing described. The
				// path tells them apart: only request bodies and parameters are inputs.
				if banned[k] && (strings.Contains(path, "requestBody") || strings.Contains(path, "parameters")) {
					t.Errorf("%s names %q: an org in the argument is an org the caller chose", path, k)
				}
				walk(path+"."+k, sub)
			}
		case []any:
			for i, sub := range t2 {
				if m, ok := sub.(map[string]any); ok {
					if n, isStr := m["name"].(string); isStr && banned[n] && strings.Contains(path, "parameters") {
						t.Errorf("%s[%d] is a %q parameter: a caller that can name the org can read another tenant", path, i, n)
					}
				}
				walk(path, sub)
			}
		}
	}
	walk("", doc)
	if seen < 200 {
		t.Fatalf("this gate walked only %d nodes of the document; it did not read the spec it is protecting", seen)
	}
	t.Logf("walked %d document nodes", seen)
}

// Every op that reaches a cluster NAMES it. This is the multi-cluster invariant, gated.
func TestEveryClusterOpNamesItsCluster(t *testing.T) {
	spec, err := json.Marshal(app(t).OpenAPISpec())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name string `json:"name"`
			} `json:"parameters"`
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]any `json:"properties"`
						Ref        string         `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	// The two ops that legitimately do not name a cluster: listing the registry, and
	// registering a cluster (which is where a name comes to exist, so it takes `name`).
	exempt := map[string]bool{"k8s_clusters_list": true, "k8s_clusters_register": true}
	checked := 0
	for p, methods := range doc.Paths {
		for m, op := range methods {
			if exempt[op.OperationID] {
				continue
			}
			checked++
			named := false
			for _, param := range op.Parameters {
				if param.Name == "cluster" {
					named = true
				}
			}
			for _, c := range op.RequestBody.Content {
				if _, ok := c.Schema.Properties["cluster"]; ok {
					named = true
				}
				// A $ref'd body carries the field through its component schema, which the
				// plane package's own gate covers; a ref is therefore accepted here.
				if c.Schema.Ref != "" {
					named = true
				}
			}
			if !named {
				t.Errorf("%s %s (%s) does not name a cluster — there is no ambient cluster, so this op would read whichever apiserver the process was started next to",
					m, p, op.OperationID)
			}
		}
	}
	if checked < 30 {
		t.Fatalf("this gate examined %d operations; the measured demand is 36", checked)
	}
	t.Logf("%d operations, every one naming its cluster", checked)
}

// §8.2 — the binary builds and mounts its whole surface alone.
func TestBuildsAndMountsAlone(t *testing.T) {
	bin := filepath.Join(t.TempDir(), name)
	sh(t, "go", "build", "-o", bin, ".")
	// `declare` exercises construct → register → project without a host, a store or a
	// peer, which is exactly what "mounts alone" means for a request-driven app: if any
	// of that needed something else to have initialised it first, this exits non-zero.
	out := filepath.Join(t.TempDir(), "d.json")
	sh(t, bin, "declare", out)
	d := readJSON[zip.Declaration](t, out)
	if len(d.Routes) == 0 {
		t.Fatal("the binary mounted no routes when run with no host")
	}
}

// §8.8 — the suite is not empty, and every package contributes.
func TestEveryPackageHasTests(t *testing.T) {
	pkgs := strings.Fields(sh(t, "go", "list", "./..."))
	if len(pkgs) < 4 {
		t.Fatalf("this gate proved nothing: %d packages", len(pkgs))
	}
	for _, p := range pkgs {
		dir := "."
		if rel := strings.TrimPrefix(p, "github.com/hanzoai/k8s"); rel != "" {
			dir = "." + rel
		}
		matches, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil || len(matches) == 0 {
			t.Errorf("%s has no test file: a package with no executed test is a package the suite does not cover", p)
		}
	}
}

// §8.9 — CI invokes the gate. Without this every gate above protects nobody.
func TestCIInvokesMakeTest(t *testing.T) {
	y := readFile(t, "hanzo.yml")
	if !strings.Contains(y, "make test") {
		t.Fatal("hanzo.yml's test: block does not run `make test`, so every gate in this file is a comment")
	}
	w := readFile(t, ".github/workflows/cicd.yml")
	if !strings.Contains(w, "hanzoai/ci/.github/workflows/build.yml@v1") {
		t.Fatal("the workflow does not import the canonical hanzoai/ci reusable, so this repo has its own build logic")
	}
	mk := readFile(t, "Makefile")
	for _, target := range []string{"generate:", "test:"} {
		if !strings.Contains(mk, target) {
			t.Fatalf("the Makefile has no %s target", target)
		}
	}
	// The regeneration gates above are worth nothing if `make test` does not run them.
	if !strings.Contains(mk, "test ./...") {
		t.Fatal("`make test` does not run the Go suite")
	}
}

// ---- helpers ---------------------------------------------------------------

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(raw)
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(readFile(t, path)), &out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

// jsonDiff compares two values by their CANONICAL JSON.
//
// Both sides go through a marshal → unmarshal → marshal round trip, which is not
// ceremony: the live spec carries authored examples as json.RawMessage, whose field
// order is preserved verbatim, while the committed file re-parses into maps, whose keys
// marshal sorted. Comparing one against the other directly reports a diff on every
// example in the document and would have made this gate permanently red for no reason.
func jsonDiff(t *testing.T, live, committed any) string {
	t.Helper()
	a, err := canonical(live)
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonical(committed)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		return ""
	}
	al, bl := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	var out []string
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, "live:      "+x, "committed: "+y)
			if len(out) >= 12 {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// canonical renders a value as JSON with every map key sorted and every nested document
// re-parsed, so two spellings of one document compare equal.
func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var norm any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return nil, err
	}
	return json.MarshalIndent(norm, "", "  ")
}

// openAPIPath rewrites a router pattern into the document's spelling: ":id" is the
// router's, "{id}" is OpenAPI's, and the two must be compared in one vocabulary.
func openAPIPath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "{" + p[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}

// typedOpDirs returns every directory in this module that registers a typed op, read
// from the AST rather than from a list, so a new package is covered by §8.5 the moment
// it registers its first op.
func typedOpDirs(t *testing.T) []string {
	t.Helper()
	verbs := map[string]bool{"Get": true, "Post": true, "Put": true, "Patch": true, "Delete": true}
	found := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "zip" && verbs[sel.Sel.Name] && len(call.Args) >= 3 {
				found[filepath.Dir(path)] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("this gate proved nothing: no package registers a typed op")
	}
	out := make([]string, 0, len(found))
	for d := range found {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// planeConstants reads the op tokens out of the plane package's own const declarations,
// so the comparison against the router is source against source.
func planeConstants(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "plane/plane.go", nil, 0)
	if err != nil {
		t.Fatalf("parse plane: %v", err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v := strings.Trim(lit.Value, `"`)
		if strings.HasPrefix(v, "k8s_") {
			out = append(out, v)
		}
		return true
	})
	sort.Strings(out)
	return out
}

// The RBAC ceiling and the code are one set, read from two sources.
func TestRBACCoversExactlyTheClosedKindSet(t *testing.T) {
	// A kind added to the code with no rule here 403s in production; a rule here with no
	// kind in the code is privilege nothing uses. Both are failures, and neither is
	// visible without this comparison — which is why the grant lives in the repo that
	// performs the verbs rather than in a chart nobody diffs against the code.
	rbac := readFile(t, "rbac.yaml")
	kinds := ops.Kinds()
	if len(kinds) == 0 {
		t.Fatal("this gate proved nothing: the kind set is empty")
	}
	for _, kind := range kinds {
		// The rule names the plural RESOURCE, so the kind's own lower-cased plural is
		// what has to appear. The two spellings differ only by pluralisation, and every
		// kind in this app's table pluralises by adding "s".
		resource := strings.ToLower(kind) + "s"
		if !strings.Contains(rbac, resource) {
			t.Errorf("rbac.yaml grants nothing for %s (%s): the op would 403 in production", kind, resource)
		}
	}
	// The absence of a secrets grant is a design decision, so it is asserted rather than
	// left to be quietly re-added by someone who needed one read.
	// The comparison is on EXACT resource tokens, not on substrings: "kmssecrets" is the
	// CR that exists precisely so no core Secret is ever read, and a substring check
	// would flag the solution as the problem.
	for _, line := range strings.Split(rbac, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "resources:") {
			continue
		}
		list := strings.Trim(strings.TrimPrefix(trimmed, "resources:"), " []")
		for _, item := range strings.Split(list, ",") {
			if strings.TrimSpace(item) == "secrets" {
				t.Errorf("rbac.yaml grants core secrets (%q) — material reaches a namespace as a KMSSecret, and nothing here reads a Secret back", trimmed)
			}
		}
	}
	t.Logf("%d custom-resource kinds, each with a rule", len(kinds))
}

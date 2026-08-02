package plane

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// The contract this file gates is the one the compiler cannot state: that no input type
// carries a tenant, and that no input type reaches its cluster name through an embedded
// struct. Both are read off the source, so a type added tomorrow is covered without
// anybody remembering to add it here.

// structs parses this package and returns every struct type it declares.
func structs(t *testing.T) map[string]*ast.StructType {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := map[string]*ast.StructType{}
	for _, p := range pkgs {
		for _, f := range p.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				if st, isStruct := ts.Type.(*ast.StructType); isStruct {
					out[ts.Name.Name] = st
				}
				return true
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("this gate proved nothing: no struct types were parsed")
	}
	return out
}

func fields(st *ast.StructType) []string {
	var out []string
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
		if len(f.Names) == 0 {
			// An embedded field. Recorded under its type name so the embedding gate
			// below can see it.
			if id, ok := f.Type.(*ast.Ident); ok {
				out = append(out, "«embedded "+id.Name+"»")
			} else {
				out = append(out, "«embedded»")
			}
		}
	}
	return out
}

// isInput reports whether a type name is one that travels IN. The convention IS the
// discriminator — a name ending in "In" or "Ref", plus the two general selectors — and it
// is a convention worth enforcing precisely so this gate can tell an input from a reply
// without a hand-maintained list a new type would be missing from.
//
// A reply may legitimately carry an org: Registered.Org says who OWNS a cluster, which a
// cross-org observer has to see. It may also carry an owner: Workload.Owner is a
// Kubernetes ownerReference, the same word for a different thing.
func isInput(name string) bool {
	return strings.HasSuffix(name, "In") || strings.HasSuffix(name, "Ref") ||
		name == "Selector" || name == "NoArgs"
}

func TestNoInputCarriesATenant(t *testing.T) {
	// An org in the argument is an org the CALLER chose, and a caller that can name the
	// org can read another tenant. The tenant rides the caller and the callee re-derives
	// it; there is no field for it, on any op, on any transport.
	banned := map[string]bool{
		"org": true, "orgid": true, "organization": true,
		"tenant": true, "tenantid": true,
	}
	checked := 0
	for name, st := range structs(t) {
		if !isInput(name) {
			continue
		}
		checked++
		for _, f := range fields(st) {
			if banned[strings.ToLower(f)] {
				t.Errorf("%s.%s: an input must not name a tenant — the tenant rides the caller", name, f)
			}
		}
	}
	// Non-vacuity: this app answers 36 ops, so a run that examined a handful of types
	// found the wrong set and must fail rather than pass quietly.
	if checked < 18 {
		t.Fatalf("this gate examined only %d input types; the naming convention it discriminates on has drifted", checked)
	}
	t.Logf("examined %d input types", checked)
}

func TestNoTypeEmbedsAnother(t *testing.T) {
	// This is not style. zip binds a URL value onto the TOP LEVEL of an input only, so a
	// `Cluster` promoted through an embedded struct never binds from `?cluster=` or from
	// a `:cluster` path segment — every op would silently read the empty cluster name and
	// answer 404 for a cluster that exists. The field is spelled per type for that
	// reason, and embedding one back in is the regression this gate names.
	for name, st := range structs(t) {
		for _, f := range fields(st) {
			if strings.HasPrefix(f, "«embedded") {
				t.Errorf("%s embeds %s: a promoted field does not bind from a URL, so every op using it would read an empty value",
					name, strings.TrimSuffix(strings.TrimPrefix(f, "«embedded "), "»"))
			}
		}
	}
}

func TestOpTokensAreNamespacedAndUnique(t *testing.T) {
	// Every op token is this app's, prefixed so a token can never collide with another
	// service's on the shared call plane, and unique so two ops cannot answer one name.
	tokens := []string{
		ClustersList, ClustersGet, ClustersRegister, ClustersDeregister, ClustersStatus,
		NodesList, NodesCordon, NodesVolumeStats,
		NamespacesList, NamespacesGet, NamespacesEnsure, NamespacesDelete,
		PodsList, PodsLogs,
		VolumesListClaims, VolumesListPersistent, VolumesEnsureClaim, VolumesExpandClaim,
		WorkloadsListDeployments, WorkloadsGetDeployment, WorkloadsListReplicaSets,
		WorkloadsListStatefulSets, WorkloadsListServices, WorkloadsListIngresses,
		WorkloadsListEvents, WorkloadsListConfigMaps,
		JobsCreate, JobsList, JobsGet,
		ResourcesList, ResourcesGet, ResourcesWatch, ResourcesApply, ResourcesDelete,
		AccessCan, MetricsPods,
	}
	seen := map[string]bool{}
	for _, tok := range tokens {
		if !strings.HasPrefix(tok, "k8s_") {
			t.Errorf("op token %q is not namespaced to this app", tok)
		}
		if seen[tok] {
			t.Errorf("op token %q is declared twice", tok)
		}
		seen[tok] = true
	}
	if len(seen) != 36 {
		t.Fatalf("the op set is %d tokens; the measured demand this app answers is 36 — add the token to this list with the op", len(seen))
	}
}

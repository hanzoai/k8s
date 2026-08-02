// Package ops is the /v1/k8s surface: one named, typed operation per thing this
// deployment may do to a Kubernetes cluster, and nothing else.
//
// # Every op names its cluster, and Reach is the only door
//
// Not one handler here constructs a Kubernetes client. Each one calls
// registry.Registry.Reach with the cluster the caller named, and Reach is the
// module's only client constructor — so "which cluster" cannot be omitted, cannot
// be defaulted, and cannot be inherited from the pod this process happens to run
// in. hanzo-k8s, lux-k8s, zoo-k8s and a customer's own cluster are four rows in a
// registry and one code path.
//
// # There is no generic passthrough, and that is the point
//
// Every operation is NAMED and TYPED, including the custom-resource quartet, whose
// kinds come from the closed table in resources.go. An "apply this YAML" op would
// have been shorter to write and would have relocated the credential problem
// behind an RPC instead of solving it: with one, the union of privilege this app
// needs cannot be written down, so nothing can be least-privileged and no reviewer
// can bound a blast radius. With this set, the union RBAC is a finite list and it
// is in the repo's LLM.md.
//
// # Every op is registered twice, from one handler
//
// Once on the app — the world's HTTP surface, which projects OpenAPI, an MCP tool
// and a CLI command — and once on app.Peer(), the fleet's socket, where a peer
// invokes it by op token through zip.Ask. One handler, one token, two transports.
// The peer app listens only on this app's 0600 socket, so an internal op is never
// served on the edge; that separation is structural rather than a check that could
// be forgotten.
//
// The registrations are written out as direct zip.Get / zip.Post calls with literal
// paths, deliberately and not by a helper: zipdoc reads the AST for exactly that
// shape, so a registration wrapped in a loop or a generic helper is a registration
// whose doc comment reaches NEITHER the OpenAPI description NOR the MCP tool
// description. The prose below each handler is the product surface an agent reads
// when deciding whether to call the op, so the shape that carries it wins over the
// shape that saves lines.
package ops

//go:generate go run github.com/zap-proto/zip/cmd/zipdoc

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/plane"
	"github.com/hanzoai/k8s/registry"
)

// Ops is the service: the registry, and nothing else. Every fact an op needs
// beyond its argument comes through Reach, so there is no second source of
// cluster state to keep in step.
type Ops struct {
	Reg *registry.Registry
}

// Mount registers every operation on both surfaces.
//
// The order is the order the API documentation reads in, not a routing order:
// zip's router decides by specificity, so mount sequence carries no meaning and a
// reader is free to find an op where they expect it.
func (o Ops) Mount(app *zip.App) {
	p := app.Peer()

	// ---- clusters — the registry ------------------------------------------
	zip.Get(app, "/v1/k8s/clusters", o.listClusters,
		zip.WithOperationID(plane.ClustersList), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/clusters/list", o.listClusters,
		zip.WithOperationID(plane.ClustersList))

	zip.Post(app, "/v1/k8s/clusters", o.registerCluster,
		zip.WithOperationID(plane.ClustersRegister), zip.WithTags("k8s"), zip.WithStatus(201))
	zip.Post(p, "/k8s/clusters/register", o.registerCluster,
		zip.WithOperationID(plane.ClustersRegister))

	zip.Get(app, "/v1/k8s/clusters/:cluster", o.getCluster,
		zip.WithOperationID(plane.ClustersGet), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/clusters/get", o.getCluster,
		zip.WithOperationID(plane.ClustersGet))

	zip.Delete(app, "/v1/k8s/clusters/:cluster", o.deregisterCluster,
		zip.WithOperationID(plane.ClustersDeregister), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/clusters/deregister", o.deregisterCluster,
		zip.WithOperationID(plane.ClustersDeregister))

	zip.Get(app, "/v1/k8s/clusters/:cluster/status", o.clusterStatus,
		zip.WithOperationID(plane.ClustersStatus), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/clusters/status", o.clusterStatus,
		zip.WithOperationID(plane.ClustersStatus))

	// ---- nodes -------------------------------------------------------------
	zip.Get(app, "/v1/k8s/nodes", o.listNodes,
		zip.WithOperationID(plane.NodesList), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/nodes/list", o.listNodes,
		zip.WithOperationID(plane.NodesList))

	zip.Get(app, "/v1/k8s/nodes/volume-stats", o.nodeVolumeStats,
		zip.WithOperationID(plane.NodesVolumeStats), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/nodes/volume-stats", o.nodeVolumeStats,
		zip.WithOperationID(plane.NodesVolumeStats))

	zip.Post(app, "/v1/k8s/nodes/:node/cordon", o.cordonNode,
		zip.WithOperationID(plane.NodesCordon), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/nodes/cordon", o.cordonNode,
		zip.WithOperationID(plane.NodesCordon))

	// ---- namespaces --------------------------------------------------------
	zip.Get(app, "/v1/k8s/namespaces", o.listNamespaces,
		zip.WithOperationID(plane.NamespacesList), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/namespaces/list", o.listNamespaces,
		zip.WithOperationID(plane.NamespacesList))

	zip.Post(app, "/v1/k8s/namespaces", o.ensureNamespace,
		zip.WithOperationID(plane.NamespacesEnsure), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/namespaces/ensure", o.ensureNamespace,
		zip.WithOperationID(plane.NamespacesEnsure))

	zip.Get(app, "/v1/k8s/namespaces/:namespace", o.getNamespace,
		zip.WithOperationID(plane.NamespacesGet), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/namespaces/get", o.getNamespace,
		zip.WithOperationID(plane.NamespacesGet))

	zip.Delete(app, "/v1/k8s/namespaces/:namespace", o.deleteNamespace,
		zip.WithOperationID(plane.NamespacesDelete), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/namespaces/delete", o.deleteNamespace,
		zip.WithOperationID(plane.NamespacesDelete))

	// ---- pods --------------------------------------------------------------
	zip.Get(app, "/v1/k8s/pods", o.listPods,
		zip.WithOperationID(plane.PodsList), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/pods/list", o.listPods,
		zip.WithOperationID(plane.PodsList))

	zip.Get(app, "/v1/k8s/pods/:namespace/:pod/logs", o.podLogs,
		zip.WithOperationID(plane.PodsLogs), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/pods/logs", o.podLogs,
		zip.WithOperationID(plane.PodsLogs))

	// ---- volumes -----------------------------------------------------------
	zip.Get(app, "/v1/k8s/volumes/claims", o.listClaims,
		zip.WithOperationID(plane.VolumesListClaims), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/volumes/list-claims", o.listClaims,
		zip.WithOperationID(plane.VolumesListClaims))

	zip.Get(app, "/v1/k8s/volumes/persistent", o.listPersistentVolumes,
		zip.WithOperationID(plane.VolumesListPersistent), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/volumes/list-persistent", o.listPersistentVolumes,
		zip.WithOperationID(plane.VolumesListPersistent))

	zip.Post(app, "/v1/k8s/volumes/claims", o.ensureClaim,
		zip.WithOperationID(plane.VolumesEnsureClaim), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/volumes/ensure-claim", o.ensureClaim,
		zip.WithOperationID(plane.VolumesEnsureClaim))

	zip.Post(app, "/v1/k8s/volumes/claims/:namespace/:name/expand", o.expandClaim,
		zip.WithOperationID(plane.VolumesExpandClaim), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/volumes/expand-claim", o.expandClaim,
		zip.WithOperationID(plane.VolumesExpandClaim))

	// ---- workloads ---------------------------------------------------------
	zip.Get(app, "/v1/k8s/workloads/deployments", o.listDeployments,
		zip.WithOperationID(plane.WorkloadsListDeployments), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/list-deployments", o.listDeployments,
		zip.WithOperationID(plane.WorkloadsListDeployments))

	zip.Get(app, "/v1/k8s/workloads/deployments/:namespace/:name", o.getDeployment,
		zip.WithOperationID(plane.WorkloadsGetDeployment), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/get-deployment", o.getDeployment,
		zip.WithOperationID(plane.WorkloadsGetDeployment))

	zip.Get(app, "/v1/k8s/workloads/replicasets", o.listReplicaSets,
		zip.WithOperationID(plane.WorkloadsListReplicaSets), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/list-replicasets", o.listReplicaSets,
		zip.WithOperationID(plane.WorkloadsListReplicaSets))

	zip.Get(app, "/v1/k8s/workloads/statefulsets", o.listStatefulSets,
		zip.WithOperationID(plane.WorkloadsListStatefulSets), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/list-statefulsets", o.listStatefulSets,
		zip.WithOperationID(plane.WorkloadsListStatefulSets))

	zip.Get(app, "/v1/k8s/workloads/services", o.listServices,
		zip.WithOperationID(plane.WorkloadsListServices), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/list-services", o.listServices,
		zip.WithOperationID(plane.WorkloadsListServices))

	zip.Get(app, "/v1/k8s/workloads/ingresses", o.listIngresses,
		zip.WithOperationID(plane.WorkloadsListIngresses), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/list-ingresses", o.listIngresses,
		zip.WithOperationID(plane.WorkloadsListIngresses))

	zip.Get(app, "/v1/k8s/workloads/events", o.listEvents,
		zip.WithOperationID(plane.WorkloadsListEvents), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/list-events", o.listEvents,
		zip.WithOperationID(plane.WorkloadsListEvents))

	zip.Get(app, "/v1/k8s/workloads/configmaps", o.listConfigMaps,
		zip.WithOperationID(plane.WorkloadsListConfigMaps), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/workloads/list-configmaps", o.listConfigMaps,
		zip.WithOperationID(plane.WorkloadsListConfigMaps))

	// ---- jobs --------------------------------------------------------------
	zip.Post(app, "/v1/k8s/jobs", o.createJob,
		zip.WithOperationID(plane.JobsCreate), zip.WithTags("k8s"), zip.WithStatus(201))
	zip.Post(p, "/k8s/jobs/create", o.createJob,
		zip.WithOperationID(plane.JobsCreate))

	zip.Get(app, "/v1/k8s/jobs", o.listJobs,
		zip.WithOperationID(plane.JobsList), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/jobs/list", o.listJobs,
		zip.WithOperationID(plane.JobsList))

	zip.Get(app, "/v1/k8s/jobs/:namespace/:name", o.getJob,
		zip.WithOperationID(plane.JobsGet), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/jobs/get", o.getJob,
		zip.WithOperationID(plane.JobsGet))

	// ---- resources — the closed kind set -----------------------------------
	zip.Get(app, "/v1/k8s/resources", o.listResources,
		zip.WithOperationID(plane.ResourcesList), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/resources/list", o.listResources,
		zip.WithOperationID(plane.ResourcesList))

	zip.Get(app, "/v1/k8s/resources/watch", o.watchResources,
		zip.WithOperationID(plane.ResourcesWatch), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/resources/watch", o.watchResources,
		zip.WithOperationID(plane.ResourcesWatch))

	zip.Put(app, "/v1/k8s/resources", o.applyResource,
		zip.WithOperationID(plane.ResourcesApply), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/resources/apply", o.applyResource,
		zip.WithOperationID(plane.ResourcesApply))

	zip.Get(app, "/v1/k8s/resources/:kind/:namespace/:name", o.getResource,
		zip.WithOperationID(plane.ResourcesGet), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/resources/get", o.getResource,
		zip.WithOperationID(plane.ResourcesGet))

	zip.Delete(app, "/v1/k8s/resources/:kind/:namespace/:name", o.deleteResource,
		zip.WithOperationID(plane.ResourcesDelete), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/resources/delete", o.deleteResource,
		zip.WithOperationID(plane.ResourcesDelete))

	// ---- access ------------------------------------------------------------
	zip.Post(app, "/v1/k8s/access/can", o.can,
		zip.WithOperationID(plane.AccessCan), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/access/can", o.can,
		zip.WithOperationID(plane.AccessCan))

	// ---- metrics -----------------------------------------------------------
	zip.Get(app, "/v1/k8s/metrics/pods", o.podMetrics,
		zip.WithOperationID(plane.MetricsPods), zip.WithTags("k8s"))
	zip.Post(p, "/k8s/metrics/pods", o.podMetrics,
		zip.WithOperationID(plane.MetricsPods))
}

// reach resolves a cluster for the caller and maps a refusal onto its HTTP
// status. It is the one line every handler starts with, and the one place a
// registry refusal becomes a wire answer.
func (o Ops) reach(ctx context.Context, cluster string) (*registry.Bound, error) {
	b, err := o.Reg.Reach(ctx, cluster)
	if err != nil {
		return nil, fail(err)
	}
	return b, nil
}

// fail maps an error onto the status a caller should see.
//
// The mapping is deliberate about ONE thing above all: a cluster another org
// registered answers 404, never 403. A "forbidden" would confirm the name exists
// and turn the refusal into an oracle for enumerating other tenants' clusters.
// A namespace outside a registration's bound DOES answer 403, because the caller
// already knows that namespace name — it typed it — so there is nothing to leak
// and everything to explain.
func fail(err error) error {
	if err == nil {
		return nil
	}
	var noTenant registry.ErrNoTenant
	if errors.As(err, &noTenant) {
		return zip.ErrForbidden(err.Error())
	}
	var noCluster registry.ErrNoCluster
	if errors.As(err, &noCluster) {
		return zip.ErrNotFound(err.Error())
	}
	var badNS registry.ErrNamespace
	if errors.As(err, &badNS) {
		return zip.ErrForbidden(err.Error())
	}
	var badScope registry.ErrScope
	if errors.As(err, &badScope) {
		return zip.ErrForbidden(err.Error())
	}
	var badName registry.ErrName
	if errors.As(err, &badName) {
		return zip.ErrBadRequest(err.Error())
	}
	// An apiserver's own verdict is surfaced with its own status and its own
	// message. A RBAC refusal read as an internal error would send an operator
	// hunting a bug in this app instead of granting the verb it named.
	switch {
	case apierrors.IsNotFound(err):
		return zip.ErrNotFound(err.Error())
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return zip.ErrForbidden(err.Error())
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return zip.ErrConflict(err.Error())
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return zip.ErrBadRequest(err.Error())
	}
	var he *zip.HTTPError
	if errors.As(err, &he) {
		return he
	}
	return zip.ErrInternal(err.Error())
}

// need refuses an empty required argument. zip's `validate:"required"` covers the
// HTTP body; a peer calling over ZAP hands the struct straight in, so the check
// belongs in the handler as well as in the tag.
func need(field, value string) error {
	if value == "" {
		return zip.ErrBadRequest(fmt.Sprintf("%q is required", field))
	}
	return nil
}

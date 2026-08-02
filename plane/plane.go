// Package plane is this app's call contract: the name of every op a peer may
// invoke on it, and the input and output each one carries.
//
// It is a LEAF — stdlib only. Both halves of a call import it, so the two halves
// cannot drift, and a consumer that wants to ASK about a cluster links this
// package and nothing else: no client-go, no kubeconfig parser, no credential.
// That is the whole point of the split. `go list -deps ./plane` is the proof.
//
// # The op token is the operation's one identity
//
// Each constant below is simultaneously the OpenAPI operationId, the MCP tool
// name, the CLI command and the plane op. One string, four projections, spelled
// once here so a caller and a callee cannot disagree at run time about a name
// the compiler never compared.
//
// # Every op NAMES its cluster
//
// Every In type that touches an apiserver carries a `Cluster` field, because there
// is no ambient cluster in this program: lux runs lux-k8s, zoo runs zoo-k8s, a
// customer runs their own, and "the cluster" is not a fact any function may
// assume. A function that can be called without saying which cluster would be a
// function that reads whichever one the process happened to be started next to.
//
// # There is no Org field in this file
//
// The tenant a call acts for rides the CALLER — forwarded from the gateway's
// assertion with zip.Ctx.Forward, or stated once by a background job with
// zip.WithCaller — and the callee re-derives it with zip.Tenant. An org in the
// argument is an org the caller CHOSE, and a caller that can name the org can
// read another tenant's cluster. `Org` appears only on REPLIES, where it is a
// property of the thing described rather than a claim by the asker.
//
// # There is no namespace default either
//
// A namespaced op states its namespace, and the registry row for the cluster
// bounds which namespaces that registration may address (registry.Cluster's
// Namespaces). A tenant naming a namespace outside its bound is refused before
// the apiserver is dialed, and refused again by the credential's own RBAC.
//
// # The wire is ZAP; the tags are the document's vocabulary
//
// These types cross as ZAP messages: a field IS its offset and no name travels.
// The compatibility rule is therefore structural — APPEND FIELDS AT THE END, and
// only at the end. Reordering, inserting or retyping one changes what every
// existing peer reads.
package plane

// The op names.
//
// Read as a set they are the whole Kubernetes authority this deployment holds:
// nine reads and a bounded list of mutations, each one named so its blast radius
// is auditable. There is deliberately no "run any k8s call" op — a generic
// passthrough relocates the credential problem behind an RPC instead of solving
// it, and the union RBAC in the repo's LLM.md could not then be written at all.
const (
	// clusters — the registry, this app's first-class concept.
	ClustersList       = "k8s_clusters_list"
	ClustersGet        = "k8s_clusters_get"
	ClustersRegister   = "k8s_clusters_register"
	ClustersDeregister = "k8s_clusters_deregister"
	ClustersStatus     = "k8s_clusters_status"

	// nodes
	NodesList        = "k8s_nodes_list"
	NodesCordon      = "k8s_nodes_cordon"
	NodesVolumeStats = "k8s_nodes_volume_stats"

	// namespaces
	NamespacesList   = "k8s_namespaces_list"
	NamespacesGet    = "k8s_namespaces_get"
	NamespacesEnsure = "k8s_namespaces_ensure"
	NamespacesDelete = "k8s_namespaces_delete"

	// pods
	PodsList = "k8s_pods_list"
	PodsLogs = "k8s_pods_logs"

	// volumes — PersistentVolume and PersistentVolumeClaim.
	VolumesListClaims     = "k8s_volumes_list_claims"
	VolumesListPersistent = "k8s_volumes_list_persistent"
	VolumesEnsureClaim    = "k8s_volumes_ensure_claim"
	VolumesExpandClaim    = "k8s_volumes_expand_claim"

	// workloads — read-only projections.
	WorkloadsListDeployments  = "k8s_workloads_list_deployments"
	WorkloadsGetDeployment    = "k8s_workloads_get_deployment"
	WorkloadsListReplicaSets  = "k8s_workloads_list_replicasets"
	WorkloadsListStatefulSets = "k8s_workloads_list_statefulsets"
	WorkloadsListServices     = "k8s_workloads_list_services"
	WorkloadsListIngresses    = "k8s_workloads_list_ingresses"
	WorkloadsListEvents       = "k8s_workloads_list_events"
	WorkloadsListConfigMaps   = "k8s_workloads_list_configmaps"

	// jobs
	JobsCreate = "k8s_jobs_create"
	JobsList   = "k8s_jobs_list"
	JobsGet    = "k8s_jobs_get"

	// resources — the CLOSED custom-resource kind set.
	ResourcesList   = "k8s_resources_list"
	ResourcesGet    = "k8s_resources_get"
	ResourcesWatch  = "k8s_resources_watch"
	ResourcesApply  = "k8s_resources_apply"
	ResourcesDelete = "k8s_resources_delete"

	// access
	AccessCan = "k8s_access_can"

	// metrics
	MetricsPods = "k8s_metrics_pods"
)

// KMS is the ONE peer this app calls, and the two op tokens it must spell to
// reach it. They belong to the KMS app and are declared identically in cloud's
// own plane package; when HIP-0106's `github.com/hanzoai/plane` extraction lands
// (migration step 2), both spellings collapse into that module and these three
// declarations are deleted in the same change. Until then this is the ONE place
// in this repo that names them, so there is exactly one line to move.
const (
	KMS    = "kms"
	KMSGet = "kms_get"
	KMSPut = "kms_put"
)

// SecretIn names one secret in KMS and carries its value on a write. It is
// byte-compatible with the KMS app's own SecretIn, which is what makes the call
// work: a ZAP field is its offset, so the two declarations must stay structurally
// identical, and appending to either without the other is the break.
type SecretIn struct {
	Ref   string `json:"ref"`
	Value []byte `json:"value,omitempty"`
}

// Secret is one secret's value as KMS returns it.
type Secret struct {
	Value []byte `json:"value,omitempty"`
}

// NoArgs is the input of an op that takes none. The registry read it serves is
// scoped by the CALLER and by nothing the caller could type.
type NoArgs struct{}

// ClusterRef names one registered cluster and nothing else.
//
// Every In type below carries a `Cluster` field of its own rather than embedding
// this one. That is not repetition for its own sake: zip binds a URL value onto
// the TOP LEVEL of an input only, so a cluster name promoted through an embedded
// struct would never bind from `?cluster=` or from a `:cluster` path segment, and
// every op would silently read the empty cluster. The field is spelled per type
// so that the wire actually carries it.
type ClusterRef struct {
	// Cluster is the registered cluster's name — "hanzo-k8s", "lux-k8s",
	// "zoo-k8s", "bootnode-k8s", or any name an org registered its own cluster
	// under. It is resolved against the CALLING ORG's registry, so two orgs may
	// use the same name for two different clusters and neither can reach the
	// other's.
	Cluster string `json:"cluster" validate:"required"`
}

// ---- clusters --------------------------------------------------------------

// Registered is one registered cluster as the registry holds it. The kubeconfig
// is NOT here and has no field: it is sealed in KMS under a ref this app derives
// from the owner and the name, and it is resolved at use and never returned.
type Registered struct {
	Name string `json:"name"`
	// Org is which org OWNS this cluster. On a reply that is a property of the
	// record, not a claim by the caller.
	Org       string `json:"org"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint,omitempty"`
	Nodes     int    `json:"nodes"`
	NvidiaGPU int    `json:"nvidiaGpu"`
	AmdGPU    int    `json:"amdGpu"`
	// Namespaces bounds what this registration may address. Empty means the
	// whole cluster, which is what an org registering its OWN cluster gets.
	Namespaces []string `json:"namespaces,omitempty"`
	Registered string   `json:"registered"`
	Default    bool     `json:"default"`
}

// Clusters is every cluster the calling org may reach.
type Clusters struct {
	Clusters []Registered `json:"clusters"`
}

// RegisterIn attaches a cluster to the calling org's registry.
type RegisterIn struct {
	// Name is what every later op will call this cluster. Unique within the org.
	Name string `json:"name" validate:"required"`
	// Kubeconfig is the credential, in kubeconfig form. It is validated, used
	// once to prove the cluster is reachable, sealed in KMS, and then dropped:
	// it is never written to disk, never logged and never returned.
	Kubeconfig string `json:"kubeconfig" validate:"required"`
	// Provider labels where the cluster came from ("doks", "byo", "k3s", …).
	Provider string `json:"provider,omitempty"`
	// Namespaces bounds this registration to a namespace set. Omit it when the
	// org owns the whole cluster; state it when the credential is a scoped
	// ServiceAccount on a shared cluster.
	Namespaces []string `json:"namespaces,omitempty"`
	// Default marks the cluster an org's workloads target when none is named by
	// a human — the ONE place a default is legitimate, because it is the org's
	// own recorded choice rather than a process's ambient neighbour.
	Default bool `json:"default,omitempty"`
}

// Deregistered reports whether a name was present to remove.
type Deregistered struct {
	Removed bool `json:"removed"`
}

// Reachable is a cluster's liveness as of this call.
type Reachable struct {
	// Connected is whether the apiserver answered.
	Connected bool `json:"connected"`
	// Version is the server version string, when it answered.
	Version string `json:"version,omitempty"`
	// Reason is why it did not, verbatim. Absent on success.
	Reason string `json:"reason,omitempty"`
}

// ---- nodes -----------------------------------------------------------------

// Node is one worker node, projected to what the boards read: is it up, will it
// take work, and what accelerators does it offer.
type Node struct {
	Name        string `json:"name"`
	Ready       bool   `json:"ready"`
	Schedulable bool   `json:"schedulable"`
	Region      string `json:"region,omitempty"`
	InstanceKind string `json:"instanceKind,omitempty"`
	CPU          string `json:"cpu,omitempty"`
	Memory       string `json:"memory,omitempty"`
	NvidiaGPU    int    `json:"nvidiaGpu"`
	AmdGPU       int    `json:"amdGpu"`
	InternalIP   string `json:"internalIp,omitempty"`
}

// Nodes is a cluster's worker inventory.
type Nodes struct {
	Nodes []Node `json:"nodes"`
}

// CordonIn takes a node in or out of service.
type CordonIn struct {
	// Cluster is the registered cluster the node belongs to.
	Cluster string `json:"cluster" validate:"required"`
	// Node is the node's name.
	Node string `json:"node" validate:"required"`
	// Schedulable is the state to leave the node in: false cordons it, true
	// uncordons it.
	Schedulable bool `json:"schedulable"`
	// Drain evicts the node's pods after cordoning. Ignored when Schedulable is
	// true, because draining a node you just returned to service is incoherent.
	Drain bool `json:"drain,omitempty"`
}

// Cordoned reports how many pods the drain actually evicted.
type Cordoned struct {
	Evicted int `json:"evicted"`
}

// VolumeUse is one mounted claim's consumption, as its kubelet reports it.
type VolumeUse struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UsedBytes int64  `json:"usedBytes"`
}

// VolumeStats is every mounted claim's fill on the cluster, plus how much of the
// fleet answered. Coverage is load-bearing: a claim with no row is UNMEASURED,
// which is a different fact from empty, and a consumer that treats the two alike
// will condemn live data.
type VolumeStats struct {
	Volumes []VolumeUse `json:"volumes"`
	// NodesRead is how many kubelets answered; NodesTotal how many were asked.
	NodesRead  int `json:"nodesRead"`
	NodesTotal int `json:"nodesTotal"`
}

// ---- namespaces ------------------------------------------------------------

// Namespace is one namespace, projected.
type Namespace struct {
	Name   string `json:"name"`
	Phase  string `json:"phase,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Namespaces is the namespaces this registration may address.
type Namespaces struct {
	Namespaces []Namespace `json:"namespaces"`
}

// NamespaceRef addresses one namespace on one cluster.
type NamespaceRef struct {
	// Cluster is the registered cluster the namespace lives on.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace is the namespace's name.
	Namespace string `json:"namespace" validate:"required"`
}

// EnsureNamespaceIn creates a namespace if it is absent.
//
// It replaces four separate get-then-create-if-missing implementations with one
// name. Idempotence is the whole point: a caller that has to write the ladder
// itself writes a race, because "get says absent" and "create" are two calls.
type EnsureNamespaceIn struct {
	// Cluster is the registered cluster to create the namespace on.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace is the namespace's name.
	Namespace string `json:"namespace" validate:"required"`
	// Labels are merged onto the namespace when it is created.
	Labels map[string]string `json:"labels,omitempty"`
}

// Ensured reports whether the act created the object or found it already there.
// Two different facts, so two different answers: an idempotent call that cannot
// say which one it did leaves the caller unable to log a creation.
type Ensured struct {
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

// Deleted reports whether the object was present to delete.
type Deleted struct {
	Removed bool `json:"removed"`
}

// ---- pods ------------------------------------------------------------------

// PodsIn selects pods on one cluster.
type PodsIn struct {
	// Cluster is the registered cluster to read.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace narrows to one namespace. Empty means every namespace this
	// registration may address, which for a bounded registration is its own set
	// and never the whole cluster.
	Namespace string `json:"namespace,omitempty"`
	// LabelSelector is a Kubernetes label selector, verbatim.
	LabelSelector string `json:"labelSelector,omitempty"`
	// NodeName narrows to the pods on one node — the field-selector variant the
	// drain path needs, named rather than left to a raw selector string.
	NodeName string `json:"nodeName,omitempty"`
}

// Pod is one pod, projected to placement, health, what it mounts and what it
// runs. Container ENV is deliberately absent: a pod spec carries injected
// secret material, and a read surface that returns it is an exfiltration door.
type Pod struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Phase      string   `json:"phase"`
	Reason     string   `json:"reason,omitempty"`
	Node       string   `json:"node,omitempty"`
	Controller string   `json:"controller,omitempty"`
	Claims     []string `json:"claims,omitempty"`
	Images     []string `json:"images,omitempty"`
	Restarts   int32    `json:"restarts"`
}

// Pods is a page of pods.
type Pods struct {
	Pods []Pod `json:"pods"`
}

// LogsIn reads one container's log.
type LogsIn struct {
	// Cluster is the registered cluster the pod runs on.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace is the pod's namespace.
	Namespace string `json:"namespace" validate:"required"`
	// Pod is the pod's name.
	Pod string `json:"pod" validate:"required"`
	// Container names the container; empty takes the pod's only one.
	Container string `json:"container,omitempty"`
	// TailLines caps how many lines come back, newest last. Zero takes the
	// server's default. A log read is unbounded by nature, so this is the bound.
	TailLines int64 `json:"tailLines,omitempty"`
}

// Logs is what the container printed.
type Logs struct {
	Log string `json:"log"`
}

// ---- volumes ---------------------------------------------------------------

// Claim is one PersistentVolumeClaim, projected.
type Claim struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Volume    string `json:"volume,omitempty"`
	// RequestGi is the size the claim asks for, in GiB.
	RequestGi    int    `json:"requestGi"`
	StorageClass string `json:"storageClass,omitempty"`
}

// Claims is a page of claims.
type Claims struct {
	Claims []Claim `json:"claims"`
}

// Volume is one PersistentVolume, projected to the identities by which it can be
// matched to the cloud device behind it.
type Volume struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	// Handle is the backing device's provider id, when the PV carries one.
	Handle    string `json:"handle,omitempty"`
	ClaimNS   string `json:"claimNamespace,omitempty"`
	ClaimName string `json:"claimName,omitempty"`
	CapacityGi int   `json:"capacityGi"`
}

// Volumes is every PersistentVolume on the cluster. PVs are cluster-scoped, so
// this op is only served to a registration that addresses the whole cluster.
type Volumes struct {
	Volumes []Volume `json:"volumes"`
}

// EnsureClaimIn creates a claim if it is absent.
type EnsureClaimIn struct {
	// Cluster is the registered cluster to create the claim on.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace is where the claim lives.
	Namespace string `json:"namespace" validate:"required"`
	// Name is the claim's name.
	Name string `json:"name" validate:"required"`
	// Gi is the storage to request, in GiB.
	Gi int `json:"gi" validate:"required"`
	// StorageClass names the class; empty takes the cluster's default.
	StorageClass string `json:"storageClass,omitempty"`
	// AccessMode is "ReadWriteOnce" (default) or "ReadWriteMany".
	AccessMode string `json:"accessMode,omitempty"`
}

// ExpandClaimIn grows a claim. There is no shrink and no delete: a claim holds
// the only copy of a tenant's data, so this app can make one bigger and nothing
// more.
type ExpandClaimIn struct {
	// Cluster is the registered cluster the claim lives on.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace is the claim's namespace.
	Namespace string `json:"namespace" validate:"required"`
	// Name is the claim's name.
	Name string `json:"name" validate:"required"`
	// Gi is the new size in GiB. It must exceed the current request; the
	// apiserver refuses a shrink and its message is surfaced verbatim.
	Gi int `json:"gi" validate:"required"`
}

// Expanded reports the size the claim now asks for.
type Expanded struct {
	Gi int `json:"gi"`
}

// ---- workloads -------------------------------------------------------------

// Selector narrows a namespaced read.
type Selector struct {
	// Cluster is the registered cluster to read.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace narrows to one namespace; empty means every namespace this
	// registration may address.
	Namespace string `json:"namespace,omitempty"`
	// LabelSelector is a Kubernetes label selector, verbatim.
	LabelSelector string `json:"labelSelector,omitempty"`
}

// Workload is one Deployment, ReplicaSet or StatefulSet, projected to the
// question every board asks: how many replicas are up, and what image is
// actually running. The RUNNING image is why this op exists — an App CR's status
// does not surface it, so the live workload is the only source.
type Workload struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Replicas  int32    `json:"replicas"`
	Ready     int32    `json:"ready"`
	Updated   int32    `json:"updated"`
	Images    []string `json:"images,omitempty"`
	Owner     string   `json:"owner,omitempty"`
}

// Workloads is a page of workloads.
type Workloads struct {
	Workloads []Workload `json:"workloads"`
}

// NamedIn addresses one namespaced object by name.
type NamedIn struct {
	// Cluster is the registered cluster the object lives on.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace is the object's namespace.
	Namespace string `json:"namespace" validate:"required"`
	// Name is the object's name.
	Name string `json:"name" validate:"required"`
}

// Service is one Service, projected to every identity by which it can be matched
// to the load balancer in front of it.
type Service struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	ClusterIP string   `json:"clusterIp,omitempty"`
	IPs       []string `json:"ips,omitempty"`
	// LBID is the provider's load-balancer id, from the cloud-controller's own
	// annotation — the strongest link between a Service and a real balancer.
	LBID  string  `json:"lbId,omitempty"`
	Ports []int32 `json:"ports,omitempty"`
}

// Services is a page of services.
type Services struct {
	Services []Service `json:"services"`
}

// Ingress is one Ingress, projected to the hosts it answers and whether each is
// covered by TLS.
type Ingress struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Hosts     []string `json:"hosts,omitempty"`
	TLSHosts  []string `json:"tlsHosts,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

// Ingresses is a page of ingresses.
type Ingresses struct {
	Ingresses []Ingress `json:"ingresses"`
}

// Event is one cluster event, projected. It is the only place a failure REASON
// for a workload that never produced a pod can be read.
type Event struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	Object    string `json:"object,omitempty"`
	Count     int32  `json:"count"`
	LastSeen  int64  `json:"lastSeen"`
}

// Events is a page of events, newest last.
type Events struct {
	Events []Event `json:"events"`
}

// ConfigMap is one ConfigMap. Data travels because a ConfigMap is by definition
// not secret material — a Secret is a different kind and this app does not read
// one back (see the repo's LLM.md on why).
type ConfigMap struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Data      map[string]string `json:"data,omitempty"`
}

// ConfigMaps is a page of config maps.
type ConfigMaps struct {
	ConfigMaps []ConfigMap `json:"configMaps"`
}

// ---- jobs ------------------------------------------------------------------

// JobIn creates one batch Job.
type JobIn struct {
	// Cluster is the registered cluster to run the job on.
	Cluster string `json:"cluster" validate:"required"`
	// Namespace is where the job runs.
	Namespace string `json:"namespace" validate:"required"`
	// Name is the job's name.
	Name string `json:"name" validate:"required"`
	// Image is the container image to run.
	Image string `json:"image" validate:"required"`
	// Command replaces the image's entrypoint. Empty keeps it.
	Command []string `json:"command,omitempty"`
	// Args are the arguments passed to the entrypoint.
	Args []string `json:"args,omitempty"`
	// Env is plain environment. Secret material does NOT belong here: seal it in
	// KMS and project it with a scoped ServiceAccount instead.
	Env map[string]string `json:"env,omitempty"`
	// ServiceAccount runs the pod as a named ServiceAccount.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Labels are stamped on the Job and its pods.
	Labels map[string]string `json:"labels,omitempty"`
	// BackoffLimit caps retries; zero means the Kubernetes default.
	BackoffLimit int32 `json:"backoffLimit,omitempty"`
	// TTLSeconds deletes the finished Job after this long. Zero leaves it, which
	// on a busy namespace is how a Job list becomes unreadable.
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`
	// Mounts attaches existing claims by name at a path.
	Mounts []Mount `json:"mounts,omitempty"`
}

// Mount attaches one existing PersistentVolumeClaim into a job's container.
type Mount struct {
	Claim string `json:"claim"`
	Path  string `json:"path"`
}

// Job is one batch Job, projected to whether it finished and how it went.
type Job struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Active    int32  `json:"active"`
	Succeeded int32  `json:"succeeded"`
	Failed    int32  `json:"failed"`
	// Phase is "running", "succeeded" or "failed" — the one word a caller wants,
	// rolled here rather than re-derived from three counters by every consumer.
	Phase     string `json:"phase"`
	StartedAt int64  `json:"startedAt,omitempty"`
	EndedAt   int64  `json:"endedAt,omitempty"`
}

// Jobs is a page of jobs.
type Jobs struct {
	Jobs []Job `json:"jobs"`
}

// ---- resources — the closed CR kind set ------------------------------------

// ResourceIn addresses custom resources of ONE registered kind.
//
// Kind is not free text: it is matched against this app's own allowlist, and a
// kind the allowlist does not carry is refused. Adding a kind is a code change
// here, which is exactly what keeps the set bounded and auditable — and is the
// difference between this op and the apply-arbitrary-YAML function it replaces.
type ResourceIn struct {
	// Cluster is the registered cluster to read.
	Cluster string `json:"cluster" validate:"required"`
	// Kind is the registered kind: "App", "Application", "Datastore",
	// "Validator", "TrainJob", "InferenceService", "IngressRoute", "Middleware"
	// or "AppProject".
	Kind string `json:"kind" validate:"required"`
	// Namespace narrows to one namespace; empty means every namespace this
	// registration may address. Ignored for a cluster-scoped kind.
	Namespace string `json:"namespace,omitempty"`
	// LabelSelector is a Kubernetes label selector, verbatim.
	LabelSelector string `json:"labelSelector,omitempty"`
}

// ResourceRef addresses ONE custom resource by kind, namespace and name.
type ResourceRef struct {
	// Cluster is the registered cluster the object lives on.
	Cluster string `json:"cluster" validate:"required"`
	// Kind is the registered kind.
	Kind string `json:"kind" validate:"required"`
	// Namespace is the object's namespace; empty for a cluster-scoped kind.
	Namespace string `json:"namespace,omitempty"`
	// Name is the object's name.
	Name string `json:"name" validate:"required"`
}

// Resource is one custom resource: its coordinates and its whole body.
//
// Spec and Status travel as maps because their SHAPE is the CR author's domain,
// not this app's — projecting them here would make this app the schema owner of
// every kind it carries, and it would have to be edited whenever any of them
// grew a field. What this app owns is the KIND SET and the VERBS. That is the
// line between a bounded surface and a passthrough: the resource and the verb
// are named and typed, and the body is domain payload.
type Resource struct {
	Kind      string         `json:"kind"`
	Namespace string         `json:"namespace,omitempty"`
	Name      string         `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Spec      map[string]any `json:"spec,omitempty"`
	Status    map[string]any `json:"status,omitempty"`
	// Version is the object's resourceVersion — the cursor a Watch resumes from.
	Version string `json:"version,omitempty"`
}

// Resources is a page of custom resources, plus the cursor a Watch resumes from.
type Resources struct {
	Resources []Resource `json:"resources"`
	Version   string     `json:"version,omitempty"`
}

// ApplyIn creates one custom resource or patches the one already there.
//
// It replaces five separate get-then-create-else-patch ladders with one name.
// The Spec is the caller's domain payload; the KIND is checked against the
// allowlist, so this cannot become "apply any object the cluster has".
type ApplyIn struct {
	// Cluster is the registered cluster to apply to.
	Cluster string `json:"cluster" validate:"required"`
	// Kind is the registered kind.
	Kind string `json:"kind" validate:"required"`
	// Namespace is where the object lives; empty for a cluster-scoped kind.
	Namespace string `json:"namespace,omitempty"`
	// Name is the object's name.
	Name string `json:"name" validate:"required"`
	// Labels are merged onto the object.
	Labels map[string]string `json:"labels,omitempty"`
	// Spec is the desired spec, applied whole.
	Spec map[string]any `json:"spec" validate:"required"`
}

// WatchIn asks what changed since a cursor.
//
// It is a BOUNDED read, not a stream: it returns the events that arrived within
// WaitSeconds and the cursor to resume from. A stream cannot cross the call
// plane as a value, and rendering one — Server-Sent Events, a websocket — is the
// EDGE's concern. The consumer composes its own stream out of these answers,
// which is why the streaming shape is not duplicated in this app.
type WatchIn struct {
	// Cluster is the registered cluster to watch.
	Cluster string `json:"cluster" validate:"required"`
	// Kind is the registered kind.
	Kind string `json:"kind" validate:"required"`
	// Namespace narrows to one namespace; empty means every namespace this
	// registration may address.
	Namespace string `json:"namespace,omitempty"`
	// Version is the cursor to resume from — the Version a previous list or
	// watch returned. Empty starts from the current state.
	Version string `json:"version,omitempty"`
	// WaitSeconds is how long to wait for a change before answering empty.
	// Clamped to a bound this app owns, so a caller cannot pin a watch open.
	WaitSeconds int `json:"waitSeconds,omitempty"`
}

// Change is one watch event: what happened, and the object it happened to.
type Change struct {
	// Type is "ADDED", "MODIFIED" or "DELETED".
	Type     string   `json:"type"`
	Resource Resource `json:"resource"`
}

// Changes is what happened since the cursor, and the cursor to resume from.
//
// Expired says the cursor is too old for the apiserver's history: the caller
// must LIST again rather than treat an empty page as "nothing changed", which is
// the bug that makes a watch-backed board silently stop updating.
type Changes struct {
	Changes []Change `json:"changes"`
	Version string   `json:"version,omitempty"`
	Expired bool     `json:"expired,omitempty"`
}

// ---- access ----------------------------------------------------------------

// CanIn asks the apiserver whether this app's own credential may do a thing.
//
// It is a SelfSubjectAccessReview: the answer comes from the cluster's RBAC
// rather than from a grant this app asserts about itself. Asking beats
// asserting — an out-of-tree ClusterRole that drifted is then a refusal a
// caller can degrade on, not a 403 in the middle of a mutation.
type CanIn struct {
	// Cluster is the registered cluster to ask.
	Cluster string `json:"cluster" validate:"required"`
	// Verb is the Kubernetes verb: "get", "list", "create", "patch", "delete", …
	Verb string `json:"verb" validate:"required"`
	// Group is the API group; empty for the core group.
	Group string `json:"group,omitempty"`
	// Resource is the plural resource name ("pods", "resourcequotas").
	Resource string `json:"resource" validate:"required"`
	// Namespace scopes the question; empty asks cluster-wide.
	Namespace string `json:"namespace,omitempty"`
}

// Can is the apiserver's verdict, with its own reason when it gave one.
type Can struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// ---- metrics ---------------------------------------------------------------

// PodMetric is one pod's live consumption, summed over its containers.
type PodMetric struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// CPUMilli is CPU use in millicores; MemoryBytes is working-set memory.
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// PodMetrics is live consumption for the pods asked about.
//
// Available is false when the cluster has no metrics-server. That is a DIFFERENT
// fact from "every pod is idle", and a board that renders the two the same way
// tells an operator a healthy cluster is asleep.
type PodMetrics struct {
	Pods      []PodMetric `json:"pods"`
	Available bool        `json:"available"`
}

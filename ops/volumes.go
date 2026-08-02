package ops

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/plane"
)

// listClaims returns PersistentVolumeClaims.
//
// Response: {"claims":[{"namespace":"acme-prod","name":"data","phase":"Bound","volume":"pvc-9f2","requestGi":100,"storageClass":"do-block-storage"}]}
func (o Ops) listClaims(ctx context.Context, in *plane.Selector) (*plane.Claims, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	scope, err := b.Scope(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	out := []plane.Claim{}
	for _, ns := range scope {
		list, err := b.Typed.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{LabelSelector: in.LabelSelector})
		if err != nil {
			return nil, fail(err)
		}
		for i := range list.Items {
			out = append(out, claimView(list.Items[i]))
		}
	}
	return &plane.Claims{Claims: out}, nil
}

// listPersistentVolumes returns the cluster's PersistentVolumes.
//
// A PV is where the link to the real cloud device lives: `handle` is the provider's
// own volume id, read from the CSI source or from the legacy flexVolume shape, and it
// is what lets a reconciliation say whether a disk somebody is paying for is attached
// to anything. The claim reference travels with it because a PV with no claim and a PV
// whose claim is gone are different situations.
//
// PVs are cluster-scoped, so a namespace-bounded registration cannot read them.
//
// Response: {"volumes":[{"name":"pvc-9f2","phase":"Bound","handle":"a1b2c3","claimNamespace":"acme-prod","claimName":"data","capacityGi":100}]}
func (o Ops) listPersistentVolumes(ctx context.Context, in *plane.ClusterRef) (*plane.Volumes, error) {
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	if err := b.WholeCluster(); err != nil {
		return nil, fail(err)
	}
	list, err := b.Typed.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fail(err)
	}
	out := make([]plane.Volume, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, volumeView(list.Items[i]))
	}
	return &plane.Volumes{Volumes: out}, nil
}

// ensureClaim creates a PersistentVolumeClaim if it is absent.
//
// Idempotent on name in the namespace, for the reason ensureNamespace is: the
// get-then-create ladder is a race when each caller writes it.
//
// Example: {"cluster":"hanzo-k8s","namespace":"acme-prod","name":"data","gi":100,"storageClass":"do-block-storage"}
// Response: {"name":"data","created":true}
func (o Ops) ensureClaim(ctx context.Context, in *plane.EnsureClaimIn) (*plane.Ensured, error) {
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	if in.Gi <= 0 {
		return nil, zip.ErrBadRequest("'gi' must be at least 1")
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	if _, err := b.Typed.CoreV1().PersistentVolumeClaims(ns).Get(ctx, in.Name, metav1.GetOptions{}); err == nil {
		return &plane.Ensured{Name: in.Name}, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fail(err)
	}
	mode := corev1.ReadWriteOnce
	if strings.EqualFold(in.AccessMode, string(corev1.ReadWriteMany)) {
		mode = corev1.ReadWriteMany
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: in.Name, Namespace: ns,
			Labels: map[string]string{"managed-by": managedBy},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{mode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: gi(in.Gi)},
			},
		},
	}
	if in.StorageClass != "" {
		sc := in.StorageClass
		pvc.Spec.StorageClassName = &sc
	}
	_, err = b.Typed.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return &plane.Ensured{Name: in.Name}, nil
	}
	if err != nil {
		return nil, fail(err)
	}
	return &plane.Ensured{Name: in.Name, Created: true}, nil
}

// expandClaim grows a PersistentVolumeClaim to a new size.
//
// This is the ONE correct way to grow a volume Kubernetes manages, and the reason it
// is a named op rather than a patch: the resize controller acts on the CLAIM, growing
// the backing cloud device and then the filesystem on it, so claim, PersistentVolume,
// device and filesystem all end up agreeing. Growing the device through the cloud
// provider's own API instead leaves the claim and the PV declaring the old capacity
// and the filesystem never grown at all — three sources of truth, two of them wrong.
//
// The StorageClass must set `allowVolumeExpansion`; when it does not, the apiserver
// refuses and its message is surfaced verbatim rather than worked around. A shrink is
// refused by the apiserver too, and there is no op that deletes a claim: the claim
// holds the only copy of the tenant's data, so this app can make one bigger and
// nothing else.
//
// Example: {"cluster":"hanzo-k8s","namespace":"acme-prod","name":"data","gi":200}
// Response: {"gi":200}
func (o Ops) expandClaim(ctx context.Context, in *plane.ExpandClaimIn) (*plane.Expanded, error) {
	if err := need("name", in.Name); err != nil {
		return nil, err
	}
	if in.Gi <= 0 {
		return nil, zip.ErrBadRequest("'gi' must be at least 1")
	}
	b, err := o.reach(ctx, in.Cluster)
	if err != nil {
		return nil, err
	}
	ns, err := b.One(in.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":"%dGi"}}}}`, in.Gi)
	if _, err := b.Typed.CoreV1().PersistentVolumeClaims(ns).Patch(
		ctx, in.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return nil, fail(err)
	}
	return &plane.Expanded{Gi: in.Gi}, nil
}

func claimView(p corev1.PersistentVolumeClaim) plane.Claim {
	v := plane.Claim{
		Namespace: p.Namespace, Name: p.Name,
		Phase: string(p.Status.Phase), Volume: p.Spec.VolumeName,
	}
	if q, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		v.RequestGi = int(q.Value() >> 30)
	}
	if p.Spec.StorageClassName != nil {
		v.StorageClass = *p.Spec.StorageClassName
	}
	return v
}

func volumeView(pv corev1.PersistentVolume) plane.Volume {
	v := plane.Volume{Name: pv.Name, Phase: string(pv.Status.Phase), Handle: volumeHandle(pv)}
	if pv.Spec.ClaimRef != nil {
		v.ClaimNS, v.ClaimName = pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
	}
	if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		v.CapacityGi = int(q.Value() >> 30)
	}
	return v
}

// volumeHandle extracts the backing device id a PV claims. Deliberately NOT filtered
// by CSI driver name: matching broadly means MORE volumes are treated as in use, which
// is the safe direction for anything that would otherwise delete one. The legacy
// flexVolume shape is read too, so a pre-CSI PV still protects its device.
func volumeHandle(pv corev1.PersistentVolume) string {
	if pv.Spec.CSI != nil && strings.TrimSpace(pv.Spec.CSI.VolumeHandle) != "" {
		return pv.Spec.CSI.VolumeHandle
	}
	if pv.Spec.FlexVolume != nil {
		if v := strings.TrimSpace(pv.Spec.FlexVolume.Options["volumeID"]); v != "" {
			return v
		}
	}
	return ""
}

func gi(n int) resource.Quantity {
	return resource.MustParse(fmt.Sprintf("%dGi", n))
}

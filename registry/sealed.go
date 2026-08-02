package registry

import (
	"context"
	"errors"
	"net/http"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/k8s/plane"
)

// Sealed is the production [Store]: the KMS peer, reached over the call plane.
//
// Every credential this app holds lives there and nowhere else. There is no
// kubeconfig on disk, no credential in an environment variable, and no path by
// which one is printed — the material is fetched at the moment of use, held for
// the length of one client construction, and never returned to a caller.
//
// The call carries the CALLER, so KMS applies its own tenant rule to the ref: a
// ref under "orgs/<org>/" is served only to a call acting for that org. That is a
// second refusal, in a second process, of the thing this package already refuses —
// and the reason the ref shape is derived rather than stored.
type Sealed struct{}

// Get reads one sealed value. An absent ref is (nil, nil): a registry with no
// index yet is an org with no clusters, which is not an error. An unreachable KMS
// IS an error, because answering "no clusters" during a KMS outage would make a
// tenant's whole fleet look deregistered.
func (Sealed) Get(ctx context.Context, ref string) ([]byte, error) {
	out, err := zip.Ask[plane.SecretIn, plane.Secret](ctx, plane.KMS, plane.KMSGet, &plane.SecretIn{Ref: ref})
	if err != nil {
		if absent(err) {
			return nil, nil
		}
		return nil, err
	}
	return out.Value, nil
}

// Put writes one sealed value. A nil value is a legitimate write: it destroys the
// material at that ref, which is how a deregistration disposes of a credential
// without this app needing a delete verb KMS does not offer.
func (Sealed) Put(ctx context.Context, ref string, value []byte) error {
	_, err := zip.Ask[plane.SecretIn, plane.Secret](ctx, plane.KMS, plane.KMSPut, &plane.SecretIn{Ref: ref, Value: value})
	return err
}

// absent reports whether an error means "there is nothing at that ref" rather
// than "the store did not answer". Only a 404 counts: every other status —
// including a 403, which means the ref belongs to another tenant — must surface,
// because treating a refusal as an empty registry is how a tenancy failure turns
// into a silently empty board.
func absent(err error) bool {
	var he *zip.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

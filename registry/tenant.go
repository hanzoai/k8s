package registry

import (
	"context"
	"strings"

	"github.com/zap-proto/zip"
)

// Tenant is the org this call may act for, or a refusal.
//
// It is zip.Tenant plus one thing zip cannot know: the org becomes a KMS ref
// SEGMENT here, so a value containing '/' would address material outside the
// org's own prefix. That is a property of this app's storage, not of the
// framework's identity model, so it is checked here and only here.
//
// zip.Tenant is the FULL rule and not a non-empty check — no validated user claim
// means the org that rode along is a claim the caller made about itself, an empty
// org is a refusal, and an org over zip.MaxOrgLen is a refusal because the value
// is retained past the request as a store key. The org is used VERBATIM: trimmed,
// never lower-cased and never truncated, because folding collapses "acme",
// "ACME" and a 32-character prefix into one bucket, which is itself a
// cross-tenant break.
func Tenant(ctx context.Context) (string, error) {
	org, ok := zip.Tenant(ctx)
	if !ok {
		return "", ErrNoTenant{}
	}
	if strings.ContainsAny(org, "/\\") || strings.Contains(org, "..") {
		return "", ErrNoTenant{}
	}
	return org, nil
}

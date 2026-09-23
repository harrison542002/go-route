package adminapi

import (
	"context"

	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

// CreateAPIKey is the one write a repeat does not leave alone: every call mints
// a new secret, so a retry leaves a second key to revoke.
func (h *Handler) CreateAPIKey(
	ctx context.Context, req gen.CreateAPIKeyRequestObject,
) (gen.CreateAPIKeyResponseObject, error) {
	var in ports.NewAPIKey
	if req.Body != nil {
		in.ModelAllowlist = fromAllowlist(req.Body.ModelAllowlist)
	}

	issued, err := h.admin.CreateAPIKey(ctx, writeFrom(ctx), req.Tenant, in)
	if err != nil {
		return nil, err
	}
	return gen.CreateAPIKey201JSONResponse(toIssuedKey(issued.Key, issued.Secret)), nil
}

func (h *Handler) ListAPIKeys(
	ctx context.Context, req gen.ListAPIKeysRequestObject,
) (gen.ListAPIKeysResponseObject, error) {
	keys, err := h.admin.ListAPIKeys(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	out := gen.ListAPIKeys200JSONResponse{Keys: make([]gen.APIKey, 0, len(keys))}
	for _, k := range keys {
		out.Keys = append(out.Keys, toKey(k))
	}
	return out, nil
}

func (h *Handler) GetAPIKey(
	ctx context.Context, req gen.GetAPIKeyRequestObject,
) (gen.GetAPIKeyResponseObject, error) {
	k, err := h.admin.GetAPIKey(ctx, req.Key)
	if err != nil {
		return nil, err
	}
	return gen.GetAPIKey200JSONResponse(toKey(k)), nil
}

// UpdateAPIKey replaces the allowlist. Absent, null and [] mean three different
// things -- an error, every alias, none -- so absent is refused here as well as
// by the spec, lest it ever read as "allow everything".
func (h *Handler) UpdateAPIKey(
	ctx context.Context, req gen.UpdateAPIKeyRequestObject,
) (gen.UpdateAPIKeyResponseObject, error) {
	if !req.Body.ModelAllowlist.IsSpecified() {
		return nil, invalidf("model_allowlist is required: an array of aliases, [] for none, or null for every alias")
	}
	p := ports.APIKeyPatch{
		ModelAllowlist: fromAllowlist(req.Body.ModelAllowlist),
	}

	k, err := h.admin.UpdateAPIKey(ctx, writeFrom(ctx), req.Key, p)
	if err != nil {
		return nil, err
	}
	return gen.UpdateAPIKey200JSONResponse(toKey(k)), nil
}

// RevokeAPIKey keeps the row, so the key stays attributable in the ledger.
func (h *Handler) RevokeAPIKey(
	ctx context.Context, req gen.RevokeAPIKeyRequestObject,
) (gen.RevokeAPIKeyResponseObject, error) {
	k, err := h.admin.RevokeAPIKey(ctx, writeFrom(ctx), req.Key)
	if err != nil {
		return nil, err
	}
	return gen.RevokeAPIKey200JSONResponse(toKey(k)), nil
}

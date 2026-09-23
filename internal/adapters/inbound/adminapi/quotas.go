package adminapi

import (
	"context"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

func (h *Handler) ListQuotas(
	ctx context.Context, req gen.ListQuotasRequestObject,
) (gen.ListQuotasResponseObject, error) {
	set, err := h.admin.ListQuotas(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	return gen.ListQuotas200JSONResponse(toQuotas(set)), nil
}

// ReplaceQuotas makes the body the tenant's entire quota set, in one
// transaction. Removing every limit must be asked for explicitly, with
// "quotas": []; a JSON null gets past the spec and is refused here.
func (h *Handler) ReplaceQuotas(
	ctx context.Context, req gen.ReplaceQuotasRequestObject,
) (gen.ReplaceQuotasResponseObject, error) {
	if req.Body.Quotas == nil {
		return nil, invalidf(`quotas is required; send "quotas": [] to remove every limit`)
	}
	set := make([]domains.Quota, 0, len(req.Body.Quotas))
	for _, q := range req.Body.Quotas {
		set = append(set, fromQuota(q))
	}

	written, err := h.admin.ReplaceQuotas(ctx, writeFrom(ctx), req.Tenant, set)
	if err != nil {
		return nil, err
	}
	return gen.ReplaceQuotas200JSONResponse(toQuotas(written)), nil
}

func (h *Handler) GetQuota(
	ctx context.Context, req gen.GetQuotaRequestObject,
) (gen.GetQuotaResponseObject, error) {
	q, err := h.admin.GetQuota(ctx, req.Tenant, domains.WindowKind(req.Window))
	if err != nil {
		return nil, err
	}
	return gen.GetQuota200JSONResponse(toQuota(q)), nil
}

// PutQuota sets one window's quota and leaves the others alone. The window
// comes from the path; a body naming a different one is refused rather than one
// of the two quietly preferred.
func (h *Handler) PutQuota(
	ctx context.Context, req gen.PutQuotaRequestObject,
) (gen.PutQuotaResponseObject, error) {
	if w := req.Body.WindowKind; w != nil && *w != req.Window {
		return nil, invalidf("window_kind in the body (%s) does not match the path (%s)", *w, req.Window)
	}
	q := fromQuota(*req.Body)
	q.WindowKind = domains.WindowKind(req.Window)

	written, err := h.admin.PutQuota(ctx, writeFrom(ctx), req.Tenant, q)
	if err != nil {
		return nil, err
	}
	return gen.PutQuota200JSONResponse(toQuota(written)), nil
}

func (h *Handler) DeleteQuota(
	ctx context.Context, req gen.DeleteQuotaRequestObject,
) (gen.DeleteQuotaResponseObject, error) {
	if err := h.admin.DeleteQuota(ctx, writeFrom(ctx), req.Tenant, domains.WindowKind(req.Window)); err != nil {
		return nil, err
	}
	return gen.DeleteQuota204Response{}, nil
}

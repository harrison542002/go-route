package adminapi

import (
	"context"

	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

// CreateTenant is safe to repeat because external_id is unique: the same body
// again is a 200, a different body for a taken id a 409.
func (h *Handler) CreateTenant(
	ctx context.Context, req gen.CreateTenantRequestObject,
) (gen.CreateTenantResponseObject, error) {
	in := ports.NewTenant{ExternalID: req.Body.ExternalId, Name: req.Body.Name}
	if req.Body.Metadata != nil {
		in.Metadata = *req.Body.Metadata
	}

	c, err := h.admin.CreateTenant(ctx, writeFrom(ctx), in)
	if err != nil {
		return nil, err
	}
	if c.Created {
		return gen.CreateTenant201JSONResponse(toTenant(c.Tenant)), nil
	}
	return gen.CreateTenant200JSONResponse(toTenant(c.Tenant)), nil
}

func (h *Handler) ListTenants(
	ctx context.Context, req gen.ListTenantsRequestObject,
) (gen.ListTenantsResponseObject, error) {
	f := ports.TenantFilter{}
	if req.Params.IncludeDisabled != nil {
		f.IncludeDisabled = *req.Params.IncludeDisabled
	}
	if req.Params.Limit != nil {
		f.Limit = *req.Params.Limit
	}

	tenants, err := h.admin.ListTenants(ctx, f)
	if err != nil {
		return nil, err
	}
	out := gen.ListTenants200JSONResponse{Tenants: make([]gen.Tenant, 0, len(tenants))}
	for _, t := range tenants {
		out.Tenants = append(out.Tenants, toTenant(t))
	}
	return out, nil
}

func (h *Handler) GetTenant(
	ctx context.Context, req gen.GetTenantRequestObject,
) (gen.GetTenantResponseObject, error) {
	t, err := h.admin.GetTenant(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	return gen.GetTenant200JSONResponse(toTenant(t)), nil
}

func (h *Handler) UpdateTenant(
	ctx context.Context, req gen.UpdateTenantRequestObject,
) (gen.UpdateTenantResponseObject, error) {
	p := ports.TenantPatch{Name: req.Body.Name}
	if req.Body.Metadata != nil {
		p.Metadata = *req.Body.Metadata
	}

	t, err := h.admin.UpdateTenant(ctx, writeFrom(ctx), req.Tenant, p)
	if err != nil {
		return nil, err
	}
	return gen.UpdateTenant200JSONResponse(toTenant(t)), nil
}

// DisableTenant answers DELETE: tenants are never removed, because the ledger
// refers to them.
func (h *Handler) DisableTenant(
	ctx context.Context, req gen.DisableTenantRequestObject,
) (gen.DisableTenantResponseObject, error) {
	t, err := h.admin.DisableTenant(ctx, writeFrom(ctx), req.Tenant)
	if err != nil {
		return nil, err
	}
	return gen.DisableTenant200JSONResponse(toTenant(t)), nil
}

func (h *Handler) EnableTenant(
	ctx context.Context, req gen.EnableTenantRequestObject,
) (gen.EnableTenantResponseObject, error) {
	t, err := h.admin.EnableTenant(ctx, writeFrom(ctx), req.Tenant)
	if err != nil {
		return nil, err
	}
	return gen.EnableTenant200JSONResponse(toTenant(t)), nil
}

package adminapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

func (h *Handler) GetUsage(
	ctx context.Context, req gen.GetUsageRequestObject,
) (gen.GetUsageResponseObject, error) {
	tenant, err := h.obs.ResolveTenant(ctx, string(req.Tenant))
	if err != nil {
		return nil, tenantError(err, string(req.Tenant))
	}

	spec := ports.ReportSpec{
		Tenant:  tenant,
		Since:   req.Params.Since,
		Until:   h.now(),
		GroupBy: ports.GroupByNone,
	}
	if req.Params.Until != nil {
		spec.Until = *req.Params.Until
	}
	if req.Params.GroupBy != nil {
		spec.GroupBy = ports.GroupBy(*req.Params.GroupBy)
	}
	if req.Params.MetaKey != nil {
		spec.MetaKey = *req.Params.MetaKey
	}
	if req.Params.Limit != nil {
		spec.Limit = *req.Params.Limit
	}

	if spec.GroupBy != ports.GroupByMetadata && spec.MetaKey != "" {
		return nil, invalidf("meta_key is only meaningful with group_by=metadata")
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s", ports.ErrInvalid, err)
	}

	report, err := h.obs.Aggregate(ctx, spec)
	if err != nil {
		return nil, err
	}
	return gen.GetUsage200JSONResponse(toUsageReport(spec, report)), nil
}

// ListAudit returns one page of a tenant's audit events, newest first.
func (h *Handler) ListAudit(
	ctx context.Context, req gen.ListAuditRequestObject,
) (gen.ListAuditResponseObject, error) {
	tenant, err := h.obs.ResolveTenant(ctx, string(req.Tenant))
	if err != nil {
		return nil, tenantError(err, string(req.Tenant))
	}

	q := ports.AuditQuery{Tenant: tenant, Since: req.Params.Since, Until: h.now()}
	if req.Params.Until != nil {
		q.Until = *req.Params.Until
	}
	if req.Params.Limit != nil {
		q.Limit = *req.Params.Limit
	}
	if c := req.Params.Cursor; c != nil && *c != "" {
		after, err := decodeCursor(*c)
		if err != nil {
			return nil, invalidf("%s; pass back next_cursor exactly as it was given", err)
		}
		q.After = &after
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s", ports.ErrInvalid, err)
	}

	page, err := h.obs.ListAudit(ctx, q)
	if err != nil {
		return nil, err
	}
	return gen.ListAudit200JSONResponse(toAuditPage(page)), nil
}

func (h *Handler) GetRequest(
	ctx context.Context, req gen.GetRequestRequestObject,
) (gen.GetRequestResponseObject, error) {
	id, err := domains.ParseDecisionID(string(req.DecisionId))
	if err != nil {
		return nil, invalidf("%s is not a decision id", string(req.DecisionId))
	}

	d, err := h.obs.Get(ctx, id)
	if errors.Is(err, ports.ErrDecisionNotFound) {
		// Old partitions are dropped, so a bare "not found" would send an operator
		// hunting a bug that is really a retention policy.
		return nil, fmt.Errorf("%w: no decision %s is held; it may have aged out of a dropped partition rather than never existing",
			ports.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return gen.GetRequest200JSONResponse(toDecision(d)), nil
}

// tenantError turns an unknown tenant into a 404 naming it; a dashboard would
// otherwise read an empty report as "spent nothing".
func tenantError(err error, ref string) error {
	if errors.Is(err, ports.ErrUnknownTenant) {
		return fmt.Errorf("%w: no tenant %q", ports.ErrNotFound, ref)
	}
	return err
}

package adminapi

import (
	"encoding/json"

	"github.com/oapi-codegen/nullable"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

func toTenant(t domains.TenantAccount) gen.Tenant {
	meta := json.RawMessage(t.Metadata)
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	return gen.Tenant{
		Id:         t.ID,
		ExternalId: string(t.ExternalID),
		Name:       t.Name,
		Metadata:   meta,
		CreatedAt:  t.CreatedAt,
		DisabledAt: nullableOf(t.DisabledAt),
	}
}

func toKey(k domains.APIKey) gen.APIKey {
	return gen.APIKey{
		Id:             k.ID,
		TenantId:       k.TenantID,
		KeyPrefix:      k.Prefix,
		ModelAllowlist: toAllowlist(k.ModelAllowlist),
		CreatedAt:      k.CreatedAt,
		LastUsedAt:     nullableOf(k.LastUsedAt),
		RevokedAt:      nullableOf(k.RevokedAt),
	}
}

func toIssuedKey(k domains.APIKey, secret string) gen.IssuedAPIKey {
	key := toKey(k)
	return gen.IssuedAPIKey{
		Id:             key.Id,
		TenantId:       key.TenantId,
		KeyPrefix:      key.KeyPrefix,
		ModelAllowlist: key.ModelAllowlist,
		CreatedAt:      key.CreatedAt,
		LastUsedAt:     key.LastUsedAt,
		RevokedAt:      key.RevokedAt,
		Secret:         secret,
	}
}

func toAllowlist(list []string) nullable.Nullable[gen.ModelAllowlist] {
	if list == nil {
		return nullable.NewNullNullable[gen.ModelAllowlist]()
	}
	return nullable.NewNullableWithValue(list)
}

func fromAllowlist(n nullable.Nullable[gen.ModelAllowlist]) []string {
	if !n.IsSpecified() || n.IsNull() {
		return nil
	}
	list := n.MustGet()
	if list == nil {
		list = []string{}
	}
	return list
}

func toQuota(q domains.Quota) gen.Quota {
	return gen.Quota{
		WindowKind:   gen.WindowKind(q.WindowKind),
		PeriodStart:  nullableOf(q.PeriodStart),
		PeriodEnd:    nullableOf(q.PeriodEnd),
		MaxCostNanos: int64(q.MaxCost),
		OnExceed:     gen.QuotaAction(q.OnExceed),
		UpdatedAt:    q.UpdatedAt,
	}
}

func toQuotas(set []domains.Quota) gen.QuotaList {
	out := gen.QuotaList{Quotas: make([]gen.Quota, 0, len(set))}
	for _, q := range set {
		out.Quotas = append(out.Quotas, toQuota(q))
	}
	return out
}

func fromQuota(in gen.QuotaInput) domains.Quota {
	out := domains.Quota{
		PeriodStart: pointerOf(in.PeriodStart),
		PeriodEnd:   pointerOf(in.PeriodEnd),
		MaxCost:     domains.USD(in.MaxCostNanos),
	}
	if in.WindowKind != nil {
		out.WindowKind = domains.WindowKind(*in.WindowKind)
	}
	if in.OnExceed != nil {
		out.OnExceed = domains.QuotaAction(*in.OnExceed)
	}
	return out
}

func nullableOf[T any](p *T) nullable.Nullable[T] {
	if p == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*p)
}

func pointerOf[T any](n nullable.Nullable[T]) *T {
	if !n.IsSpecified() || n.IsNull() {
		return nil
	}
	v := n.MustGet()
	return &v
}

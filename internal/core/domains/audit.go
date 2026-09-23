package domains

import (
	"time"

	"github.com/google/uuid"
)

// AuditEvent is one row of audit_log as a reader sees it, covering both kinds
// the table holds: routed traffic and admin mutations. The ladder itself is not
// here -- that is RoutingDecision -- so a page of events stays small.
type AuditEvent struct {
	ID uuid.UUID
	At time.Time

	// TenantID is nil for an admin action taken before any tenant exists.
	TenantID *uuid.UUID

	KeyID *uuid.UUID

	// Actor is gateway for routed traffic and admin:<name> for a mutation made
	// over the admin API.
	Actor string

	// Action is route_request for routed traffic, or the name of the mutation,
	// such as quota.upsert.
	Action string

	// RequestID is the decision id for routed traffic, empty otherwise.
	RequestID string

	// Detail is the JSON state an admin mutation left behind, or the alias or
	// rule that produced a ladder.
	Detail string

	// FinalTarget is the target that served, empty when none did.
	FinalTarget string

	StatusCode *int

	// Error is the message from the final attempt when it failed.
	Error string
}

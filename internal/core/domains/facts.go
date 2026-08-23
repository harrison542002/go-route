package domains

import "time"

// RequestFacts is everything the router knows about an inbound request before dispatch.
// It is extracted once at ingress and is immutable thereafter.
type RequestFacts struct {
	Tenant         Tenant
	Metadata       map[string]string
	RequestedModel string // the "model" field as the client sent it
	Stream         bool
	WantsUsage     bool
	ReceivedAt     time.Time
}

// Tenant identifies the billing/policy scope.
// For single tenant deploy, set it as default
type Tenant string

const DefaultTenant Tenant = "default"
const (
	MaxMetadataKeys = 32
	MaxMetadataLen  = 256
)

package domains

import (
	"time"

	"github.com/google/uuid"
)

// RequestFacts is everything the router knows about an inbound request before dispatch.
// It is extracted once at ingress and is immutable thereafter.
type RequestFacts struct {
	Tenant Tenant

	// KeyID is the api_keys row that authenticated this request, zero
	// when nothing did. It attributes spend to one credential, so
	// revoking a leaked key does not mean guessing which traffic was its.
	KeyID uuid.UUID

	Metadata       map[string]string
	RequestedModel string
	Stream         bool
	WantsUsage     bool
	ReceivedAt     time.Time

	// PromptSize, MaxOutputTokens and Choices are what a quota reservation is
	// estimated from. MaxOutputTokens is zero when the client set no ceiling;
	// Choices is the n parameter, zero when unset.
	PromptSize      PromptSize
	MaxOutputTokens int
	Choices         int
}

// Tenant identifies the billing/policy scope.
// For single tenant deploy, set it as default
type Tenant string

const DefaultTenant Tenant = "default"
const (
	MaxMetadataKeys = 32
	MaxMetadataLen  = 256
	MaxTenantLen    = 256
)

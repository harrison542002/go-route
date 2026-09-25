package proxyapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

var maxBodyBytes int64 = 20 << 20 // multimodal requests carry megabytes of base64

// Router resolves a request into an ordered ladder of target references.
// Today this is a static alias table; policy routing implements the same
// contract later, producing ladders with ReasonRuleMatch, and nothing
// here changes.
//
//go:generate mockgen -source=handler.go -destination=mocks/handler_mock.go -package=mocks
type Router interface {
	Route(domains.RequestFacts) (domains.Ladder, error)
}

// Resolver turns target references into dialable targets. It is the seam
// between the domain, which reasons about names, and dispatch, which
// needs live provider clients.
type Resolver interface {
	Resolve(domains.Ladder) ([]ports.Target, error)
}

type Handler struct {
	router     Router
	resolver   Resolver
	dispatcher ports.Dispatcher
	sink       ports.DecisionSink
	quota      ports.QuotaEnforcer
	now        func() time.Time
}

// NewHandler wires the request path. A nil quota enforces nothing, which is
// what a deployment without Redis gets.
func NewHandler(
	r Router,
	res Resolver,
	d ports.Dispatcher,
	sink ports.DecisionSink,
	quota ports.QuotaEnforcer,
	now func() time.Time,
) *Handler {
	if now == nil {
		now = time.Now
	}
	if quota == nil {
		quota = unlimited{}
	}
	return &Handler{router: r, resolver: res, dispatcher: d, sink: sink, quota: quota, now: now}
}

// unlimited is the enforcer for a deployment with no quota store.
type unlimited struct{}

func (unlimited) Reserve(context.Context, ports.QuotaRequest) (ports.Reservation, error) {
	return ports.Reservation{}, nil
}

func (unlimited) Reconcile(context.Context, ports.Reservation, domains.Outcome) {}

func (h *Handler) Completions(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	ctx := r.Context()
	decisionID := domains.NewDecisionID()

	identity, ok := IdentityFrom(ctx)
	if !ok {
		writeAuthError(w, ports.ErrNoCredentials)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
		return
	}

	facts, err := ExtractFacts(r, raw, identity.Tenant, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	facts.KeyID = identity.KeyID

	if !identity.AllowsModel(facts.RequestedModel) {
		writeAuthError(w, fmt.Errorf("%w: %s", ports.ErrModelNotAllowed, facts.RequestedModel))
		return
	}

	ladder, err := h.router.Route(facts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	targets, err := h.resolver.Resolve(ladder)
	if err != nil {
		slog.Error("resolver failed on a validated ladder",
			"decision_id", decisionID.String(), "err", err)
		writeError(w, http.StatusInternalServerError, "routing misconfiguration", "internal_error")
		return
	}

	// Quota is the last gate before dispatch: a request refused for any other
	// reason never takes a slot, and nothing that costs money has happened yet.
	reservation, err := h.quota.Reserve(ctx, ports.QuotaRequest{
		Tenant:          facts.Tenant,
		At:              now,
		Prompt:          facts.PromptSize,
		MaxOutputTokens: facts.MaxOutputTokens,
		Choices:         facts.Choices,
		Targets:         targetNames(ladder),
	})
	if err != nil {
		var exceeded *ports.QuotaExceededError
		if errors.As(err, &exceeded) {
			writeQuotaExceeded(w, decisionID, exceeded.Breach, now)
			h.sink.Record(domains.NewRoutingDecision(decisionID, facts, ladder,
				domains.Outcome{Status: domains.StatusPolicyBlocked}))
			return
		}

		slog.Error("quota check unavailable; failing closed",
			"decision_id", decisionID.String(), "err", err)
		writeError(w, http.StatusServiceUnavailable,
			"quota enforcement is unavailable", "internal_error")
		return
	}

	var outcome domains.Outcome

	// Deferred so it runs however dispatch ends, a panic included: a
	// reservation never reconciled holds the tenant's budget until the window
	// closes. A zero outcome releases the whole reservation.
	defer func() {
		h.quota.Reconcile(ctx, reservation, outcome)
	}()

	out := NewClientStream(w, decisionID, facts.Stream)

	outcome = h.dispatcher.Run(ctx, targets, &ports.ProviderRequest{
		Body:       raw,
		Stream:     facts.Stream,
		WantsUsage: facts.WantsUsage,
	}, out)

	if outcome.Status == domains.StatusExhausted {
		writeError(w, statusFor(outcome), lastMessage(outcome), "upstream_error")
	}

	decision := domains.NewRoutingDecision(decisionID, facts, ladder, outcome)
	h.sink.Record(decision)
}

func targetNames(l domains.Ladder) []string {
	out := make([]string, len(l.Targets))
	for i, t := range l.Targets {
		out[i] = t.Name
	}
	return out
}

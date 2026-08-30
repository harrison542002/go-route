package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/dispatch"
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
	Resolve(domains.Ladder) ([]dispatch.Target, error)
}

type Handler struct {
	router     Router
	resolver   Resolver
	dispatcher *dispatch.Dispatcher
	sink       ports.DecisionSink
	pricer     ports.PricingTable
	now        func() time.Time
}

func NewHandler(r Router, res Resolver, d *dispatch.Dispatcher, sink ports.DecisionSink, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{router: r, resolver: res, dispatcher: d, sink: sink, now: now}
}

func (h *Handler) Completions(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	decisionID := domains.NewDecisionID()

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
		return
	}

	facts, err := ExtractFacts(r, raw, domains.DefaultTenant, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
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

	out := NewClientStream(w, decisionID, facts.Stream)

	outcome := h.dispatcher.Run(r.Context(), targets, &ports.ProviderRequest{
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

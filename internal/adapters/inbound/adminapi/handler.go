package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/schemas/admin/gen"
)

type Handler struct {
	admin ports.Admin
	auth  ports.AdminAuth
	obs   ports.ObservabilityRepository
	now   func() time.Time
}

var _ gen.StrictServerInterface = (*Handler)(nil)

func NewHandler(
	admin ports.Admin, auth ports.AdminAuth, obs ports.ObservabilityRepository, now func() time.Time,
) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{admin: admin, auth: auth, obs: obs, now: now}
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ports.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error(), gen.InvalidRequestError)

	case errors.Is(err, ports.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error(), gen.NotFoundError)

	case errors.Is(err, ports.ErrConflict):
		writeError(w, http.StatusConflict, err.Error(), gen.ConflictError)

	// The request's own context is checked as well as the error because a driver
	// does not always wrap the context's error in its own.
	case errors.Is(err, context.DeadlineExceeded) || r.Context().Err() != nil:
		slog.Warn("admin request timed out", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusGatewayTimeout, "request timed out", gen.TimeoutError)

	default:
		slog.Error("admin request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error", gen.InternalError)
	}
}

func badRequest(w http.ResponseWriter, _ *http.Request, err error) {
	msg := err.Error()
	var param *gen.InvalidParamFormatError
	if errors.As(err, &param) {
		root := param.Err
		for next := errors.Unwrap(root); next != nil; next = errors.Unwrap(root) {
			root = next
		}
		msg = fmt.Sprintf("parameter %s: %v", param.ParamName, root)
	}
	writeError(w, http.StatusBadRequest, msg, gen.InvalidRequestError)
}

func writeError(w http.ResponseWriter, status int, message string, errType gen.ErrorBodyType) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(gen.Error{Error: gen.ErrorBody{Message: message, Type: errType}})
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ports.ErrInvalid, fmt.Sprintf(format, args...))
}

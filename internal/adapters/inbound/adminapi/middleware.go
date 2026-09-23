package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/harrison542002/go-route/schemas/admin/gen"
)

const maxBodyBytes = 1 << 20

func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func Body(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large", gen.InvalidRequestError)
			} else {
				writeError(w, http.StatusBadRequest, "could not read request body", gen.InvalidRequestError)
			}
			return
		}

		if len(bytes.TrimSpace(body)) > 0 {
			var v json.RawMessage
			if err := json.Unmarshal(body, &v); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), gen.InvalidRequestError)
				return
			}
			r.Header.Set("Content-Type", "application/json")
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }

		next.ServeHTTP(w, r)
	})
}

func Validate(spec *openapi3.T) func(http.Handler) http.Handler {
	return nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		DoNotValidateServers: true,
		Options: openapi3filter.Options{
			AuthenticationFunc:  openapi3filter.NoopAuthenticationFunc,
			SkipSettingDefaults: true,
		},
		ErrorHandlerWithOpts: validationFailed,
	})
}

func validationFailed(
	_ context.Context, err error, w http.ResponseWriter, _ *http.Request, opts nethttpmiddleware.ErrorHandlerOpts,
) {
	if opts.MatchedRoute == nil {
		if errors.Is(err, routers.ErrMethodNotAllowed) {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", gen.InvalidRequestError)
			return
		}
		writeError(w, http.StatusNotFound, "no such endpoint", gen.NotFoundError)
		return
	}

	var reqErr *openapi3filter.RequestError
	if !errors.As(err, &reqErr) {
		slog.Error("admin request validation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error", gen.InternalError)
		return
	}

	writeError(w, http.StatusBadRequest, validationMessage(reqErr), gen.InvalidRequestError)
}

// validationMessage says where the request went wrong and why in one line;
// kin-openapi's own message appends the whole offending schema.
func validationMessage(e *openapi3filter.RequestError) string {
	where := "request body"
	if p := e.Parameter; p != nil {
		where = fmt.Sprintf("%s parameter %s", p.In, p.Name)
	}

	var schemaErr *openapi3.SchemaError
	switch {
	case errors.As(e.Err, &schemaErr):
		if path := schemaErr.JSONPointer(); len(path) > 0 {
			where += " field " + strings.Join(path, ".")
		}
		reason := schemaErr.Reason
		switch schemaErr.SchemaField {
		case "required":
			return where + " is required"
		case "format":
			// The reason goes on to quote the format's whole regexp.
			reason, _, _ = strings.Cut(reason, " (")
		}
		return where + ": " + reason
	case errors.Is(e.Err, openapi3filter.ErrInvalidRequired):
		return where + " is required"
	case e.Err != nil:
		msg, _, _ := strings.Cut(e.Err.Error(), "\n")
		return where + ": " + msg
	}

	msg, _, _ := strings.Cut(e.Error(), "\n")
	return msg
}

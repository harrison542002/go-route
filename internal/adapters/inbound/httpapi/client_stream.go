package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/core/sse"
	"github.com/harrison542002/go-route/internal/ports"
)

type ClientStream struct {
	w          http.ResponseWriter
	rc         *http.ResponseController
	decisionID domains.DecisionID
	committed  bool
	stream     bool
}

var _ ports.ClientStream = (*ClientStream)(nil)

func NewClientStream(w http.ResponseWriter, id domains.DecisionID, stream bool) *ClientStream {
	return &ClientStream{
		w:          w,
		rc:         http.NewResponseController(w),
		decisionID: id,
		stream:     stream,
	}
}

func (c *ClientStream) Commit() error {
	if c.committed {
		return fmt.Errorf("httpapi: Commit called twice")
	}
	c.committed = true

	h := c.w.Header()

	if c.stream {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")

		// nginx and several ingress controllers buffer proxied responses by
		// default, which turns a stream into one lump delivered at the end.
		h.Set("X-Accel-Buffering", "no")
	} else {
		h.Set("Content-Type", "application/json")
	}

	// The decision ID can be sent now because it is generated before
	// dispatch. Cost cannot: it is not known until the stream ends, and
	// HTTP trailers are poorly supported by client SDKs. Cost lives in
	// the decision log, retrievable via `go-route explain`.
	h.Set("X-Go-Route-Decision-Id", c.decisionID.String())

	c.w.WriteHeader(http.StatusOK)

	return c.rc.Flush()
}

func (c *ClientStream) Send(ev ports.StreamEvent) error {
	if !c.stream {
		_, err := c.w.Write(ev.Raw)
		return err
	}
	if !c.committed {
		return fmt.Errorf("httpapi: Send before Commit")
	}
	if err := sse.WriteEvent(c.w, sse.Event{Data: ev.Raw}); err != nil {
		return err
	}
	return c.rc.Flush()
}

func (c *ClientStream) SendError(cause error) error {
	if !c.committed {
		return fmt.Errorf("httpapi: SendError before Commit")
	}

	payload, err := json.Marshal(errorEnvelope{Error: errorBody{
		Message: "upstream stream ended unexpectedly: " + cause.Error(),
		Type:    "upstream_error",
	}})

	if err != nil {
		return err
	}

	if err := sse.WriteEvent(c.w, sse.Event{Data: payload}); err != nil {
		return err
	}
	return c.rc.Flush()
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

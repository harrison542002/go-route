package oaicompat

import (
	"bytes"
	"io"

	"github.com/harrison542002/go-route/internal/core/sse"
	"github.com/harrison542002/go-route/internal/ports"
)

// mirrors from OpenAI response
var doneSentinel = []byte("[DONE]")

type reader struct {
	dec  *sse.Decoder
	body io.ReadCloser
}

var _ ports.StreamReader = (*reader)(nil)

func (r *reader) Next() (ports.StreamEvent, error) {
	ev, err := r.dec.Next()
	if err != nil {
		return ports.StreamEvent{}, err
	}

	if bytes.Equal(ev.Data, doneSentinel) {
		return ports.StreamEvent{Raw: ev.Data, Terminal: true}, nil
	}

	out := ports.StreamEvent{Raw: ev.Data}

	// A chunk we cannot parse is still forwarded verbatim. Losing cost
	// data for one request is acceptable; breaking the client's stream
	// because a provider shipped a shape we do not model is not.
	if c, err := parseChunk(ev.Data); err == nil {
		out.Usage = c.usage()
		out.UsageOnly = c.usageOnly()
	}

	return out, nil
}

func (r *reader) Close() error {
	return r.body.Close()
}

// nonStreamReader adapts a non-streaming response to the same interface:
// one event, Terminal, then io.EOF. Keeping a single code path downstream
// is worth more than the small cost of buffering here.
type nonStreamReader struct {
	body io.ReadCloser
	raw  []byte
	done bool
}

var _ ports.StreamReader = (*nonStreamReader)(nil)

func (r *nonStreamReader) Next() (ports.StreamEvent, error) {
	if r.done {
		return ports.StreamEvent{}, io.EOF
	}
	r.done = true

	out := ports.StreamEvent{Raw: r.raw, Terminal: true}
	if c, err := parseChunk(r.raw); err == nil {
		out.Usage = c.usage()
	}
	return out, nil
}

func (r *nonStreamReader) Close() error {
	return r.body.Close()
}

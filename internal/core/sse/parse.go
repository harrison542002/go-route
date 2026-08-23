// Package sse decodes and encodes the Server-Side Event wire format.

package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

var (
	ErrLineTooLong = errors.New("sse: line exceeds maximum length")
	errPartialLine = errors.New("sse: partial line at EOF")
)

const (
	defaultMaxLine = 1 << 20  // 1 MB
	readBufSize    = 16 << 10 //16 KB
)

// Event is one dispatched SSE event. Data is the concatenated
// payload with framing removed (no prefix "data :" and no trailing newlines)
type Event struct {
	Type string
	Data []byte
}

// Decoder reads Events from a stream.
// It is not safe for concurrent use
type Decoder struct {
	r       *bufio.Reader
	maxLine int

	// state for the event currently being built
	data    []byte
	evType  []byte
	sawData bool // at least one "data" field seen this event
	dirty   bool // at least one field line seen since the last dispatch

	err error // sticky: once set, Next return it
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{
		r:       bufio.NewReaderSize(r, readBufSize),
		maxLine: defaultMaxLine,
	}
}

// Next returns the next event. It returns io.EOF on a clean end and
// io.ErrUnexpected EOF if the stream ended mid-event
func (d *Decoder) Next() (Event, error) {
	if d.err != nil {
		return Event{}, d.err
	}

	for {
		line, err := d.readLine()

		switch {
		case errors.Is(err, errPartialLine):
			return Event{}, d.fail(io.ErrUnexpectedEOF)
		case errors.Is(err, io.EOF):
			if d.dirty {
				return Event{}, d.fail(io.ErrUnexpectedEOF)
			}
			return Event{}, d.fail(io.EOF)
		case err != nil:
			return Event{}, d.fail(err)
		}

		if len(line) == 0 {
			if !d.sawData {
				// Spec: a dispatch with no data field emits nothing.
				// Comment-only or metadata-only frames land here.
				d.reset()
				continue
			}
			ev := Event{
				Type: string(d.evType),
				Data: append([]byte(nil), d.data...),
			}
			d.reset()
			return ev, nil
		}

		if line[0] == ':' {
			continue
		}

		field, value := parseField(line)
		d.dirty = true

		switch string(field) {
		case "data":
			if d.sawData {
				d.data = append(d.data, '\n')
			}
			d.data = append(d.data, value...)
			d.sawData = true
		case "event":
			d.evType = append(d.evType[:0], value...)
		default:
			// "id", "retry", and anything unknown are ignored
		}
	}
}

// WriteEvent writes ev in SSE wire format, including the terminating blank
// line. Data containing '\n' is split back into one data field per segement,
// inverting the join perform by the decoder
func WriteEvent(w io.Writer, ev Event) error {
	if bytes.ContainsAny([]byte(ev.Type), "\r\n") {
		return fmt.Errorf("sse: event type contains a line terminator: %q", ev.Type)
	}

	var buf bytes.Buffer
	if ev.Type != "" {
		buf.WriteString("event: ")
		buf.WriteString(ev.Type)
		buf.WriteByte('\n')
	}

	for _, seg := range bytes.Split(ev.Data, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(seg)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')

	_, err := w.Write(buf.Bytes())
	return err
}

func (d *Decoder) readLine() ([]byte, error) {
	var acc []byte

	for {
		chunk, err := d.r.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			if len(acc)+len(chunk) > d.maxLine {
				return nil, ErrLineTooLong
			}

			acc = append(acc, chunk...)
			continue
		case errors.Is(err, io.EOF):
			if len(acc) == 0 && len(chunk) == 0 {
				return nil, io.EOF
			}

			if len(acc)+len(chunk) > d.maxLine {
				return nil, ErrLineTooLong
			}
			return append(acc, chunk...), errPartialLine
		case err != nil:
			return nil, err
		}

		if len(acc)+len(chunk) > d.maxLine {
			return nil, ErrLineTooLong
		}
		if len(acc) > 0 {
			return trimEOL(append(acc, chunk...)), nil
		}
		return trimEOL(chunk), nil
	}
}

func (d *Decoder) fail(err error) error {
	d.err = err
	return err
}

func (d *Decoder) reset() {
	d.data = d.data[:0]
	d.evType = d.evType[:0]
	d.sawData = false
	d.dirty = false
}

func trimEOL(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r"))
}

func parseField(line []byte) (field, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil
	}

	field, value = line[:i], line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}

func drain(d *Decoder) ([]Event, error) {
	var out []Event
	for {
		ev, err := d.Next()
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

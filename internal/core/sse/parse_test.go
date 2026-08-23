package sse

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecoder(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []Event
		wantErr error
	}{
		{
			name:  "two simple events",
			input: "data: {\"a\":1}\n\ndata: {\"a\":2}\n\n",
			want:  []Event{{Data: []byte(`{"a":1}`)}, {Data: []byte(`{"a":2}`)}},
		},
		{
			name:  "sentinel is opaque to this layer",
			input: "data: [DONE]\n\n",
			want:  []Event{{Data: []byte("[DONE]")}},
		},
		{
			name:  "multiline data joined with newline, no trailing",
			input: "data: line1\ndata: line2\n\n",
			want:  []Event{{Data: []byte("line1\nline2")}},
		},
		{
			name:  "empty data line contributes an empty segment",
			input: "data: a\ndata:\ndata: b\n\n",
			want:  []Event{{Data: []byte("a\n\nb")}},
		},
		{
			name:  "comment ignored",
			input: ": ping\n\ndata: x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "event type captured",
			input: "event: message_start\ndata: {}\n\n",
			want:  []Event{{Type: "message_start", Data: []byte("{}")}},
		},
		{
			name:  "event type does not leak into next event",
			input: "event: a\ndata: 1\n\ndata: 2\n\n",
			want:  []Event{{Type: "a", Data: []byte("1")}, {Type: "", Data: []byte("2")}},
		},
		{
			name:  "crlf terminators",
			input: "event: x\r\ndata: y\r\n\r\n",
			want:  []Event{{Type: "x", Data: []byte("y")}},
		},
		{
			name:  "no space after colon",
			input: "data:x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "exactly one space stripped",
			input: "data:  x\n\n",
			want:  []Event{{Data: []byte(" x")}},
		},
		{
			name:  "field with no colon has empty value",
			input: "data\n\n",
			want:  []Event{{Data: []byte("")}},
		},
		{
			name:  "unknown fields ignored",
			input: "id: 42\nretry: 100\ndata: x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "dispatch with no data field emits nothing",
			input: "event: ping\n\ndata: x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "consecutive blank lines are harmless",
			input: "data: x\n\n\n\ndata: y\n\n",
			want:  []Event{{Data: []byte("x")}, {Data: []byte("y")}},
		},
		{
			name:    "truncated mid-frame is reported",
			input:   "data: {\"a\":1}\n\ndata: {\"a\":2",
			want:    []Event{{Data: []byte(`{"a":1}`)}},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "final frame without blank line is truncated",
			input:   "data: {\"a\":1}\n\ndata: {\"a\":2}\n",
			want:    []Event{{Data: []byte(`{"a":1}`)}},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:  "clean end after blank line is EOF",
			input: "data: x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "trailing comment then EOF is clean",
			input: "data: x\n\n: ping\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDecoder(strings.NewReader(tt.input))
			got, err := drain(d)
			wantErr := tt.wantErr
			if wantErr == nil {
				wantErr = io.EOF
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("terminal error = %v, want %v", err, wantErr)
			}
			assertEvents(t, got, tt.want)
		})
	}
}

// Data must be safe to hold after subsequent Next calls.
func TestDecoder_DataIsNotAliased(t *testing.T) {
	d := NewDecoder(strings.NewReader("data: first\n\ndata: second\n\n"))

	first, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Next(); err != nil {
		t.Fatal(err)
	}
	if string(first.Data) != "first" {
		t.Fatalf("first event mutated to %q — Data is aliased to internal buffer", first.Data)
	}
}

func TestDecoder_LineTooLong(t *testing.T) {
	d := NewDecoder(strings.NewReader("data: " + strings.Repeat("x", 5000) + "\n\n"))
	d.maxLine = 1024

	if _, err := d.Next(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

// A line larger than the bufio buffer but under maxLine must still decode.
func TestDecoder_LineExceedingBufioBuffer(t *testing.T) {
	payload := strings.Repeat("y", 64<<10) // > 16 KiB internal buffer
	d := NewDecoder(strings.NewReader("data: " + payload + "\n\n"))

	ev, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(ev.Data) != payload {
		t.Fatalf("payload length = %d, want %d", len(ev.Data), len(payload))
	}
}

func TestDecoder_ErrorIsSticky(t *testing.T) {
	d := NewDecoder(strings.NewReader("data: x"))

	if _, err := d.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("first err = %v", err)
	}
	if _, err := d.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("second err = %v, want same sticky error", err)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, name := range []string{"openai_stream.txt", "anthropic_stream.txt"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Skipf("fixture missing: %v", err)
			}

			first, err := drain(NewDecoder(bytes.NewReader(raw)))
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("decode: %v", err)
			}
			if len(first) == 0 {
				t.Fatal("no events decoded")
			}

			var buf bytes.Buffer
			for _, ev := range first {
				if err := WriteEvent(&buf, ev); err != nil {
					t.Fatal(err)
				}
			}

			second, err := drain(NewDecoder(&buf))
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("re-decode: %v", err)
			}
			assertEvents(t, second, first)
		})
	}
}

func TestFixtureShape(t *testing.T) {
	cases := []struct{ file, wantLastData, wantLastType string }{
		{"openai_stream.txt", "[DONE]", ""},
		{"anthropic_stream.txt", "", "message_stop"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Skipf("fixture missing: %v", err)
			}
			evs, err := drain(NewDecoder(bytes.NewReader(raw)))
			if !errors.Is(err, io.EOF) {
				t.Fatalf("fixture should decode cleanly, got %v", err)
			}
			last := evs[len(evs)-1]
			t.Logf("%d events, last: type=%q data=%q", len(evs), last.Type, last.Data)

			if tc.wantLastType != "" && last.Type != tc.wantLastType {
				t.Errorf("last type = %q, want %q", last.Type, tc.wantLastType)
			}
			if tc.wantLastData != "" && string(last.Data) != tc.wantLastData {
				t.Errorf("last data = %q, want %q", last.Data, tc.wantLastData)
			}
		})
	}
}

func TestDecoder_BareCR(t *testing.T) {
	t.Skip("bare \\r terminators unsupported by design — see docs/adr/0002-sse-line-endings.md")
}

func TestWriteEvent_Shape(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want string
	}{
		{"typed", Event{Type: "message_start", Data: []byte(`{"a":1}`)},
			"event: message_start\ndata: {\"a\":1}\n\n"},
		{"untyped emits no event line", Event{Data: []byte("x")},
			"data: x\n\n"},
		{"multiline data splits", Event{Data: []byte("a\nb")},
			"data: a\ndata: b\n\n"},
		{"empty data still emits one field", Event{Data: []byte("")},
			"data: \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := WriteEvent(&b, tt.ev); err != nil {
				t.Fatal(err)
			}
			if b.String() != tt.want {
				t.Errorf("got %q, want %q", b.String(), tt.want)
			}
		})
	}
}

func assertEvents(t *testing.T, got, want []Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d\n got: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Errorf("event %d type = %q, want %q", i, got[i].Type, want[i].Type)
		}
		if !bytes.Equal(got[i].Data, want[i].Data) {
			t.Errorf("event %d data = %q, want %q", i, got[i].Data, want[i].Data)
		}
	}
}

package transcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/jsoncodec"
	"example.com/idlbridge/internal/rpc"
	"example.com/idlbridge/internal/wire"
)

// ResponseOptions controls response rendering.
type ResponseOptions struct {
	JSON jsoncodec.Options
	// Wire selects binary output instead of JSON (content negotiation).
	Wire bool
}

// RenderResponse serialises a backend response for the given binding.
// response_body narrows the output to a single field.
func RenderResponse(b *Binding, msg *dyn.Message, opt ResponseOptions) ([]byte, string, error) {
	if msg == nil {
		msg = dyn.New(b.Method.Output)
	}
	if b.ResponseField != nil {
		inner, ok := msg.Get(b.ResponseField)
		if !ok {
			// Absent response_body field renders as its default value.
			inner = b.ResponseField.Default()
			if b.ResponseField.Type.Message != nil {
				inner = dyn.New(b.ResponseField.Type.Message)
			}
		}
		if b.ResponseField.Type.Message != nil {
			m, _ := inner.(*dyn.Message)
			if m == nil {
				m = dyn.New(b.ResponseField.Type.Message)
			}
			msg = m
		} else {
			// A scalar response_body is not a message; render it directly.
			if opt.Wire {
				return nil, "application/protobuf", fmt.Errorf("response_body %q names a non-message field; binary output is unavailable", b.Rule.ResponseBody)
			}
			data, err := jsoncodec.MarshalValue(b.ResponseField.Type, inner, opt.JSON)
			return data, "application/json", err
		}
	}
	if opt.Wire {
		data, err := wire.Marshal(msg)
		if err != nil {
			return nil, "application/protobuf", err
		}
		return data, "application/protobuf", nil
	}
	data, err := jsoncodec.Marshal(msg, opt.JSON)
	if err != nil {
		return nil, "application/json", err
	}
	return data, "application/json", nil
}

// StreamWriter emits a server-streaming response as chunked JSON: the body is
// a sequence of newline-delimited JSON objects, each one response message.
// (A "[" / "," / "]" framing would also be valid JSON; NDJSON is what most
// gRPC-JSON transcoders emit and is what we document.)
type StreamWriter struct {
	W       http.ResponseWriter
	Flusher http.Flusher
	Binding *Binding
	Options ResponseOptions
	// NewlineDelimited selects NDJSON (default) over a JSON array.
	Array bool

	wroteHeader bool
	wroteOne    bool
	err         error
}

// Begin writes the response status and headers. It must be called before the
// first Send.
func (s *StreamWriter) Begin(status int) {
	if s.wroteHeader {
		return
	}
	h := s.W.Header()
	if s.Options.Wire {
		h.Set("Content-Type", "application/x-protobuf-stream")
	} else if s.Array {
		h.Set("Content-Type", "application/json")
	} else {
		h.Set("Content-Type", "application/x-ndjson")
	}
	h.Set("X-Accel-Buffering", "no")
	h.Set("Cache-Control", "no-cache")
	s.W.WriteHeader(status)
	if s.Array {
		if _, err := s.W.Write([]byte("[")); err != nil {
			s.err = err
		}
	}
	s.wroteHeader = true
	s.flush()
}

// Send appends one message to the stream and flushes it.
func (s *StreamWriter) Send(msg *dyn.Message) error {
	if s.err != nil {
		return s.err
	}
	data, _, err := RenderResponse(s.Binding, msg, s.Options)
	if err != nil {
		return err
	}
	var buf []byte
	if s.Array {
		if s.wroteOne {
			buf = append(buf, ',')
		}
		buf = append(buf, data...)
	} else {
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	if _, err := s.W.Write(buf); err != nil {
		s.err = err
		return err
	}
	s.wroteOne = true
	s.flush()
	return nil
}

// Written reports whether the response status has been sent.
func (s *StreamWriter) Written() bool { return s.wroteHeader }

// Finish closes the stream framing.
func (s *StreamWriter) Finish() {
	if !s.wroteHeader {
		s.Begin(http.StatusOK)
	}
	if s.Array && s.err == nil {
		if _, err := s.W.Write([]byte("]")); err != nil {
			s.err = err
		}
	}
	s.flush()
}

func (s *StreamWriter) flush() {
	if s.Flusher != nil {
		s.Flusher.Flush()
	}
}

// StatusError is emitted inside a stream after a successful Begin: the stream
// carries an error document as its final line, matching the trailer semantics
// of gRPC-JSON transcoders that cannot change the status code at that point.
func (s *StreamWriter) StatusError(st *rpc.Status) {
	if !s.wroteHeader {
		return
	}
	body := ErrorBodyFor(st)
	data, err := jsonMarshal(body, s.Options.JSON.Indent)
	if err != nil {
		return
	}
	var buf []byte
	if s.Array {
		if s.wroteOne {
			buf = append(buf, ',')
		}
		buf = append(buf, data...)
	} else {
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	_, _ = s.W.Write(buf)
	s.wroteOne = true
	s.flush()
}

// DeadlineFor returns a per-request timeout from a header, defaulting to 30s.
func DeadlineFor(h http.Header) time.Duration {
	const def = 30 * time.Second
	v := h.Get("Grpc-Timeout")
	if v == "" {
		v = h.Get("X-Request-Timeout")
	}
	if v == "" {
		return def
	}
	d, err := ParseTimeout(v)
	if err != nil || d <= 0 {
		return def
	}
	max := 10 * time.Minute
	if d > max {
		return max
	}
	return d
}

// ParseTimeout parses a gRPC-style timeout value: a positive number followed
// by H, M, S, m, u or n (hours, minutes, seconds, milliseconds, microseconds,
// nanoseconds). A plain "1.5s" style value is also accepted.
func ParseTimeout(v string) (time.Duration, error) {
	if v == "" {
		return 0, fmt.Errorf("empty timeout")
	}
	unit := v[len(v)-1]
	num := v[:len(v)-1]
	var mult time.Duration
	switch unit {
	case 'H':
		mult = time.Hour
	case 'M':
		mult = time.Minute
	case 'S':
		mult = time.Second
	case 'm':
		mult = time.Millisecond
	case 'u', 'U':
		mult = time.Microsecond
	case 'n', 'N':
		mult = time.Nanosecond
	case 'h':
		mult = time.Hour
	case 's':
		mult = time.Second
	default:
		return 0, fmt.Errorf("invalid timeout unit %q", unit)
	}
	var f float64
	if _, err := fmt.Sscanf(num, "%f", &f); err != nil {
		return 0, fmt.Errorf("invalid timeout %q", v)
	}
	return time.Duration(f * float64(mult)), nil
}

// jsonMarshal is a tiny indirection so tests can stub formatting.
func jsonMarshal(v any, indent string) ([]byte, error) {
	if indent == "" {
		return json.Marshal(v)
	}
	return json.MarshalIndent(v, "", indent)
}

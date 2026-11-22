package transcode_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/jsoncodec"
	"example.com/idlbridge/internal/rpc"
	"example.com/idlbridge/internal/transcode"
)

func TestStatusMappingCompleteness(t *testing.T) {
	// Every code must map to a documented status; no code falls through to
	// an accidental default.
	seen := map[int]bool{}
	for c := rpc.OK; c <= rpc.Unauthenticated; c++ {
		st := transcode.HTTPStatus(c)
		if st < 100 || st > 599 {
			t.Fatalf("%s -> invalid HTTP %d", c, st)
		}
		if c > rpc.OK && st < 400 {
			t.Fatalf("%s -> %d should be an error status", c, st)
		}
		seen[st] = true
	}
	if !seen[400] || !seen[500] || !seen[404] {
		t.Fatalf("expected distinct mappings, got %v", seen)
	}
}

func TestErrorStatusFor(t *testing.T) {
	for _, kind := range []string{"path", "query", "body"} {
		code, st := transcode.ErrorStatusFor(&transcode.BindingError{Kind: kind, Msg: "bad"})
		if code != http.StatusBadRequest || st.Code != rpc.InvalidArgument {
			t.Fatalf("%s: %d %v", kind, code, st)
		}
	}
	code, st := transcode.ErrorStatusFor(context.DeadlineExceeded)
	if code != http.StatusInternalServerError || st.Code != rpc.Internal {
		t.Fatalf("generic error: %d %v", code, st)
	}
	if transcode.StatusOf(nil).Code != rpc.OK {
		t.Fatal("nil error should be OK")
	}
}

func newStreamWriter(t *testing.T, array bool, desc *idl.Message) (*transcode.StreamWriter, *httptest.ResponseRecorder, http.Flusher) {
	t.Helper()
	rec := httptest.NewRecorder()
	binding := &transcode.Binding{Method: &idl.Method{Output: desc}}
	sw := &transcode.StreamWriter{
		W:       rec,
		Binding: binding,
		Options: transcode.ResponseOptions{JSON: jsoncodec.Options{}},
		Array:   array,
	}
	if f, ok := any(rec).(http.Flusher); ok {
		sw.Flusher = f
	}
	return sw, rec, nil
}

func TestStreamWriterNDJSON(t *testing.T) {
	desc := &idl.Message{Name: "M", FullName: "t.M"}
	f := &idl.Field{Name: "v", JSONName: "v", Number: 1, Type: idl.Type{Scalar: idl.TString}}
	desc.Fields = []*idl.Field{f}
	sw, rec, _ := newStreamWriter(t, false, desc)
	sw.Begin(http.StatusOK)
	for _, v := range []string{"a", "b"} {
		m := dyn.New(desc)
		m.Set(f, v)
		if err := sw.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	sw.Finish()
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}
	if got := rec.Body.String(); got != "{\"v\":\"a\"}\n{\"v\":\"b\"}\n" {
		t.Fatalf("body %q", got)
	}
	// Each line must be independently valid JSON.
	for _, line := range []string{"{\"v\":\"a\"}", "{\"v\":\"b\"}"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamWriterArray(t *testing.T) {
	desc := &idl.Message{Name: "M", FullName: "t.M"}
	f := &idl.Field{Name: "v", JSONName: "v", Number: 1, Type: idl.Type{Scalar: idl.TInt32}}
	desc.Fields = []*idl.Field{f}
	sw, rec, _ := newStreamWriter(t, true, desc)
	sw.Begin(http.StatusOK)
	for _, v := range []int32{1, 2, 3} {
		m := dyn.New(desc)
		m.Set(f, v)
		if err := sw.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	sw.Finish()
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("array framing invalid: %v (%s)", err, rec.Body)
	}
	if len(arr) != 3 {
		t.Fatalf("array = %v", arr)
	}
}

func TestStreamWriterStatusError(t *testing.T) {
	desc := &idl.Message{Name: "M", FullName: "t.M"}
	f := &idl.Field{Name: "v", JSONName: "v", Number: 1, Type: idl.Type{Scalar: idl.TString}}
	desc.Fields = []*idl.Field{f}
	sw, rec, _ := newStreamWriter(t, false, desc)
	sw.Begin(http.StatusOK)
	m := dyn.New(desc)
	m.Set(f, "ok")
	sw.Send(m)
	sw.StatusError(rpc.NewStatus(rpc.Unavailable, "backend died"))
	sw.Finish()
	lines := splitLines(rec.Body.String())
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &errDoc); err != nil {
		t.Fatal(err)
	}
	if errDoc["status"] != "UNAVAILABLE" || errDoc["message"] != "backend died" {
		t.Fatalf("error doc = %v", errDoc)
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestResponseBodyField(t *testing.T) {
	desc := &idl.Message{Name: "R", FullName: "t.R"}
	inner := &idl.Message{Name: "I", FullName: "t.I"}
	inner.Fields = []*idl.Field{
		{Name: "x", JSONName: "x", Number: 1, Type: idl.Type{Scalar: idl.TString}},
	}
	desc.Fields = []*idl.Field{
		{Name: "inner", JSONName: "inner", Number: 1, Type: idl.Type{Message: inner}},
	}
	b := &transcode.Binding{Method: &idl.Method{Output: desc}, ResponseField: desc.Fields[0]}
	m := dyn.New(desc)
	c := dyn.New(inner)
	c.Set(inner.Fields[0], "hi")
	m.Set(desc.Fields[0], c)
	data, ct, err := transcode.RenderResponse(b, m, transcode.ResponseOptions{})
	if err != nil || ct != "application/json" {
		t.Fatalf("%v %q", err, ct)
	}
	if string(data) != `{"x":"hi"}` {
		t.Fatalf("response_body = %s", data)
	}
	// An absent response_body field renders a default message.
	empty := dyn.New(desc)
	data, _, err = transcode.RenderResponse(b, empty, transcode.ResponseOptions{})
	if err != nil || string(data) != `{}` {
		t.Fatalf("absent response_body: %v %s", err, data)
	}
}

func TestParseTimeout(t *testing.T) {
	cases := map[string]string{
		"5S":   "5s",
		"100m": "100ms",
		"1.5s": "1.5s",
		"2H":   "2h0m0s",
	}
	for in, want := range cases {
		d, err := transcode.ParseTimeout(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if d.String() != want {
			t.Fatalf("%s = %v want %v", in, d, want)
		}
	}
	for _, bad := range []string{"", "x", "5X", "abcS"} {
		if _, err := transcode.ParseTimeout(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

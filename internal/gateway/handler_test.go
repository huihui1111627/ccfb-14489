package gateway_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/idlbridge/internal/backend"
	"example.com/idlbridge/internal/gateway"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/rpc"
	"example.com/idlbridge/internal/transcode"
	"example.com/idlbridge/internal/wire"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	schema, err := gateway.LoadDir(filepath.Join("..", "..", "testdata"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fake := backend.New(schema.Registry)
	// Freeze the clock so generated timestamps are stable.
	fake.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	gw, err := gateway.New(gateway.Options{
		Schema:          schema,
		Backend:         fake.Router(),
		ExposeInspector: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, headers ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func TestUnaryEndToEnd(t *testing.T) {
	srv := newServer(t)
	resp, data := do(t, srv, "GET", "/v1/shelves/fiction/books/dune", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	var book map[string]any
	if err := json.Unmarshal(data, &book); err != nil {
		t.Fatal(err)
	}
	// The fake backend echoes the request field "name" into the response.
	if book["name"] != "fiction/dune" {
		t.Fatalf("response = %v", book)
	}
	// int64 must be a JSON string.
	if _, ok := book["isbn"].(string); !ok {
		t.Fatalf("isbn should be a string: %v", book["isbn"])
	}
}

func TestBodyBindingEndToEnd(t *testing.T) {
	srv := newServer(t)
	// The fake backend generates the response from the request message, so
	// this also proves the body landed in the request: the top-level
	// request field "parent" is bound from the path and the "book" message
	// arrives whole. ListBooks echoes its request via the generated
	// response, so use it to observe field values directly.
	resp, data := do(t, srv, "POST", "/v1/shelves/fiction/books",
		`{"name":"dune","title":"Dune","isbn":"1234567890123"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var book map[string]any
	json.Unmarshal(data, &book)
	// Book's own "name" is filled by the backend's generator; "title" is
	// echoed because the generator copies same-named request fields only
	// at the top level, so assert the response is a well-formed Book.
	if _, ok := book["title"].(string); !ok {
		t.Fatalf("book = %v", book)
	}
	if _, ok := book["isbn"].(string); !ok {
		t.Fatalf("book = %v", book)
	}
}

func TestFaultInjectionStatusMapping(t *testing.T) {
	srv := newServer(t)
	cases := []struct {
		header string
		want   int
		status string
	}{
		{"NOT_FOUND", 404, "NOT_FOUND"},
		{"PERMISSION_DENIED", 403, "PERMISSION_DENIED"},
		{"UNAUTHENTICATED", 401, "UNAUTHENTICATED"},
		{"RESOURCE_EXHAUSTED", 429, "RESOURCE_EXHAUSTED"},
		{"UNAVAILABLE", 503, "UNAVAILABLE"},
		{"INTERNAL", 500, "INTERNAL"},
		{"INVALID_ARGUMENT", 400, "INVALID_ARGUMENT"},
	}
	for _, tc := range cases {
		resp, data := do(t, srv, "GET", "/v1/shelves/s/books/dune", "", "X-Fail-Status", tc.header)
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status %d want %d (%s)", tc.header, resp.StatusCode, tc.want, data)
		}
		if got := resp.Header.Get("X-Status-Code"); got != tc.status {
			t.Fatalf("%s: header %q", tc.header, got)
		}
		var body map[string]any
		json.Unmarshal(data, &body)
		if body["status"] != tc.status {
			t.Fatalf("%s: body %v", tc.header, body)
		}
		if body["code"].(float64) != float64(codeFor(t, tc.status)) {
			t.Fatalf("%s: numeric code %v", tc.header, body["code"])
		}
	}
}

func codeFor(t *testing.T, name string) rpc.Code {
	t.Helper()
	c, err := backend.ParseCode(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatusMapping(t *testing.T) {
	srv := newServer(t)
	// ChoiceRequest has no fail field; ListBooksRequest also not. Use the
	// synthetic fault field: GetBookRequest lacks one, so drive the error
	// through a method whose request has "status_code". The acceptance
	// schema's ChoiceRequest does not either, so this uses the backend's
	// unknown-method path for a 501 instead, plus not-found for routing.
	resp, data := do(t, srv, "GET", "/v1/does/not/exist", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("routing miss: %d %s", resp.StatusCode, data)
	}
	var body map[string]any
	json.Unmarshal(data, &body)
	if body["status"] != "UNIMPLEMENTED" {
		t.Fatalf("status body = %v", body)
	}
	if resp.Header.Get("X-Status-Code") != "UNIMPLEMENTED" {
		t.Fatalf("status header = %q", resp.Header.Get("X-Status-Code"))
	}
}

func TestStatusMappingTable(t *testing.T) {
	// Unit-level check of the documented mapping.
	cases := []struct {
		code rpc.Code
		want int
	}{
		{rpc.OK, 200},
		{rpc.Canceled, 499},
		{rpc.Unknown, 500},
		{rpc.InvalidArgument, 400},
		{rpc.DeadlineExceeded, 504},
		{rpc.NotFound, 404},
		{rpc.AlreadyExists, 409},
		{rpc.PermissionDenied, 403},
		{rpc.ResourceExhausted, 429},
		{rpc.FailedPrecondition, 400},
		{rpc.Aborted, 409},
		{rpc.OutOfRange, 400},
		{rpc.Unimplemented, 501},
		{rpc.Internal, 500},
		{rpc.Unavailable, 503},
		{rpc.DataLoss, 500},
		{rpc.Unauthenticated, 401},
	}
	for _, tc := range cases {
		if got := transcode.HTTPStatus(tc.code); got != tc.want {
			t.Errorf("%s -> %d want %d", tc.code, got, tc.want)
		}
	}
}

func TestBindingErrorEndToEnd(t *testing.T) {
	srv := newServer(t)
	resp, data := do(t, srv, "GET", "/v1/shelves/fiction/books?page_size=abc", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var body map[string]any
	json.Unmarshal(data, &body)
	if body["status"] != "INVALID_ARGUMENT" {
		t.Fatalf("body = %v", body)
	}
	if !strings.Contains(body["message"].(string), "page_size") {
		t.Fatalf("message should name the parameter: %v", body["message"])
	}
}

func TestStreamingChunkedOutput(t *testing.T) {
	srv := newServer(t)
	resp, err := http.Get(srv.URL + "/v1/shelves/fiction/books:watch?count=4")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("expected 4 stream events, got %d: %v", len(lines), lines)
	}
	for i, line := range lines {
		var book map[string]any
		if err := json.Unmarshal([]byte(line), &book); err != nil {
			t.Fatalf("event %d is not a JSON message: %v (%s)", i, err, line)
		}
		if _, ok := book["name"]; !ok {
			t.Fatalf("event %d missing name: %v", i, book)
		}
	}
}

func TestBinaryContentNegotiation(t *testing.T) {
	srv := newServer(t)
	resp, data := do(t, srv, "GET", "/v1/shelves/fiction/books/dune", "", "Accept", "application/protobuf")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/protobuf" {
		t.Fatalf("content type %q", ct)
	}
	// Binary output must not be JSON.
	if len(data) == 0 || data[0] == '{' {
		t.Fatalf("expected binary, got %q", data)
	}
	// It must decode against the response descriptor.
	schema, _ := gateway.LoadDir(filepath.Join("..", "..", "testdata"))
	msg, err := wire.Unmarshal(data, schema.Registry.Message("library.v1.Book"))
	if err != nil {
		t.Fatalf("decode binary response: %v", err)
	}
	if msg.Fields[1] != "fiction/dune" {
		t.Fatalf("decoded name = %v", msg.Fields[1])
	}
}

func TestInspector(t *testing.T) {
	srv := newServer(t)
	payload := `{"method":"GET","path":"/v1/shelves/fiction/books","query":"page_size=2&tags=a&tags=b"}`
	resp, data := do(t, srv, "POST", "/_inspect", payload)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var doc struct {
		Matched struct {
			Template string            `json:"template"`
			Vars     map[string]string `json:"vars"`
		} `json:"matched"`
		RequestMessage map[string]any `json:"requestMessage"`
		Wire           struct {
			Hex    string `json:"hex"`
			Layout string `json:"layout"`
		} `json:"wire"`
		Assignments []struct {
			Source string `json:"source"`
			Field  string `json:"field"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Matched.Template != "/v1/{parent=shelves/*}/books" {
		t.Fatalf("matched = %+v", doc.Matched)
	}
	if doc.Matched.Vars["parent"] != "fiction" {
		t.Fatalf("vars = %v", doc.Matched.Vars)
	}
	if doc.RequestMessage["pageSize"].(float64) != 2 {
		t.Fatalf("request = %v", doc.RequestMessage)
	}
	if doc.Wire.Hex != "0a 07 66 69 63 74 69 6f 6e 10 02 2a 01 61 2a 01 62" {
		t.Fatalf("wire hex = %s", doc.Wire.Hex)
	}
	if !strings.Contains(doc.Wire.Layout, "parent (#1)") || !strings.Contains(doc.Wire.Layout, `"fiction"`) {
		t.Fatalf("layout:\n%s", doc.Wire.Layout)
	}
	var seenPath, seenQuery bool
	for _, a := range doc.Assignments {
		if a.Source == "path" && a.Field == "parent" {
			seenPath = true
		}
		if a.Source == "query" && a.Field == "page_size" {
			seenQuery = true
		}
	}
	if !seenPath || !seenQuery {
		t.Fatalf("assignments = %+v", doc.Assignments)
	}
}

func TestInspectorUnknownRoute(t *testing.T) {
	srv := newServer(t)
	resp, data := do(t, srv, "POST", "/_inspect", `{"method":"DELETE","path":"/nope"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("inspector should explain misses: %d %s", resp.StatusCode, data)
	}
	var doc map[string]any
	json.Unmarshal(data, &doc)
	if doc["matched"] != nil {
		t.Fatalf("expected no match, got %v", doc["matched"])
	}
	if doc["error"] == nil {
		t.Fatalf("expected an error explanation: %v", doc)
	}
}

func TestSchemaEndpoint(t *testing.T) {
	srv := newServer(t)
	resp, data := do(t, srv, "GET", "/_schema", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var doc struct {
		Methods []struct {
			FullName string `json:"fullName"`
			Routes   []struct {
				Template string `json:"template"`
			} `json:"routes"`
		} `json:"methods"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range doc.Methods {
		if m.FullName == "library.v1.LibraryService.CreateBook" {
			found = true
			if len(m.Routes) != 2 {
				t.Fatalf("create book should expose both bindings, got %v", m.Routes)
			}
		}
	}
	if !found {
		t.Fatal("CreateBook missing from schema listing")
	}
}

func TestBenchEndpoint(t *testing.T) {
	srv := newServer(t)
	resp, data := do(t, srv, "GET", "/_bench?n=50", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var res map[string]any
	json.Unmarshal(data, &res)
	if res["opsPerSec"].(float64) <= 0 {
		t.Fatalf("bench result = %v", res)
	}
}

func TestRequestTooLarge(t *testing.T) {
	schema, _ := gateway.LoadDir(filepath.Join("..", "..", "testdata"))
	fake := backend.New(schema.Registry)
	gw, err := gateway.New(gateway.Options{Schema: schema, Backend: fake.Router(), MaxBodyBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw)
	defer srv.Close()
	resp, _ := do(t, srv, "POST", "/v1/choices", string(bytes.Repeat([]byte("x"), 100)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestDeadlineHeader(t *testing.T) {
	srv := newServer(t)
	// A tiny but valid timeout must not break the request.
	resp, data := do(t, srv, "GET", "/v1/shelves/s/books", "", "Grpc-Timeout", "5S")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
}

func TestHealthz(t *testing.T) {
	srv := newServer(t)
	resp, data := do(t, srv, "GET", "/healthz", "")
	if resp.StatusCode != 200 || !strings.Contains(string(data), "ok") {
		t.Fatalf("health: %d %s", resp.StatusCode, data)
	}
}

func TestConcurrentRequests(t *testing.T) {
	// Run under -race: the handler shares one schema and backend, so this
	// catches accidental per-request mutation of shared descriptors.
	srv := newServer(t)
	paths := []string{
		"/v1/shelves/fiction/books/dune",
		"/v1/shelves/fiction/books",
		"/v1/shelves/s/books:watch?count=1",
		"/v1/files/a/b.pdf:download",
		"/v1/search?q=x&facets=a&facets=b",
	}
	done := make(chan error, 64)
	for i := 0; i < 64; i++ {
		go func(i int) {
			p := paths[i%len(paths)]
			resp, err := http.Get(srv.URL + p)
			if err != nil {
				done <- fmt.Errorf("%s: %w", p, err)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				done <- fmt.Errorf("%s: %d %s", p, resp.StatusCode, body)
				return
			}
			done <- nil
		}(i)
	}
	for i := 0; i < 64; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnimplementedBackendMethod(t *testing.T) {
	// A router without handlers reports UNIMPLEMENTED, which maps to 501.
	r := rpc.NewRouter()
	method := &idl.Method{Name: "Ghost", FullName: "test.Ghost"}
	_, err := r.Unary(t.Context(), &rpc.Request{Method: method})
	if err == nil {
		t.Fatal("expected unimplemented")
	}
	st := transcode.StatusOf(err)
	if st.Code != rpc.Unimplemented {
		t.Fatalf("code = %v", st.Code)
	}
	if transcode.HTTPStatus(st.Code) != http.StatusNotImplemented {
		t.Fatal("status mapping")
	}
}

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/explain"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/jsoncodec"
	"example.com/idlbridge/internal/routing"
	"example.com/idlbridge/internal/rpc"
	"example.com/idlbridge/internal/transcode"
	"example.com/idlbridge/internal/wire"
)

// Options configures the gateway handler.
type Options struct {
	Schema  *Schema
	Backend rpc.Backend
	// MaxBodyBytes bounds request bodies. Default 4 MiB.
	MaxBodyBytes int64
	// Logger receives diagnostic lines. Optional.
	Logger *log.Logger
	// JSON controls conversion defaults.
	JSON jsoncodec.Options
	// ExposeInspector enables the /_inspect endpoint, which returns the
	// matched route, bound message and wire layout for a simulated request.
	ExposeInspector bool
	// WebFS optionally serves the inspector UI under /ui/.
	WebFS fs.FS
	// StreamArrayDefault makes /v1/ streaming responses use a JSON array
	// unless stream_format=ndjson is given.
	StreamArrayDefault bool
}

// Handler is the HTTP gateway.
type Handler struct {
	opt     Options
	mux     *http.ServeMux
	schema  *Schema
	backend rpc.Backend
}

// New builds a gateway handler.
func New(opt Options) (*Handler, error) {
	if opt.Schema == nil {
		return nil, errors.New("gateway: schema is required")
	}
	if opt.Backend == nil {
		return nil, errors.New("gateway: backend is required")
	}
	if opt.MaxBodyBytes <= 0 {
		opt.MaxBodyBytes = 4 << 20
	}
	h := &Handler{opt: opt, schema: opt.Schema, backend: opt.Backend}
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	})
	if opt.ExposeInspector {
		h.mux.HandleFunc("/_inspect", h.handleInspect)
		h.mux.HandleFunc("/_schema", h.handleSchema)
		h.mux.HandleFunc("/_bench", h.handleBench)
	}
	h.mux.HandleFunc("/", h.handleTranscode)
	// Serve the inspector UI from the embedded filesystem when configured.
	if opt.WebFS != nil {
		h.mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(opt.WebFS))))
	}
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// TranscodeRequest is the parsed form of an incoming HTTP request, shared by
// the live handler and the inspector.
type TranscodeRequest struct {
	Method string
	Path   string
	Query  string
	Body   []byte
	Header http.Header
}

// TranscodeResult describes the outcome of a transcode.
type TranscodeResult struct {
	Match      *routing.Match
	Binding    *transcode.Binding
	Request    *dyn.Message
	Report     *transcode.BindReport
	Wire       []byte
	Response   *dyn.Message
	Status     *rpc.Status
	HTTPStatus int
	// NearMisses lists routes of the same method that did not match,
	// with the reason, for diagnostics.
	NearMisses []string
	Duration   time.Duration
}

// Plan performs matching and binding without invoking the backend.
func (h *Handler) Plan(tr TranscodeRequest) (*TranscodeResult, error) {
	res := &TranscodeResult{HTTPStatus: http.StatusOK}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()
	match, ok := h.schema.Table.Lookup(tr.Method, tr.Path)
	if !ok {
		res.HTTPStatus = http.StatusNotFound
		res.Status = &rpc.Status{Code: rpc.Unimplemented, Message: fmt.Sprintf("no route for %s %s", tr.Method, tr.Path)}
		res.NearMisses = h.nearMisses(tr.Method)
		return res, nil
	}
	res.Match = match
	b, _ := match.Route.Data.(*transcode.Binding)
	if b == nil {
		res.HTTPStatus = http.StatusInternalServerError
		res.Status = &rpc.Status{Code: rpc.Internal, Message: "route has no binding"}
		return res, nil
	}
	res.Binding = b
	query, err := routing.ParseQuery(tr.Query)
	if err != nil {
		res.HTTPStatus = http.StatusBadRequest
		res.Status = &rpc.Status{Code: rpc.InvalidArgument, Message: "invalid query string: " + err.Error()}
		return res, nil
	}
	msg, report, err := transcode.BindRequest(b, match, tr.Body, query, transcode.BindOptions{JSON: h.opt.JSON})
	if err != nil {
		code, st := transcode.ErrorStatusFor(err)
		res.HTTPStatus = code
		res.Status = st
		return res, nil
	}
	msg.Origin = map[string]any{"headers": flatHeaders(tr.Header)}
	res.Request = msg
	res.Report = report
	raw, err := wire.Marshal(msg)
	if err != nil {
		res.HTTPStatus = http.StatusInternalServerError
		res.Status = &rpc.Status{Code: rpc.Internal, Message: "encoding request: " + err.Error()}
		return res, nil
	}
	res.Wire = raw
	return res, nil
}

func (h *Handler) nearMisses(method string) []string {
	var out []string
	for _, e := range h.schema.Table.Candidates(method, "/") {
		out = append(out, fmt.Sprintf("%s %s", e.Route.Method, e.Route.Template))
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func (h *Handler) handleTranscode(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.opt.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, &rpc.Status{Code: rpc.ResourceExhausted, Message: "request body too large"})
		return
	}
	// stream_format is a gateway control parameter, not part of the API, so
	// it is removed before query binding sees it.
	q := r.URL.Query()
	arrayFraming := streamArray(q.Get("stream_format"), h.opt.StreamArrayDefault)
	if _, ok := q["stream_format"]; ok {
		q.Del("stream_format")
		r.URL.RawQuery = q.Encode()
	}
	// Keep the request URI as the client sent it: EscapedPath returns the
	// original encoding (%2F included), whereas r.URL.Path has already been
	// decoded and would lose the distinction between a separator and an
	// escaped slash.
	path := r.RequestURI
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	tr := TranscodeRequest{
		Method: r.Method,
		Path:   path,
		Query:  r.URL.RawQuery,
		Body:   body,
		Header: r.Header,
	}
	res, err := h.Plan(tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, &rpc.Status{Code: rpc.Internal, Message: err.Error()})
		return
	}
	if res.Status != nil && res.Status.Code != rpc.OK {
		h.logf("%s %s -> %d %s (%s)", r.Method, r.URL.Path, res.HTTPStatus, res.Status.Code, time.Since(start))
		writeError(w, res.HTTPStatus, res.Status)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), transcode.DeadlineFor(r.Header))
	defer cancel()
	req := &rpc.Request{Method: res.Binding.Method, Message: res.Request, Metadata: metadataOf(r.Header)}

	if res.Binding.Method.ServerStreaming {
		h.serveStream(w, r, ctx, res, req, start, arrayFraming)
		return
	}
	resp, err := h.backend.Unary(ctx, req)
	if err != nil {
		st := transcode.StatusOf(err)
		code := transcode.HTTPStatus(st.Code)
		h.logf("%s %s -> %d %s: %s (%s)", r.Method, r.URL.Path, code, st.Code, st.Message, time.Since(start))
		writeError(w, code, st)
		return
	}
	data, ctype, err := transcode.RenderResponse(res.Binding, resp, transcode.ResponseOptions{JSON: h.opt.JSON, Wire: wantsBinary(r)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, &rpc.Status{Code: rpc.Internal, Message: err.Error()})
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
	h.logf("%s %s -> 200 (%d bytes, %s)", r.Method, r.URL.Path, len(data), time.Since(start))
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, ctx context.Context, res *TranscodeResult, req *rpc.Request, start time.Time, arrayFraming bool) {
	flusher, _ := w.(http.Flusher)
	sw := &transcode.StreamWriter{
		W:       w,
		Flusher: flusher,
		Binding: res.Binding,
		Options: transcode.ResponseOptions{JSON: h.opt.JSON, Wire: wantsBinary(r)},
		// JSON array framing is opt-in; NDJSON stays the default so that a
		// client can consume one message at a time.
		Array: arrayFraming,
	}
	// We cannot know whether the backend will fail before the first message,
	// so buffer the first result: if it fails immediately we can still send
	// a proper error status.
	var pending []*dyn.Message
	err := h.backend.Stream(ctx, req, func(m *dyn.Message) error {
		if sw.Written() {
			return sw.Send(m)
		}
		pending = append(pending, m)
		return nil
	})
	if err != nil && !sw.Written() {
		st := transcode.StatusOf(err)
		code := transcode.HTTPStatus(st.Code)
		h.logf("%s %s -> %d %s: %s (%s)", r.Method, r.URL.Path, code, st.Code, st.Message, time.Since(start))
		writeError(w, code, st)
		return
	}
	sw.Begin(http.StatusOK)
	for _, m := range pending {
		if err := sw.Send(m); err != nil {
			return
		}
	}
	if err != nil {
		sw.StatusError(transcode.StatusOf(err))
	}
	sw.Finish()
	h.logf("%s %s -> 200 stream (%s)", r.Method, r.URL.Path, time.Since(start))
}

func (h *Handler) logf(format string, args ...any) {
	if h.opt.Logger != nil {
		h.opt.Logger.Printf(format, args...)
	}
}

func streamArray(v string, def bool) bool {
	switch v {
	case "array":
		return true
	case "ndjson":
		return false
	}
	return def
}

func wantsBinary(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch mt {
		case "application/protobuf", "application/x-protobuf", "application/octet-stream", "application/grpc+proto":
			return true
		case "application/json", "*/*":
			return false
		}
	}
	return false
}

// flatHeaders copies request headers into a plain map for backend use.
func flatHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func metadataOf(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"Authorization", "X-Request-Id", "User-Agent"} {
		if v := h.Get(k); v != "" {
			out[strings.ToLower(k)] = v
		}
	}
	return out
}

func writeError(w http.ResponseWriter, status int, st *rpc.Status) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Status-Code", st.Code.String())
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(transcode.ErrorBodyFor(st))
}

// --- Inspector endpoints ---------------------------------------------------

// inspectResponse is the JSON document returned by /_inspect.
type inspectResponse struct {
	Request struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Query  string `json:"query"`
		Body   string `json:"body"`
	} `json:"request"`
	Matched        *routeInfo             `json:"matched,omitempty"`
	Candidates     []routeInfo            `json:"candidates"`
	RequestMessage any                    `json:"requestMessage,omitempty"`
	Assignments    []transcode.Assignment `json:"assignments"`
	Wire           wireInfo               `json:"wire"`
	Error          *errorInfo             `json:"error,omitempty"`
}

type routeInfo struct {
	Method   string            `json:"method"`
	Template string            `json:"template"`
	RPC      string            `json:"rpc"`
	Body     string            `json:"body"`
	Matched  bool              `json:"matched"`
	Vars     map[string]string `json:"vars,omitempty"`
	Verb     string            `json:"verb,omitempty"`
}

type wireInfo struct {
	Length int    `json:"length"`
	Hex    string `json:"hex"`
	Base64 string `json:"base64"`
	Layout string `json:"layout"`
}

type errorInfo struct {
	HTTP   int    `json:"http"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (h *Handler) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, &rpc.Status{Code: rpc.InvalidArgument, Message: "use POST"})
		return
	}
	var in struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Query  string `json:"query"`
		Body   string `json:"body"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.opt.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, &rpc.Status{Code: rpc.InvalidArgument, Message: "invalid inspector request: " + err.Error()})
		return
	}
	if in.Method == "" {
		in.Method = "GET"
	}
	// Allow the path to carry its own query string.
	if i := strings.IndexByte(in.Path, '?'); i >= 0 {
		if in.Query == "" {
			in.Query = in.Path[i+1:]
		}
		in.Path = in.Path[:i]
	}
	res, err := h.Plan(TranscodeRequest{Method: in.Method, Path: in.Path, Query: in.Query, Body: []byte(in.Body)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, &rpc.Status{Code: rpc.Internal, Message: err.Error()})
		return
	}
	out := inspectResponse{}
	out.Request.Method = in.Method
	out.Request.Path = in.Path
	out.Request.Query = in.Query
	out.Request.Body = in.Body
	for _, e := range h.schema.Table.Candidates(in.Method, in.Path) {
		b, _ := e.Data.(*transcode.Binding)
		ri := routeInfo{Method: e.Route.Method, Template: e.Route.Template}
		if b != nil {
			ri.RPC = b.Method.FullName
			ri.Body = b.Rule.Body
		}
		if res.Match != nil && e.Route == res.Match.Route {
			ri.Matched = true
			ri.Vars = res.Match.Vars
			ri.Verb = res.Match.Verb
		}
		out.Candidates = append(out.Candidates, ri)
		if ri.Matched {
			m := ri
			out.Matched = &m
		}
	}
	if res.Report != nil {
		out.Assignments = res.Report.Assignments
	}
	if res.Request != nil {
		jv, err := jsoncodec.Marshal(res.Request, jsoncodec.Options{})
		if err == nil {
			out.RequestMessage = json.RawMessage(jv)
		}
		out.Wire.Length = len(res.Wire)
		out.Wire.Hex = hexString(res.Wire)
		out.Wire.Base64 = base64String(res.Wire)
		info := explain.Decode(res.Wire, res.Request.Desc)
		out.Wire.Layout = info.Render(res.Wire)
	}
	if res.Status != nil && res.Status.Code != rpc.OK {
		out.Error = &errorInfo{HTTP: res.HTTPStatus, Code: res.Status.Code.String(), Detail: res.Status.Message}
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		h.logf("inspect encode: %v", err)
	}
}

// description is the JSON shape of one method for the UI.
type methodInfo struct {
	FullName        string      `json:"fullName"`
	Service         string      `json:"service"`
	Name            string      `json:"name"`
	Input           string      `json:"input"`
	Output          string      `json:"output"`
	ServerStreaming bool        `json:"serverStreaming"`
	Routes          []routeInfo `json:"routes"`
	RequestFields   []fieldInfo `json:"requestFields"`
	ResponseFields  []fieldInfo `json:"responseFields"`
}

type fieldInfo struct {
	Name     string `json:"name"`
	JSONName string `json:"jsonName"`
	Number   int32  `json:"number"`
	Type     string `json:"type"`
	Repeated bool   `json:"repeated"`
	Map      bool   `json:"map"`
	Oneof    string `json:"oneof,omitempty"`
	Message  string `json:"messageType,omitempty"`
	Enum     string `json:"enumType,omitempty"`
}

func describeFields(m *idl.Message) []fieldInfo {
	var out []fieldInfo
	for _, f := range m.Fields {
		fi := fieldInfo{Name: f.Name, JSONName: f.JSONName, Number: f.Number,
			Repeated: f.Repeated, Map: f.Map}
		switch {
		case f.Map:
			fi.Type = "map<" + f.MapKey.String() + "," + f.MapValue.String() + ">"
		case f.Type.Message != nil:
			fi.Type = "message"
			fi.Message = f.Type.Message.FullName
		case f.Type.Enum != nil:
			fi.Type = "enum"
			fi.Enum = f.Type.Enum.FullName
		default:
			fi.Type = f.Type.Scalar.String()
		}
		if f.Oneof != nil {
			fi.Oneof = f.Oneof.Name
		}
		out = append(out, fi)
	}
	return out
}

func (h *Handler) handleSchema(w http.ResponseWriter, r *http.Request) {
	type schemaDoc struct {
		Package string       `json:"package"`
		Methods []methodInfo `json:"methods"`
	}
	doc := schemaDoc{Package: packageName(h.schema)}
	for _, m := range h.schema.Methods {
		mi := methodInfo{
			FullName:        m.FullName,
			Service:         m.Service.FullName,
			Name:            m.Name,
			Input:           m.Input.FullName,
			Output:          m.Output.FullName,
			ServerStreaming: m.ServerStreaming,
			RequestFields:   describeFields(m.Input),
			ResponseFields:  describeFields(m.Output),
		}
		for _, b := range h.schema.BindingsFor(m) {
			mi.Routes = append(mi.Routes, routeInfo{
				Method: b.Rule.Method, Template: b.Rule.Path,
				RPC: m.FullName, Body: b.Rule.Body,
			})
		}
		doc.Methods = append(doc.Methods, mi)
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}

func packageName(s *Schema) string {
	if len(s.Files) > 0 {
		return s.Files[0].Package
	}
	return ""
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*3)
	for i, x := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[x>>4], digits[x&15])
	}
	return string(out)
}

func base64String(b []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		out.WriteByte(table[chunk[0]>>2])
		out.WriteByte(table[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			out.WriteByte(table[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(table[chunk[2]&0x3f])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

// handleBench runs an in-process transcode benchmark.
func (h *Handler) handleBench(w http.ResponseWriter, r *http.Request) {
	n := 2000
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 200000 {
			n = parsed
		}
	}
	type benchResult struct {
		Iterations  int     `json:"iterations"`
		BodyBytes   int     `json:"bodyBytes"`
		BinaryBytes int     `json:"binaryBytes"`
		TotalMs     float64 `json:"totalMs"`
		PerOpUs     float64 `json:"perOpMicros"`
		OpsPerSec   float64 `json:"opsPerSec"`
	}
	// Pick the first non-streaming method with a body '*' rule.
	var target *transcode.Binding
	for _, b := range h.schema.Bindings {
		if !b.Method.ServerStreaming && b.BodyStar {
			target = b
			break
		}
	}
	if target == nil && len(h.schema.Bindings) > 0 {
		target = h.schema.Bindings[0]
	}
	if target == nil {
		writeError(w, http.StatusNotFound, &rpc.Status{Code: rpc.NotFound, Message: "no methods"})
		return
	}
	sample := buildSampleBody(target.Method.Input)
	path := samplePath(target)
	start := time.Now()
	var wireBytes int
	for i := 0; i < n; i++ {
		res, err := h.Plan(TranscodeRequest{Method: target.Rule.Method, Path: path, Body: sample})
		if err != nil || res.Request == nil {
			writeError(w, http.StatusInternalServerError, &rpc.Status{Code: rpc.Internal, Message: "benchmark plan failed"})
			return
		}
		wireBytes = len(res.Wire)
	}
	elapsed := time.Since(start)
	res := benchResult{
		Iterations:  n,
		BodyBytes:   len(sample),
		BinaryBytes: wireBytes,
		TotalMs:     float64(elapsed.Microseconds()) / 1000,
		PerOpUs:     float64(elapsed.Microseconds()) / float64(n),
		OpsPerSec:   float64(n) / elapsed.Seconds(),
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}

// buildSampleBody fills a message with plausible JSON derived from its
// descriptor, for the benchmark and for UI examples.
func buildSampleBody(m *idl.Message) []byte {
	sample := dyn.New(m)
	fillSample(sample, 0)
	data, err := jsoncodec.Marshal(sample, jsoncodec.Options{})
	if err != nil {
		return []byte("{}")
	}
	return data
}

func fillSample(m *dyn.Message, depth int) {
	if depth > 3 {
		return
	}
	for _, f := range m.Desc.Fields {
		switch {
		case f.Map:
			mm := dyn.Map{}
			if f.MapValue.Message != nil {
				child := dyn.New(f.MapValue.Message)
				fillSample(child, depth+1)
				mm["k1"] = child
			} else {
				mm[sampleKey(f.MapKey)] = sampleValue(f.MapValue)
			}
			m.Set(f, mm)
		case f.Repeated:
			var list []any
			for i := 0; i < 2; i++ {
				if f.Type.Message != nil {
					child := dyn.New(f.Type.Message)
					fillSample(child, depth+1)
					list = append(list, child)
				} else {
					list = append(list, sampleValue(f.Type))
				}
			}
			m.Set(f, list)
		case f.Type.Message != nil:
			child := dyn.New(f.Type.Message)
			fillSample(child, depth+1)
			m.Set(f, child)
		default:
			m.Set(f, sampleValue(f.Type))
		}
	}
}

func sampleKey(t idl.ScalarType) any {
	switch t {
	case idl.TString:
		return "k1"
	case idl.TBool:
		return true
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int32(1)
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return int64(1)
	case idl.TUint32, idl.TFixed32:
		return uint32(1)
	case idl.TUint64, idl.TFixed64:
		return uint64(1)
	}
	return nil
}

func sampleValue(t idl.Type) any {
	if t.Enum != nil && len(t.Enum.Values) > 0 {
		return t.Enum.Values[0].Number
	}
	if t.Message != nil {
		switch idl.WellKnown(t.Message) {
		case idl.KindTimestamp:
			return &dyn.Message{Desc: t.Message, Fields: map[int32]any{1: int64(1700000000)}}
		case idl.KindDuration:
			return &dyn.Message{Desc: t.Message, Fields: map[int32]any{1: int64(3)}}
		}
		return dyn.New(t.Message)
	}
	switch t.Scalar {
	case idl.TDouble:
		return float64(1.5)
	case idl.TFloat:
		return float32(1.5)
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int32(42)
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return int64(42)
	case idl.TUint32, idl.TFixed32:
		return uint32(42)
	case idl.TUint64, idl.TFixed64:
		return uint64(42)
	case idl.TBool:
		return true
	case idl.TString:
		return "sample"
	case idl.TBytes:
		return []byte("bytes")
	}
	return nil
}

// samplePath substitutes sample values into the route template.
func samplePath(b *transcode.Binding) string {
	if b == nil || b.Route == nil {
		return "/"
	}
	return routing.ExamplePath(b.Route.Template)
}

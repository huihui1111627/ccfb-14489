// Package backend provides a fake backend service set used by the gateway in
// place of real gRPC servers. The handlers are driven entirely by the IDL:
// they echo, reflect and generate data according to the matched method's
// request/response descriptors, so new methods need no Go code.
package backend

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/rpc"
	"example.com/idlbridge/internal/wire"
)

// Fake is a simulated service.
type Fake struct {
	Registry *idl.Registry
	// Now lets tests pin the clock for timestamp generation.
	Now func() time.Time
}

// New creates a fake backend. Handlers are registered for every method in the
// registry, using the method's leading path segment as the service family.
func New(reg *idl.Registry) *Fake {
	return &Fake{Registry: reg, Now: time.Now}
}

// Router builds a backend router covering every method in the registry.
func (f *Fake) Router() *rpc.Router {
	r := rpc.NewRouter()
	for _, m := range f.Registry.Methods() {
		m := m
		if m.ServerStreaming {
			r.HandleStream(m.FullName, func(ctx context.Context, req *rpc.Request, send func(*dyn.Message) error) error {
				return f.Stream(ctx, req, send)
			})
			continue
		}
		r.Handle(m.FullName, func(ctx context.Context, req *rpc.Request) (*dyn.Message, error) {
			return f.Unary(ctx, req)
		})
	}
	return r
}

// Unary implements a deterministic handler: if the request has a field named
// "fail_with_status" or the method name starts with a fault-injection prefix,
// an error is returned. Otherwise the response is generated from the request.
func (f *Fake) Unary(ctx context.Context, req *rpc.Request) (*dyn.Message, error) {
	if err := injectedError(req); err != nil {
		return nil, err
	}
	return f.Generate(req.Method, req.Message, 0), nil
}

// Stream emits a sequence of generated responses.
func (f *Fake) Stream(ctx context.Context, req *rpc.Request, send func(*dyn.Message) error) error {
	if err := injectedError(req); err != nil {
		return err
	}
	n := 3
	for _, name := range []string{"count", "limit", "page_size"} {
		if fl := req.Message.Desc.FieldByName(name); fl != nil {
			if v, ok := req.Message.Get(fl); ok {
				switch x := v.(type) {
				case int32:
					n = int(x)
				case int64:
					n = int(x)
				case uint32:
					n = int(x)
				case uint64:
					n = int(x)
				}
				break
			}
		}
	}
	if n < 0 {
		n = 0
	}
	if n > 1000 {
		n = 1000
	}
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := send(f.Generate(req.Method, req.Message, i)); err != nil {
			return err
		}
	}
	return nil
}

func injectedError(req *rpc.Request) error {
	// HTTP callers choose the error through a request header, which the
	// gateway records in the message origin. gRPC metadata conventions are
	// mirrored: any header starting with "x-fail-status" names a status.
	if req.Message != nil && req.Message.Origin != nil {
		if hdrs, ok := req.Message.Origin["headers"].(map[string]string); ok {
			for k, v := range hdrs {
				if strings.HasPrefix(strings.ToLower(k), "x-fail-status") {
					if code, err := ParseCode(v); err == nil && code != rpc.OK {
						return &rpc.Status{Code: code, Message: fmt.Sprintf("simulated %s for %s", code, req.Method.Name)}
					}
				}
			}
		}
	}
	for _, name := range []string{"fail_with_status", "failWithStatus", "status_code"} {
		fl := req.Message.Desc.FieldByName(name)
		if fl == nil {
			continue
		}
		v, ok := req.Message.Get(fl)
		if !ok {
			continue
		}
		var code rpc.Code
		switch x := v.(type) {
		case int32:
			code = rpc.Code(x)
		case int64:
			code = rpc.Code(x)
		case string:
			c, err := ParseCode(x)
			if err != nil {
				continue
			}
			code = c
		default:
			continue
		}
		if code == 0 {
			continue
		}
		return &rpc.Status{Code: code, Message: fmt.Sprintf("simulated %s for %s", code, req.Method.Name)}
	}
	return nil
}

// ParseCode parses a status name such as "NOT_FOUND".
func ParseCode(s string) (rpc.Code, error) {
	s = strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
	for c := rpc.OK; c <= rpc.Unauthenticated; c++ {
		if c.String() == s {
			return c, nil
		}
	}
	return 0, fmt.Errorf("unknown status %q", s)
}

// Generate synthesises a response for a method from its request. Field values
// are deterministic functions of the request so responses are repeatable.
func (f *Fake) Generate(m *idl.Method, req *dyn.Message, index int) *dyn.Message {
	out := dyn.New(m.Output)
	h := fnv.New64a()
	if raw, err := wire.Marshal(req); err == nil {
		h.Write(raw)
	}
	seed := h.Sum64()
	fillMessage(out, req, m, seed, index, f.Now(), 0)
	return out
}

// fillMessage fills every field of dst. Fields that exist on the request with
// compatible names/types are copied; otherwise a deterministic value is
// synthesised.
func fillMessage(dst, req *dyn.Message, m *idl.Method, seed uint64, index int, now time.Time, depth int) {
	if depth > 8 {
		return
	}
	for _, f := range dst.Desc.Fields {
		// Prefer an identically named request field.
		if src := req.Desc.FieldByName(f.Name); src != nil && !f.Map && !f.Repeated {
			if v, ok := req.Get(src); ok && compatible(f.Type, src.Type) {
				dst.Set(f, dyn.CloneValue(v))
				continue
			}
		}
		switch {
		case f.Map:
			mm := dyn.Map{}
			for i := 0; i < 2; i++ {
				k := fakeKey(f.MapKey, seed, i)
				if f.MapValue.Message != nil {
					child := dyn.New(f.MapValue.Message)
					fillMessage(child, req, m, seed, i, now, depth+1)
					mm[k] = child
				} else {
					mm[k] = fakeValue(f.MapValue, seed, i, now)
				}
			}
			dst.Set(f, mm)
		case f.Repeated:
			n := 2
			if f.Type.Message == nil {
				n = 3
			}
			list := make([]any, 0, n)
			for i := 0; i < n; i++ {
				if f.Type.Message != nil {
					child := dyn.New(f.Type.Message)
					fillMessage(child, req, m, seed, i, now, depth+1)
					list = append(list, child)
				} else {
					list = append(list, fakeValue(f.Type, seed, i, now))
				}
			}
			dst.Set(f, list)
		case f.Type.Message != nil:
			child := dyn.New(f.Type.Message)
			fillMessage(child, req, m, seed, index, now, depth+1)
			dst.Set(f, child)
		default:
			dst.Set(f, fakeValue(f.Type, seed, index, now))
		}
	}
}

func compatible(dst, src idl.Type) bool {
	if dst.Message != nil || src.Message != nil {
		return dst.Message == src.Message
	}
	if dst.Enum != nil || src.Enum != nil {
		return dst.Enum == src.Enum
	}
	return dst.Scalar == src.Scalar
}

// uindex reduces v modulo n, tolerating negative values.
func uindex(v int64, n int) int {
	if n <= 0 {
		return 0
	}
	m := v % int64(n)
	if m < 0 {
		m += int64(n)
	}
	return int(m)
}

func fakeValue(t idl.Type, seed uint64, index int, now time.Time) any {
	switch {
	case t.Enum != nil:
		if len(t.Enum.Values) == 0 {
			return int32(0)
		}
		// Fake values are often negative (hashes), so reduce the modulus
		// into range before indexing.
		return t.Enum.Values[uindex(int64(seed)+int64(index)*2654435761, len(t.Enum.Values))].Number
	case t.Message != nil:
		switch idl.WellKnown(t.Message) {
		case idl.KindTimestamp:
			return &dyn.Message{Desc: t.Message, Fields: map[int32]any{
				1: now.Add(time.Duration(index) * time.Minute).Unix(),
				2: int32(0),
			}}
		case idl.KindDuration:
			return &dyn.Message{Desc: t.Message, Fields: map[int32]any{
				1: int64(1 + index),
				2: int32(500_000_000),
			}}
		}
		return dyn.New(t.Message)
	}
	n := seed + uint64(index)*2654435761
	switch t.Scalar {
	case idl.TDouble:
		return float64(n%1000) / 8
	case idl.TFloat:
		return float32(n%1000) / 8
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int32(n % 1000)
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return int64(n % 1_000_000)
	case idl.TUint32, idl.TFixed32:
		return uint32(n % 1000)
	case idl.TUint64, idl.TFixed64:
		return n % 1_000_000
	case idl.TBool:
		return n%2 == 0
	case idl.TString:
		return "sim-" + strconv.FormatUint(n%100000, 10)
	case idl.TBytes:
		return []byte{byte(n), byte(n >> 8), byte(n >> 16)}
	}
	return nil
}

func fakeKey(t idl.ScalarType, seed uint64, i int) any {
	n := seed + uint64(i)
	switch t {
	case idl.TString:
		return "key-" + strconv.FormatUint(n%97, 10)
	case idl.TBool:
		return i%2 == 0
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int32(n % 100)
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return int64(n % 100)
	case idl.TUint32, idl.TFixed32:
		return uint32(n % 100)
	case idl.TUint64, idl.TFixed64:
		return n % 100
	}
	return nil
}

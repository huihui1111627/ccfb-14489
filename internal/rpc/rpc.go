// Package rpc defines the boundary between the gateway and backend services.
package rpc

import (
	"context"
	"fmt"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
)

// Request is one invocation of a backend method.
type Request struct {
	Method *idl.Method
	// Message is the decoded request.
	Message *dyn.Message
	// Metadata carries headers forwarded from the gateway.
	Metadata map[string]string
}

// Status is a backend error, in the style of gRPC status codes.
type Status struct {
	Code    Code
	Message string
	// Details is an optional machine-readable payload.
	Details map[string]any
}

func (s *Status) Error() string {
	return fmt.Sprintf("%s: %s", s.Code, s.Message)
}

// Code enumerates backend status codes. The numeric values match the
// conventional gRPC numbering so that they can be mapped to HTTP status
// deterministically.
type Code int

const (
	OK                 Code = 0
	Canceled           Code = 1
	Unknown            Code = 2
	InvalidArgument    Code = 3
	DeadlineExceeded   Code = 4
	NotFound           Code = 5
	AlreadyExists      Code = 6
	PermissionDenied   Code = 7
	ResourceExhausted  Code = 8
	FailedPrecondition Code = 9
	Aborted            Code = 10
	OutOfRange         Code = 11
	Unimplemented      Code = 12
	Internal           Code = 13
	Unavailable        Code = 14
	DataLoss           Code = 15
	Unauthenticated    Code = 16
)

var codeNames = map[Code]string{
	OK: "OK", Canceled: "CANCELLED", Unknown: "UNKNOWN",
	InvalidArgument: "INVALID_ARGUMENT", DeadlineExceeded: "DEADLINE_EXCEEDED",
	NotFound: "NOT_FOUND", AlreadyExists: "ALREADY_EXISTS",
	PermissionDenied: "PERMISSION_DENIED", ResourceExhausted: "RESOURCE_EXHAUSTED",
	FailedPrecondition: "FAILED_PRECONDITION", Aborted: "ABORTED",
	OutOfRange: "OUT_OF_RANGE", Unimplemented: "UNIMPLEMENTED",
	Internal: "INTERNAL", Unavailable: "UNAVAILABLE", DataLoss: "DATA_LOSS",
	Unauthenticated: "UNAUTHENTICATED",
}

func (c Code) String() string {
	if n, ok := codeNames[c]; ok {
		return n
	}
	return fmt.Sprintf("CODE(%d)", int(c))
}

// NewStatus builds an error status.
func NewStatus(c Code, format string, args ...any) *Status {
	return &Status{Code: c, Message: fmt.Sprintf(format, args...)}
}

// Backend is the interface implemented by simulated services.
type Backend interface {
	// Unary invokes a unary method.
	Unary(ctx context.Context, req *Request) (*dyn.Message, error)
	// Stream invokes a server-streaming method, sending each response to
	// send. Returning a Status error terminates the stream with an error.
	Stream(ctx context.Context, req *Request, send func(*dyn.Message) error) error
}

// HandlerFunc adapts a function to a unary backend call.
type HandlerFunc func(ctx context.Context, req *Request) (*dyn.Message, error)

// Router dispatches requests to registered handlers.
type Router struct {
	handlers map[string]HandlerFunc
	streams  map[string]StreamFunc
}

// StreamFunc implements a streaming method.
type StreamFunc func(ctx context.Context, req *Request, send func(*dyn.Message) error) error

// NewRouter creates an empty backend router.
func NewRouter() *Router {
	return &Router{handlers: map[string]HandlerFunc{}, streams: map[string]StreamFunc{}}
}

// Handle registers a unary handler for a fully-qualified method name.
func (r *Router) Handle(fullName string, h HandlerFunc) { r.handlers[fullName] = h }

// HandleStream registers a streaming handler.
func (r *Router) HandleStream(fullName string, h StreamFunc) { r.streams[fullName] = h }

// Unary implements Backend.
func (r *Router) Unary(ctx context.Context, req *Request) (*dyn.Message, error) {
	if h, ok := r.handlers[req.Method.FullName]; ok {
		return h(ctx, req)
	}
	if _, ok := r.streams[req.Method.FullName]; ok {
		return nil, NewStatus(InvalidArgument, "%s is a server-streaming method; use the streaming endpoint", req.Method.FullName)
	}
	return nil, NewStatus(Unimplemented, "no handler for %s", req.Method.FullName)
}

// Stream implements Backend.
func (r *Router) Stream(ctx context.Context, req *Request, send func(*dyn.Message) error) error {
	if h, ok := r.streams[req.Method.FullName]; ok {
		return h(ctx, req, send)
	}
	if _, ok := r.handlers[req.Method.FullName]; ok {
		msg, err := r.Unary(ctx, req)
		if err != nil {
			return err
		}
		return send(msg)
	}
	return NewStatus(Unimplemented, "no handler for %s", req.Method.FullName)
}

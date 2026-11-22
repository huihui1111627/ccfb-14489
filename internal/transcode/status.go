package transcode

import (
	"errors"
	"net/http"

	"example.com/idlbridge/internal/rpc"
)

// HTTPStatus maps a backend status code to an HTTP status, following the
// google.api.http error mapping.
func HTTPStatus(c rpc.Code) int {
	switch c {
	case rpc.OK:
		return http.StatusOK
	case rpc.Canceled:
		return 499 // client closed request; not in net/http constants
	case rpc.Unknown:
		return http.StatusInternalServerError
	case rpc.InvalidArgument:
		return http.StatusBadRequest
	case rpc.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case rpc.NotFound:
		return http.StatusNotFound
	case rpc.AlreadyExists:
		return http.StatusConflict
	case rpc.PermissionDenied:
		return http.StatusForbidden
	case rpc.ResourceExhausted:
		return http.StatusTooManyRequests
	case rpc.FailedPrecondition:
		return http.StatusBadRequest
	case rpc.Aborted:
		return http.StatusConflict
	case rpc.OutOfRange:
		return http.StatusBadRequest
	case rpc.Unimplemented:
		return http.StatusNotImplemented
	case rpc.Internal:
		return http.StatusInternalServerError
	case rpc.Unavailable:
		return http.StatusServiceUnavailable
	case rpc.DataLoss:
		return http.StatusInternalServerError
	case rpc.Unauthenticated:
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

// ErrorBody is the JSON representation of a failure.
type ErrorBody struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Status  string         `json:"status"`
	Details map[string]any `json:"details,omitempty"`
}

// StatusOf extracts a Status from an error, defaulting to UNKNOWN.
func StatusOf(err error) *rpc.Status {
	if err == nil {
		return &rpc.Status{Code: rpc.OK}
	}
	var st *rpc.Status
	if errors.As(err, &st) {
		return st
	}
	return &rpc.Status{Code: rpc.Unknown, Message: err.Error()}
}

// ErrorBodyFor renders the JSON error document for a status.
func ErrorBodyFor(st *rpc.Status) ErrorBody {
	return ErrorBody{
		Code:    int(st.Code),
		Message: st.Message,
		Status:  st.Code.String(),
		Details: st.Details,
	}
}

// ErrorStatusFor maps a binding failure to an HTTP status and status code.
func ErrorStatusFor(err error) (int, *rpc.Status) {
	var be *BindingError
	if errors.As(err, &be) {
		switch be.Kind {
		case "path":
			return http.StatusBadRequest, &rpc.Status{Code: rpc.InvalidArgument, Message: be.Error()}
		case "query":
			return http.StatusBadRequest, &rpc.Status{Code: rpc.InvalidArgument, Message: be.Error()}
		case "body":
			return http.StatusBadRequest, &rpc.Status{Code: rpc.InvalidArgument, Message: be.Error()}
		}
	}
	return http.StatusInternalServerError, &rpc.Status{Code: rpc.Internal, Message: err.Error()}
}

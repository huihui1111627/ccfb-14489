package jsoncodec

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
)

// ValueToJSON renders one typed value using the JSON mapping.
func ValueToJSON(t idl.Type, v any, opt Options) (any, error) {
	return valueToJSON(t, v, opt)
}

// JSONToValue parses one typed value from decoded JSON.
func JSONToValue(t idl.Type, raw any, opt Options) (any, error) {
	return jsonToValue(t, raw, opt, "$")
}

// StringToValue converts a plain string (path variable or query parameter)
// into a typed value. Strings, bytes, enums, well-known scalar wrappers and
// numbers are all handled; a message type other than a well-known type is
// rejected because it cannot be expressed as a single path segment.
func StringToValue(t idl.Type, s string, opt Options) (any, error) {
	if t.Enum != nil {
		if ev := t.Enum.ValueByName(s); ev != nil {
			return ev.Number, nil
		}
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("unknown enum value %q for %s", s, t.Enum.FullName)
		}
		return int32(n), nil
	}
	if t.Message != nil {
		switch idl.WellKnown(t.Message) {
		case idl.KindTimestamp, idl.KindDuration, idl.KindFieldMask, idl.KindWrapper:
			return jsonToValue(t, s, opt, "$")
		case idl.KindStruct, idl.KindValue, idl.KindListValue:
			var raw any
			dec := json.NewDecoder(strings.NewReader(s))
			dec.UseNumber()
			if err := dec.Decode(&raw); err != nil {
				return nil, fmt.Errorf("invalid JSON in parameter %q: %w", s, err)
			}
			return jsonToValue(t, raw, opt, "$")
		}
		return nil, fmt.Errorf("message type %s cannot be bound from a path or query parameter", t.Message.FullName)
	}
	switch t.Scalar {
	case idl.TString:
		return s, nil
	case idl.TBytes:
		b, err := decodeBase64(s)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 for bytes: %w", err)
		}
		return b, nil
	case idl.TBool:
		switch strings.ToLower(s) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
		return nil, fmt.Errorf("invalid bool %q", s)
	case idl.TDouble, idl.TFloat:
		var f float64
		switch s {
		case "NaN":
			f = math.NaN()
		case "Infinity", "+Infinity":
			f = math.Inf(1)
		case "-Infinity":
			f = math.Inf(-1)
		default:
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid float %q", s)
			}
			f = v
		}
		if t.Scalar == idl.TFloat {
			return float32(f), nil
		}
		return f, nil
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid int32 %q", s)
		}
		return int32(n), nil
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid int64 %q", s)
		}
		return n, nil
	case idl.TUint32, idl.TFixed32:
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid uint32 %q", s)
		}
		return uint32(n), nil
	case idl.TUint64, idl.TFixed64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid uint64 %q", s)
		}
		return n, nil
	}
	return nil, fmt.Errorf("unsupported scalar %v", t.Scalar)
}

// MarshalValue renders a single typed value as JSON, for diagnostics.
func MarshalValue(t idl.Type, v any, opt Options) ([]byte, error) {
	jv, err := valueToJSON(t, v, opt)
	if err != nil {
		return nil, err
	}
	return json.Marshal(jv)
}

// ValueString renders a canonical scalar value for human display.
func ValueString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case []byte:
		return "0x" + fmt.Sprintf("%x", x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case bool:
		return strconv.FormatBool(x)
	case *dyn.Message:
		return x.Desc.FullName
	case []any:
		return fmt.Sprintf("[%d items]", len(x))
	case dyn.Map:
		return fmt.Sprintf("{%d entries}", len(x))
	default:
		return fmt.Sprint(x)
	}
}

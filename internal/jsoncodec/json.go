// Package jsoncodec implements the proto3 JSON mapping for dynamic messages.
//
// Supported special cases: field names in either lowerCamelCase or the
// original snake_case (both accepted, camelCase emitted), enums as names,
// 64-bit integers as strings, bytes as base64, Timestamp/Duration/FieldMask/
// Struct/Value/ListValue/Any and the wrapper types, omission of default
// values, and configurable unknown-field handling.
package jsoncodec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
)

// UnknownPolicy selects how unrecognised JSON fields are treated.
type UnknownPolicy int

const (
	// UnknownReject fails the conversion (the default, matching proto3 JSON).
	UnknownReject UnknownPolicy = iota
	// UnknownIgnore silently drops unknown fields.
	UnknownIgnore
	// UnknownPreserve keeps unknown fields so they survive a round trip.
	UnknownPreserve
)

// Options controls JSON conversion.
type Options struct {
	Unknown UnknownPolicy
	// EmitDefaults writes default values too (normally omitted).
	EmitDefaults bool
	// UseProtoNames emits snake_case field names instead of lowerCamelCase.
	UseProtoNames bool
	// UseEnumNumbers emits enum numbers instead of names.
	UseEnumNumbers bool
	// Indent pretty-prints output when non-empty.
	Indent string
}

// Error is a conversion error with a JSON path for diagnostics.
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return "json: " + e.Msg
	}
	return "json at " + e.Path + ": " + e.Msg
}

func errAt(path, format string, args ...any) error {
	return &Error{Path: path, Msg: fmt.Sprintf(format, args...)}
}

// Marshal converts a dynamic message to JSON.
func Marshal(m *dyn.Message, opt Options) ([]byte, error) {
	v, err := messageToJSON(m, opt)
	if err != nil {
		return nil, err
	}
	if opt.Indent != "" {
		return json.MarshalIndent(v, "", opt.Indent)
	}
	return json.Marshal(v)
}

// Unmarshal parses JSON into a dynamic message of the given type.
func Unmarshal(data []byte, desc *idl.Message, opt Options) (*dyn.Message, error) {
	var raw any
	if err := decodeJSON(data, &raw); err != nil {
		return nil, &Error{Msg: err.Error()}
	}
	return jsonToMessage(raw, desc, opt, "$")
}

func messageToJSON(m *dyn.Message, opt Options) (any, error) {
	if m == nil {
		return nil, nil
	}
	switch idl.WellKnown(m.Desc) {
	case idl.KindTimestamp:
		return timestampToJSON(m)
	case idl.KindDuration:
		return durationToJSON(m)
	case idl.KindFieldMask:
		return fieldMaskToJSON(m)
	case idl.KindStruct:
		return structToJSON(m, opt)
	case idl.KindValue:
		return valueMessageToJSON(m, opt)
	case idl.KindListValue:
		return listValueToJSON(m, opt)
	case idl.KindWrapper:
		return wrapperToJSON(m, opt)
	case idl.KindEmpty:
		return map[string]any{}, nil
	case idl.KindAny:
		return anyToJSON(m, opt)
	}
	out := make(map[string]any, len(m.Fields)+1)
	for _, f := range m.Desc.SortedFields() {
		v, ok := m.Fields[f.Number]
		if !ok {
			continue
		}
		name := f.JSONName
		if opt.UseProtoNames {
			name = f.Name
		}
		if !opt.EmitDefaults {
			if f.Repeated || f.Map {
				if isEmptyList(v) {
					continue
				}
			} else if !f.HasPresence() && dyn.IsDefault(f, v) {
				continue
			}
		}
		jv, err := fieldToJSON(f, v, opt)
		if err != nil {
			return nil, errAt("$."+f.Name, "%s", err)
		}
		if jv == nil && !f.HasPresence() {
			continue
		}
		out[name] = jv
	}
	if opt.Unknown == UnknownPreserve && len(m.JSONUnknown) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(m.JSONUnknown, &extra); err == nil {
			for k, v := range extra {
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}
		}
	}
	return out, nil
}

func isEmptyList(v any) bool {
	switch x := v.(type) {
	case []any:
		return len(x) == 0
	case dyn.Map:
		return len(x) == 0
	}
	return false
}

func fieldToJSON(f *idl.Field, v any, opt Options) (any, error) {
	if f.Map {
		mm, ok := v.(dyn.Map)
		if !ok {
			return nil, fmt.Errorf("expected map, got %T", v)
		}
		out := make(map[string]any, len(mm))
		for _, k := range dyn.SortedMapKeys(mm, f.MapKey) {
			ks, err := mapKeyToString(k, f.MapKey)
			if err != nil {
				return nil, err
			}
			// A nil or untyped entry is an empty message, not JSON null.
			if mm[k] == nil && f.MapValue.Message != nil {
				mm[k] = dyn.New(f.MapValue.Message)
			} else if mvv, ok := mm[k].(*dyn.Message); ok && mvv == nil {
				mm[k] = dyn.New(f.MapValue.Message)
			}
			jv, err := valueToJSON(f.MapValue, mm[k], opt)
			if err != nil {
				return nil, err
			}
			out[ks] = jv
		}
		return out, nil
	}
	if f.Repeated {
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("expected repeated, got %T", v)
		}
		out := make([]any, len(list))
		for i, e := range list {
			jv, err := valueToJSON(f.Type, e, opt)
			if err != nil {
				return nil, err
			}
			out[i] = jv
		}
		return out, nil
	}
	return valueToJSON(f.Type, v, opt)
}

func valueToJSON(t idl.Type, v any, opt Options) (any, error) {
	if t.Message != nil {
		m, _ := v.(*dyn.Message)
		if m == nil {
			return nil, nil
		}
		return messageToJSON(m, opt)
	}
	if t.Enum != nil {
		n, ok := v.(int32)
		if !ok {
			return nil, fmt.Errorf("expected enum, got %T", v)
		}
		if opt.UseEnumNumbers {
			return int64(n), nil
		}
		ev := t.Enum.ValueByNumber(n)
		if ev == nil {
			// Unknown enum values are emitted as numbers, per the spec.
			return int64(n), nil
		}
		return ev.Name, nil
	}
	switch t.Scalar {
	case idl.TDouble:
		return floatJSON(float64(v.(float64)), 64)
	case idl.TFloat:
		return floatJSON(float64(v.(float32)), 32)
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int64(v.(int32)), nil
	case idl.TUint32, idl.TFixed32:
		return uint64(v.(uint32)), nil
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return strconv.FormatInt(v.(int64), 10), nil
	case idl.TUint64, idl.TFixed64:
		return strconv.FormatUint(v.(uint64), 10), nil
	case idl.TBool:
		return v.(bool), nil
	case idl.TString:
		return v.(string), nil
	case idl.TBytes:
		return base64.StdEncoding.EncodeToString(v.([]byte)), nil
	}
	return nil, fmt.Errorf("unsupported scalar %v", t.Scalar)
}

func floatJSON(f float64, bits int) (any, error) {
	if math.IsNaN(f) {
		return "NaN", nil
	}
	if math.IsInf(f, 1) {
		return "Infinity", nil
	}
	if math.IsInf(f, -1) {
		return "-Infinity", nil
	}
	if bits == 32 {
		// json.Marshal would print the exact float64 value of the float32,
		// e.g. 1.100000023841858. The shortest round-tripping float32
		// representation (1.1) is what the spec's examples show.
		return json.Number(strconv.FormatFloat(f, 'g', -1, 32)), nil
	}
	return f, nil
}

func mapKeyToString(k any, t idl.ScalarType) (string, error) {
	switch t {
	case idl.TString:
		return k.(string), nil
	case idl.TBool:
		if k.(bool) {
			return "true", nil
		}
		return "false", nil
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return strconv.FormatInt(int64(k.(int32)), 10), nil
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return strconv.FormatInt(k.(int64), 10), nil
	case idl.TUint32, idl.TFixed32:
		return strconv.FormatUint(uint64(k.(uint32)), 10), nil
	case idl.TUint64, idl.TFixed64:
		return strconv.FormatUint(k.(uint64), 10), nil
	}
	return "", fmt.Errorf("invalid map key type %v", t)
}

func parseMapKey(s string, t idl.ScalarType) (any, error) {
	switch t {
	case idl.TString:
		return s, nil
	case idl.TBool:
		switch s {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("invalid bool map key %q", s)
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		n, err := strconv.ParseInt(s, 10, 32)
		return int32(n), err
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return strconv.ParseInt(s, 10, 64)
	case idl.TUint32, idl.TFixed32:
		n, err := strconv.ParseUint(s, 10, 32)
		return uint32(n), err
	case idl.TUint64, idl.TFixed64:
		return strconv.ParseUint(s, 10, 64)
	}
	return nil, fmt.Errorf("invalid map key type %v", t)
}

func jsonToMessage(raw any, desc *idl.Message, opt Options, path string) (*dyn.Message, error) {
	if raw == nil {
		return nil, nil
	}
	switch idl.WellKnown(desc) {
	case idl.KindTimestamp:
		return jsonToTimestamp(raw, desc, path)
	case idl.KindDuration:
		return jsonToDuration(raw, desc, path)
	case idl.KindFieldMask:
		return jsonToFieldMask(raw, desc, path)
	case idl.KindStruct:
		return jsonToStruct(raw, desc, opt, path)
	case idl.KindValue:
		return jsonToValueMessage(raw, desc, opt, path)
	case idl.KindListValue:
		return jsonToListValue(raw, desc, opt, path)
	case idl.KindWrapper:
		return jsonToWrapper(raw, desc, opt, path)
	case idl.KindEmpty:
		if _, ok := raw.(map[string]any); !ok {
			return nil, errAt(path, "expected object for google.protobuf.Empty")
		}
		return dyn.New(desc), nil
	case idl.KindAny:
		return jsonToAny(raw, desc, opt, path)
	}
	obj, ok := raw.(mapOrList)
	if !ok {
		return nil, errAt(path, "expected JSON object for message %s", desc.FullName)
	}
	m := dyn.New(desc)
	// Detect duplicate fields spelled with different accepted names
	// ("book_name" and "bookName"). encoding/json keeps both object keys, so
	// this is a faithful check of the input document.
	fieldHits := map[int32]string{}
	// Preserve unknown fields verbatim by number so they survive round trips.
	// Keys are sorted to keep output stable.
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var preserved map[string]any
	for _, key := range keys {
		val := obj[key]
		f := desc.FieldByName(key)
		if f == nil {
			// Also accept a snake_case spelling that was overridden by
			// json_name, and vice versa.
			f = desc.FieldByName(toSnake(key))
			if f == nil {
				f = desc.FieldByName(idl.JSONName(key))
			}
		}
		child := path + "." + key
		if f == nil {
			switch opt.Unknown {
			case UnknownReject:
				return nil, errAt(child, "unknown field %q in %s", key, desc.FullName)
			case UnknownIgnore:
				continue
			case UnknownPreserve:
				if preserved == nil {
					preserved = map[string]any{}
				}
				preserved[key] = val
				continue
			}
		}
		if prev, dup := fieldHits[f.Number]; dup {
			return nil, errAt(child, "field %q appears more than once (also as %q)", key, prev)
		}
		fieldHits[f.Number] = key
		if f.Oneof != nil {
			for _, other := range f.Oneof.Fields {
				if other == f {
					continue
				}
				if prev, dup := fieldHits[other.Number]; dup {
					return nil, errAt(child, "field %q is in oneof %q alongside %q", key, f.Oneof.Name, prev)
				}
			}
		}
		fv, err := jsonToFieldValue(f, val, opt, child)
		if err != nil {
			return nil, err
		}
		if fv != nil {
			if f.Oneof != nil {
				for _, other := range f.Oneof.Fields {
					if other != f {
						delete(m.Fields, other.Number)
					}
				}
			}
			m.Set(f, fv)
		}
	}
	if len(preserved) > 0 {
		raw, err := json.Marshal(preserved)
		if err != nil {
			return nil, errAt(path, "%s", err)
		}
		m.JSONUnknown = raw
	}
	return m, nil
}

func toSnake(s string) string { return idl.SnakeName(s) }

func jsonToFieldValue(f *idl.Field, raw any, opt Options, path string) (any, error) {
	if f.Map {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, errAt(path, "expected object for map field %s", f.Name)
		}
		out := dyn.Map{}
		for ks, v := range obj {
			k, err := parseMapKey(ks, f.MapKey)
			if err != nil {
				return nil, errAt(path, "invalid map key %q: %s", ks, err)
			}
			vv, err := jsonToValue(f.MapValue, v, opt, path+"["+strconv.Quote(ks)+"]")
			if err != nil {
				return nil, err
			}
			if vv == nil && f.MapValue.Message != nil {
				vv = dyn.New(f.MapValue.Message)
			}
			if vv == nil {
				vv = zeroForType(f.MapValue)
			}
			out[k] = vv
		}
		return out, nil
	}
	if f.Repeated {
		arr, ok := raw.([]any)
		if !ok {
			return nil, errAt(path, "expected array for repeated field %s", f.Name)
		}
		out := make([]any, 0, len(arr))
		for i, e := range arr {
			v, err := jsonToValue(f.Type, e, opt, path+"["+strconv.Itoa(i)+"]")
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return jsonToValue(f.Type, raw, opt, path)
}

func zeroForType(t idl.Type) any {
	if t.Enum != nil {
		return int32(0)
	}
	if t.Message != nil {
		return nil
	}
	f := &idl.Field{Type: t}
	return f.Default()
}

func jsonToValue(t idl.Type, raw any, opt Options, path string) (any, error) {
	if t.Message != nil {
		if raw == nil {
			return nil, nil
		}
		m, err := jsonToMessage(raw, t.Message, opt, path)
		if err != nil {
			return nil, err
		}
		if m == nil {
			m = dyn.New(t.Message)
		}
		return m, nil
	}
	if t.Enum != nil {
		switch x := raw.(type) {
		case string:
			ev := t.Enum.ValueByName(x)
			if ev == nil {
				return nil, errAt(path, "unknown enum value %q for enum %s", x, t.Enum.FullName)
			}
			return ev.Number, nil
		case json.Number:
			n, err := strconv.ParseInt(string(x), 10, 32)
			if err != nil {
				return nil, errAt(path, "invalid enum number %q", x)
			}
			return int32(n), nil
		}
		return nil, errAt(path, "expected enum name or number, got %T", raw)
	}
	if raw == nil {
		return nil, errAt(path, "null is not a valid %s value", t.Scalar)
	}
	switch t.Scalar {
	case idl.TDouble, idl.TFloat:
		f, err := parseFloat(raw)
		if err != nil {
			return nil, errAt(path, "%s", err)
		}
		if t.Scalar == idl.TFloat {
			return float32(f), nil
		}
		return f, nil
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		n, err := parseInt(raw, 32, true)
		if err != nil {
			return nil, errAt(path, "%s", err)
		}
		return int32(n), nil
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		n, err := parseInt(raw, 64, true)
		if err != nil {
			return nil, errAt(path, "%s", err)
		}
		return n, nil
	case idl.TUint32, idl.TFixed32:
		n, err := parseInt(raw, 32, false)
		if err != nil {
			return nil, errAt(path, "%s", err)
		}
		return uint32(n), nil
	case idl.TUint64, idl.TFixed64:
		n, err := parseInt(raw, 64, false)
		if err != nil {
			return nil, errAt(path, "%s", err)
		}
		return uint64(n), nil
	case idl.TBool:
		b, ok := raw.(bool)
		if !ok {
			return nil, errAt(path, "expected bool, got %T", raw)
		}
		return b, nil
	case idl.TString:
		s, ok := raw.(string)
		if !ok {
			return nil, errAt(path, "expected string, got %T", raw)
		}
		return s, nil
	case idl.TBytes:
		s, ok := raw.(string)
		if !ok {
			return nil, errAt(path, "expected base64 string for bytes, got %T", raw)
		}
		b, err := decodeBase64(s)
		if err != nil {
			return nil, errAt(path, "invalid base64: %s", err)
		}
		return b, nil
	}
	return nil, errAt(path, "unsupported scalar %v", t.Scalar)
}

func decodeBase64(s string) ([]byte, error) {
	// Accept standard and URL-safe alphabets, with or without padding.
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}

func parseFloat(raw any) (float64, error) {
	switch x := raw.(type) {
	case json.Number:
		return strconv.ParseFloat(string(x), 64)
	case float64:
		return x, nil
	case string:
		switch x {
		case "NaN":
			return math.NaN(), nil
		case "Infinity":
			return math.Inf(1), nil
		case "-Infinity":
			return math.Inf(-1), nil
		}
		return strconv.ParseFloat(x, 64)
	}
	return 0, fmt.Errorf("expected number, got %T", raw)
}

// parseInt accepts numbers and quoted numbers. Quoted input is canonical for
// 64-bit values but tolerated for 32-bit values as well.
func parseInt(raw any, bits int, signed bool) (int64, error) {
	var s string
	switch x := raw.(type) {
	case json.Number:
		s = string(x)
	case string:
		s = x
	default:
		return 0, fmt.Errorf("expected integer, got %T", raw)
	}
	if signed {
		return strconv.ParseInt(s, 10, bits)
	}
	u, err := strconv.ParseUint(s, 10, bits)
	return int64(u), err
}

// timestampToJSON formats RFC 3339 with 0, 3, 6 or 9 fractional digits.
func timestampToJSON(m *dyn.Message) (any, error) {
	secs, nanos := int64(0), int32(0)
	if v, ok := m.Fields[1]; ok {
		secs = v.(int64)
	}
	if v, ok := m.Fields[2]; ok {
		nanos = v.(int32)
	}
	if nanos < 0 || nanos > 999999999 {
		return nil, fmt.Errorf("timestamp nanos out of range: %d", nanos)
	}
	// The RFC 3339 mapping cannot represent years outside 0001-9999.
	const minSecs, maxSecs = -62135596800, 253402300799
	if secs < minSecs || secs > maxSecs {
		return nil, fmt.Errorf("timestamp seconds %d out of range for RFC 3339", secs)
	}
	t := time.Unix(secs, int64(nanos)).UTC()
	return formatTimestamp(t), nil
}

func formatTimestamp(t time.Time) string {
	base := t.Format("2006-01-02T15:04:05")
	ns := t.Nanosecond()
	switch {
	case ns == 0:
		return base + "Z"
	case ns%1000000 == 0:
		return fmt.Sprintf("%s.%03dZ", base, ns/1000000)
	case ns%1000 == 0:
		return fmt.Sprintf("%s.%06dZ", base, ns/1000)
	default:
		return fmt.Sprintf("%s.%09dZ", base, ns)
	}
}

func jsonToTimestamp(raw any, desc *idl.Message, path string) (*dyn.Message, error) {
	s, ok := raw.(string)
	if !ok {
		return nil, errAt(path, "expected RFC 3339 string for Timestamp")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, errAt(path, "invalid timestamp %q: %s", s, err)
	}
	m := dyn.New(desc)
	if t.Unix() != 0 {
		m.Fields[1] = t.Unix()
	}
	if ns := int32(t.Nanosecond()); ns != 0 {
		m.Fields[2] = ns
	}
	return m, nil
}

// durationToJSON formats seconds.nanos with 0, 3, 6 or 9 digits.
func durationToJSON(m *dyn.Message) (any, error) {
	secs, nanos := int64(0), int32(0)
	if v, ok := m.Fields[1]; ok {
		secs = v.(int64)
	}
	if v, ok := m.Fields[2]; ok {
		nanos = v.(int32)
	}
	if nanos <= -1e9 || nanos >= 1e9 {
		return nil, fmt.Errorf("duration nanos out of range: %d", nanos)
	}
	if secs > 0 && nanos < 0 || secs < 0 && nanos > 0 {
		return nil, fmt.Errorf("duration seconds and nanos have inconsistent signs")
	}
	neg := secs < 0 || nanos < 0
	if secs < 0 {
		secs = -secs
	}
	if nanos < 0 {
		nanos = -nanos
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	fmt.Fprintf(&b, "%d", secs)
	switch {
	case nanos == 0:
	case nanos%1000000 == 0:
		fmt.Fprintf(&b, ".%03d", nanos/1000000)
	case nanos%1000 == 0:
		fmt.Fprintf(&b, ".%06d", nanos/1000)
	default:
		fmt.Fprintf(&b, ".%09d", nanos)
	}
	b.WriteByte('s')
	return b.String(), nil
}

func jsonToDuration(raw any, desc *idl.Message, path string) (*dyn.Message, error) {
	s, ok := raw.(string)
	if !ok || !strings.HasSuffix(s, "s") {
		return nil, errAt(path, "expected duration string like \"1.5s\"")
	}
	body := s[:len(s)-1]
	neg := strings.HasPrefix(body, "-")
	body = strings.TrimPrefix(body, "-")
	if body == "" {
		return nil, errAt(path, "invalid duration %q", s)
	}
	intPart, fracPart := body, ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
	}
	if len(fracPart) > 9 {
		return nil, errAt(path, "duration has more than 9 fractional digits")
	}
	var secs int64
	if _, err := fmt.Sscanf(intPart, "%d", &secs); err != nil {
		return nil, errAt(path, "invalid duration seconds %q", intPart)
	}
	nanos := int64(0)
	if fracPart != "" {
		padded := fracPart + strings.Repeat("0", 9-len(fracPart))
		n, err := strconv.ParseInt(padded, 10, 32)
		if err != nil {
			return nil, errAt(path, "invalid duration fraction %q", fracPart)
		}
		nanos = n
	}
	if neg {
		secs, nanos = -secs, -nanos
	}
	m := dyn.New(desc)
	if secs != 0 {
		m.Fields[1] = secs
	}
	if nanos != 0 {
		m.Fields[2] = int32(nanos)
	}
	return m, nil
}

func fieldMaskToJSON(m *dyn.Message) (any, error) {
	var paths []string
	if v, ok := m.Fields[1]; ok {
		for _, p := range v.([]any) {
			paths = append(paths, toSnakePath(p.(string)))
		}
	}
	return strings.Join(paths, ","), nil
}

func toSnakePath(p string) string {
	parts := strings.Split(p, ".")
	for i, s := range parts {
		parts[i] = toSnake(s)
	}
	return strings.Join(parts, ".")
}

func jsonToFieldMask(raw any, desc *idl.Message, path string) (*dyn.Message, error) {
	s, ok := raw.(string)
	if !ok {
		return nil, errAt(path, "expected string for FieldMask")
	}
	m := dyn.New(desc)
	if s == "" {
		return m, nil
	}
	var list []any
	for _, p := range strings.Split(s, ",") {
		parts := strings.Split(p, ".")
		for i, part := range parts {
			parts[i] = idl.JSONName(part)
		}
		list = append(list, strings.Join(parts, "."))
	}
	m.Fields[1] = list
	return m, nil
}

func wrapperToJSON(m *dyn.Message, opt Options) (any, error) {
	if v, ok := m.Fields[1]; ok {
		return valueToJSON(m.Desc.Fields[0].Type, v, opt)
	}
	return nil, nil
}

func jsonToWrapper(raw any, desc *idl.Message, opt Options, path string) (*dyn.Message, error) {
	m := dyn.New(desc)
	if raw == nil {
		return m, nil
	}
	v, err := jsonToValue(desc.Fields[0].Type, raw, opt, path)
	if err != nil {
		return nil, err
	}
	m.Fields[1] = v
	return m, nil
}

func structToJSON(m *dyn.Message, opt Options) (any, error) {
	out := map[string]any{}
	if v, ok := m.Fields[1]; ok {
		mm := v.(dyn.Map)
		for k, val := range mm {
			vm, _ := val.(*dyn.Message)
			if vm == nil {
				out[k.(string)] = nil
				continue
			}
			jv, err := valueMessageToJSON(vm, opt)
			if err != nil {
				return nil, err
			}
			out[k.(string)] = jv
		}
	}
	return out, nil
}

func jsonToStruct(raw any, desc *idl.Message, opt Options, path string) (*dyn.Message, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errAt(path, "expected object for Struct")
	}
	valueDesc := structValueDesc(desc)
	m := dyn.New(desc)
	mm := dyn.Map{}
	for k, v := range obj {
		vm, err := jsonToValueMessage(v, valueDesc, opt, path+"."+k)
		if err != nil {
			return nil, err
		}
		if vm == nil {
			vm = dyn.New(valueDesc)
		}
		mm[k] = vm
	}
	if len(mm) > 0 {
		m.Fields[1] = mm
	}
	return m, nil
}

// structValueDesc resolves google.protobuf.Value without importing the
// registry: Struct's map value field carries it.
func structValueDesc(desc *idl.Message) *idl.Message {
	if f := desc.FieldByNumber(1); f != nil && f.MapValue.Message != nil {
		return f.MapValue.Message
	}
	panic("idl: Struct descriptor is missing its Value map entry")
}

func listValueToJSON(m *dyn.Message, opt Options) (any, error) {
	out := []any{}
	if v, ok := m.Fields[1]; ok {
		for _, e := range v.([]any) {
			vm, _ := e.(*dyn.Message)
			if vm == nil {
				out = append(out, nil)
				continue
			}
			jv, err := valueMessageToJSON(vm, opt)
			if err != nil {
				return nil, err
			}
			out = append(out, jv)
		}
	}
	return out, nil
}

func jsonToListValue(raw any, desc *idl.Message, opt Options, path string) (*dyn.Message, error) {
	arr, ok := raw.([]any)
	if !ok {
		return nil, errAt(path, "expected array for ListValue")
	}
	valueDesc := desc.FieldByNumber(1).Type.Message
	m := dyn.New(desc)
	var list []any
	for i, e := range arr {
		v, err := jsonToValueMessage(e, valueDesc, opt, path+"["+strconv.Itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		if v == nil {
			v = dyn.New(valueDesc)
		}
		list = append(list, v)
	}
	if len(list) > 0 {
		m.Fields[1] = list
	}
	return m, nil
}

func valueMessageToJSON(m *dyn.Message, opt Options) (any, error) {
	for _, f := range m.Desc.Fields {
		v, ok := m.Fields[f.Number]
		if !ok {
			continue
		}
		switch f.Number {
		case 1: // null_value
			return nil, nil
		case 2:
			return valueToJSON(f.Type, v, opt)
		case 3:
			return v.(string), nil
		case 4:
			return v.(bool), nil
		case 5:
			return structToJSON(v.(*dyn.Message), opt)
		case 6:
			return listValueToJSON(v.(*dyn.Message), opt)
		}
	}
	return nil, nil
}

func jsonToValueMessage(raw any, desc *idl.Message, opt Options, path string) (*dyn.Message, error) {
	m := dyn.New(desc)
	switch x := raw.(type) {
	case nil:
		if f := desc.FieldByNumber(1); f != nil {
			m.Fields[1] = int32(0)
		}
		return m, nil
	case bool:
		m.Fields[4] = x
	case string:
		m.Fields[3] = x
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return nil, errAt(path, "invalid number %q", x)
		}
		m.Fields[2] = f
	case float64:
		m.Fields[2] = x
	case map[string]any:
		sf := desc.FieldByNumber(5)
		if sf == nil || sf.Type.Message == nil {
			return nil, errAt(path, "Value descriptor missing struct_value")
		}
		sm, err := jsonToStruct(x, sf.Type.Message, opt, path)
		if err != nil {
			return nil, err
		}
		m.Fields[5] = sm
	case []any:
		lf := desc.FieldByNumber(6)
		if lf == nil || lf.Type.Message == nil {
			return nil, errAt(path, "Value descriptor missing list_value")
		}
		lm, err := jsonToListValue(x, lf.Type.Message, opt, path)
		if err != nil {
			return nil, err
		}
		m.Fields[6] = lm
	default:
		return nil, errAt(path, "unsupported JSON value %T", raw)
	}
	return m, nil
}

func anyToJSON(m *dyn.Message, opt Options) (any, error) {
	typeURL, _ := m.Fields[1].(string)
	value, _ := m.Fields[2].([]byte)
	if typeURL == "" {
		return nil, nil
	}
	return map[string]any{
		"@type": typeURL,
		"value": base64.StdEncoding.EncodeToString(value),
	}, nil
}

func jsonToAny(raw any, desc *idl.Message, opt Options, path string) (*dyn.Message, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errAt(path, "expected object for Any")
	}
	tu, _ := obj["@type"].(string)
	if tu == "" {
		return nil, errAt(path, "Any is missing @type")
	}
	vs, _ := obj["value"].(string)
	b, err := decodeBase64(vs)
	if err != nil {
		return nil, errAt(path, "invalid Any value encoding: %s", err)
	}
	m := dyn.New(desc)
	m.Fields[1] = tu
	if len(b) > 0 {
		m.Fields[2] = b
	}
	return m, nil
}

// UnknownJSONFields returns preserved unknown JSON object members as a
// decoded map (empty when nothing was preserved).
func UnknownJSONFields(m *dyn.Message) map[string]any {
	if m == nil || len(m.JSONUnknown) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(m.JSONUnknown, &out); err != nil {
		return nil
	}
	return out
}

// UnmarshalField parses a JSON value for one message-typed field.
func UnmarshalField(f *idl.Field, data []byte, opt Options) (*dyn.Message, error) {
	if f.Type.Message == nil {
		return nil, fmt.Errorf("field %s is not a message", f.Name)
	}
	var raw any
	if err := decodeJSON(data, &raw); err != nil {
		return nil, &Error{Path: f.Name, Msg: err.Error()}
	}
	return jsonToMessage(raw, f.Type.Message, opt, "$."+f.Name)
}

// mapOrList names the decoded representation of a JSON object.
type mapOrList = map[string]any

// decodeJSON decodes data with exact duplicate-key detection. Go's
// encoding/json silently keeps the last duplicate key; protobuf JSON requires
// rejecting duplicates, so the token stream is decoded manually.
func decodeJSON(data []byte, out *any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing content after JSON value")
	}
	*out = v
	return nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				if _, dup := obj[key]; dup {
					return nil, fmt.Errorf("duplicate object key %q", key)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			var arr []any
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if arr == nil {
				arr = []any{}
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)
	case json.Number, string, bool, nil:
		return t, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

// Package wire implements protobuf binary encoding for dynamic messages.
//
// It supports all wire types: varints (including zig-zag for sint32/sint64),
// fixed 32/64-bit values, length-delimited strings, bytes and nested
// messages, packed repeated scalars, maps (encoded as repeated entry
// messages), and unknown-field skipping.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
)

// Wire types.
const (
	Varint     = 0
	Fixed64    = 1
	LengthDelm = 2
	StartGroup = 3
	EndGroup   = 4
	Fixed32    = 5
)

// Field numbers above this limit cannot be encoded.
const MaxFieldNumber = 536870911

var (
	ErrTruncated        = errors.New("wire: truncated message")
	ErrOverflow         = errors.New("wire: varint overflow (more than 10 bytes)")
	ErrDepth            = errors.New("wire: message nesting too deep")
	ErrInvalidUTF8      = errors.New("wire: invalid UTF-8 in string field")
	ErrInvalidWire      = errors.New("wire: invalid wire type for field")
	ErrGroupUnsupported = errors.New("wire: groups are not supported")
	ErrLengthOverflow   = errors.New("wire: length exceeds remaining data")
)

// MaxDepth bounds recursion when decoding nested messages.
const MaxDepth = 100

// EncodeOptions controls binary encoding.
type EncodeOptions struct {
	// Deterministic sorts map entries by key.
	Deterministic bool
	// EmitUnknown re-emits preserved unknown fields. Default true.
	DropUnknown bool
}

// Encode serialises m to the wire format.
func Encode(m *dyn.Message, opt EncodeOptions) ([]byte, error) {
	b := make([]byte, 0, 128)
	b, err := appendMessage(b, m, opt, 0)
	return b, err
}

// Marshal is Encode with default options.
func Marshal(m *dyn.Message) ([]byte, error) { return Encode(m, EncodeOptions{}) }

func appendMessage(dst []byte, m *dyn.Message, opt EncodeOptions, depth int) ([]byte, error) {
	if depth > MaxDepth {
		return dst, ErrDepth
	}
	for _, f := range m.Desc.SortedFields() {
		v, ok := m.Fields[f.Number]
		if !ok {
			continue
		}
		var err error
		dst, err = appendField(dst, f, v, opt, depth)
		if err != nil {
			return dst, fmt.Errorf("%s.%s: %w", m.Desc.FullName, f.Name, err)
		}
	}
	if !opt.DropUnknown {
		for _, u := range m.Unknown {
			dst = append(dst, u.Raw...)
		}
	}
	return dst, nil
}

func appendTag(dst []byte, num int32, wt int) []byte {
	return binary.AppendUvarint(dst, uint64(num)<<3|uint64(wt))
}

func appendField(dst []byte, f *idl.Field, v any, opt EncodeOptions, depth int) ([]byte, error) {
	switch {
	case f.Map:
		m, ok := v.(dyn.Map)
		if !ok {
			return dst, fmt.Errorf("expected map value, got %T", v)
		}
		keys := dyn.SortedMapKeys(m, f.MapKey)
		if !opt.Deterministic {
			// Sorted order is still deterministic; unsorted would depend on
			// Go map iteration, which we never want on the wire.
		}
		entryDesc := mapEntryDesc(f)
		for _, k := range keys {
			entry := dyn.New(entryDesc)
			entry.Set(entryDesc.Fields[0], k)
			entry.Set(entryDesc.Fields[1], m[k])
			body, err := appendMessage(nil, entry, opt, depth+1)
			if err != nil {
				return dst, err
			}
			dst = appendTag(dst, f.Number, LengthDelm)
			dst = binary.AppendUvarint(dst, uint64(len(body)))
			dst = append(dst, body...)
		}
		return dst, nil
	case f.Repeated:
		list, ok := v.([]any)
		if !ok {
			return dst, fmt.Errorf("expected repeated value, got %T", v)
		}
		if len(list) == 0 {
			return dst, nil
		}
		if f.IsPacked() {
			var payload []byte
			for _, e := range list {
				var err error
				payload, err = appendScalar(payload, f.Type, e)
				if err != nil {
					return dst, err
				}
			}
			if len(payload) == 0 {
				return dst, nil
			}
			dst = appendTag(dst, f.Number, LengthDelm)
			dst = binary.AppendUvarint(dst, uint64(len(payload)))
			return append(dst, payload...), nil
		}
		for _, e := range list {
			var err error
			dst, err = appendSingular(dst, f.Number, f.Type, e, opt, depth)
			if err != nil {
				return dst, err
			}
		}
		return dst, nil
	default:
		return appendSingular(dst, f.Number, f.Type, v, opt, depth)
	}
}

func appendSingular(dst []byte, num int32, t idl.Type, v any, opt EncodeOptions, depth int) ([]byte, error) {
	if t.Message != nil {
		m, ok := v.(*dyn.Message)
		if !ok || m == nil {
			// A nil message field is treated the same as an unset field.
			return dst, nil
		}
		body, err := appendMessage(nil, m, opt, depth+1)
		if err != nil {
			return dst, err
		}
		dst = appendTag(dst, num, LengthDelm)
		dst = binary.AppendUvarint(dst, uint64(len(body)))
		return append(dst, body...), nil
	}
	wt := t.WireType()
	dst = appendTag(dst, num, wt)
	switch t.Scalar {
	case idl.TString:
		s, ok := v.(string)
		if !ok {
			return dst, fmt.Errorf("expected string, got %T", v)
		}
		if !utf8.ValidString(s) {
			return dst, ErrInvalidUTF8
		}
		dst = binary.AppendUvarint(dst, uint64(len(s)))
		return append(dst, s...), nil
	case idl.TBytes:
		b, ok := v.([]byte)
		if !ok {
			return dst, fmt.Errorf("expected bytes, got %T", v)
		}
		dst = binary.AppendUvarint(dst, uint64(len(b)))
		return append(dst, b...), nil
	}
	return appendScalar(dst, t, v)
}

func appendScalar(dst []byte, t idl.Type, v any) ([]byte, error) {
	if t.Enum != nil {
		n, ok := v.(int32)
		if !ok {
			return dst, fmt.Errorf("expected enum int32, got %T", v)
		}
		return binary.AppendUvarint(dst, uint64(int64(n))), nil
	}
	switch t.Scalar {
	case idl.TDouble:
		x, ok := v.(float64)
		if !ok {
			return dst, fmt.Errorf("expected float64, got %T", v)
		}
		return binary.LittleEndian.AppendUint64(dst, math.Float64bits(x)), nil
	case idl.TFloat:
		x, ok := v.(float32)
		if !ok {
			return dst, fmt.Errorf("expected float32, got %T", v)
		}
		return binary.LittleEndian.AppendUint32(dst, math.Float32bits(x)), nil
	case idl.TInt32:
		x, ok := v.(int32)
		if !ok {
			return dst, fmt.Errorf("expected int32, got %T", v)
		}
		return binary.AppendUvarint(dst, uint64(int64(x))), nil
	case idl.TInt64:
		x, ok := v.(int64)
		if !ok {
			return dst, fmt.Errorf("expected int64, got %T", v)
		}
		return binary.AppendUvarint(dst, uint64(x)), nil
	case idl.TUint32:
		x, ok := v.(uint32)
		if !ok {
			return dst, fmt.Errorf("expected uint32, got %T", v)
		}
		return binary.AppendUvarint(dst, uint64(x)), nil
	case idl.TUint64:
		x, ok := v.(uint64)
		if !ok {
			return dst, fmt.Errorf("expected uint64, got %T", v)
		}
		return binary.AppendUvarint(dst, x), nil
	case idl.TSint32:
		x, ok := v.(int32)
		if !ok {
			return dst, fmt.Errorf("expected sint32, got %T", v)
		}
		return binary.AppendUvarint(dst, zigzag32(x)), nil
	case idl.TSint64:
		x, ok := v.(int64)
		if !ok {
			return dst, fmt.Errorf("expected sint64, got %T", v)
		}
		return binary.AppendUvarint(dst, zigzag64(x)), nil
	case idl.TFixed32:
		x, ok := v.(uint32)
		if !ok {
			return dst, fmt.Errorf("expected fixed32, got %T", v)
		}
		return binary.LittleEndian.AppendUint32(dst, x), nil
	case idl.TFixed64:
		x, ok := v.(uint64)
		if !ok {
			return dst, fmt.Errorf("expected fixed64, got %T", v)
		}
		return binary.LittleEndian.AppendUint64(dst, x), nil
	case idl.TSfixed32:
		x, ok := v.(int32)
		if !ok {
			return dst, fmt.Errorf("expected sfixed32, got %T", v)
		}
		return binary.LittleEndian.AppendUint32(dst, uint32(x)), nil
	case idl.TSfixed64:
		x, ok := v.(int64)
		if !ok {
			return dst, fmt.Errorf("expected sfixed64, got %T", v)
		}
		return binary.LittleEndian.AppendUint64(dst, uint64(x)), nil
	case idl.TBool:
		x, ok := v.(bool)
		if !ok {
			return dst, fmt.Errorf("expected bool, got %T", v)
		}
		if x {
			return append(dst, 1), nil
		}
		return append(dst, 0), nil
	}
	return dst, fmt.Errorf("unsupported scalar type %v", t.Scalar)
}

func zigzag32(x int32) uint64 { return uint64(uint32(x<<1) ^ uint32(x>>31)) }
func zigzag64(x int64) uint64 { return uint64(x<<1) ^ uint64(x>>63) }

func unzigzag32(u uint64) int32 { return int32(uint32(u>>1) ^ uint32(-int32(u&1))) }
func unzigzag64(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// mapEntryDesc returns the synthetic message descriptor for a map entry. The
// descriptor is built once during schema linking, so encoding is allocation-
// free and safe for concurrent use.
func mapEntryDesc(f *idl.Field) *idl.Message {
	if f.MapEntry != nil {
		return f.MapEntry
	}
	// Defensive fallback for fields constructed by hand (tests).
	ed := &idl.Message{
		Name:     idl.JSONName(f.Name) + "Entry",
		FullName: "map<" + f.MapKey.String() + "," + f.MapValue.String() + ">",
		Fields: []*idl.Field{
			{Name: "key", JSONName: "key", Number: 1, Type: idl.Type{Scalar: f.MapKey}},
			{Name: "value", JSONName: "value", Number: 2, Type: f.MapValue},
		},
	}
	ed.FieldByNumber(1)
	return ed
}

// EntryDesc exposes the synthetic map entry descriptor.
func EntryDesc(f *idl.Field) *idl.Message { return mapEntryDesc(f) }

// DecodeOptions controls binary decoding.
type DecodeOptions struct {
	// DiscardUnknown drops unknown fields instead of preserving them.
	DiscardUnknown bool
	// AllowInvalidUTF8 accepts string fields containing invalid UTF-8.
	AllowInvalidUTF8 bool
}

// Decode parses b into a new dynamic message of the given type.
func Decode(b []byte, desc *idl.Message, opt DecodeOptions) (*dyn.Message, error) {
	m := dyn.New(desc)
	if err := decodeInto(m, b, opt, 0); err != nil {
		return nil, err
	}
	return m, nil
}

// Unmarshal is Decode with default options.
func Unmarshal(b []byte, desc *idl.Message) (*dyn.Message, error) {
	return Decode(b, desc, DecodeOptions{})
}

func decodeInto(m *dyn.Message, b []byte, opt DecodeOptions, depth int) error {
	if depth > MaxDepth {
		return ErrDepth
	}
	pos := 0
	for pos < len(b) {
		start := pos
		tag, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			if n == 0 {
				return ErrTruncated
			}
			return ErrOverflow
		}
		pos += n
		if tag > math.MaxUint32 {
			return fmt.Errorf("wire: tag overflow")
		}
		num := int32(tag >> 3)
		wt := int(tag & 7)
		if num <= 0 || num > MaxFieldNumber {
			return fmt.Errorf("wire: invalid field number %d", num)
		}
		f := m.Desc.FieldByNumber(num)
		if f == nil {
			// Unknown field: skip it, remembering the raw bytes.
			end, err := skipValue(b, pos, wt)
			if err != nil {
				return err
			}
			if !opt.DiscardUnknown {
				raw := append([]byte(nil), b[start:end]...)
				m.Unknown = append(m.Unknown, dyn.UnknownField{Number: num, WireType: wt, Raw: raw})
			}
			pos = end
			continue
		}
		var err error
		pos, err = decodeField(m, f, b, pos, wt, opt, depth)
		if err != nil {
			return fmt.Errorf("%s.%s (#%d): %w", m.Desc.FullName, f.Name, f.Number, err)
		}
	}
	return nil
}

func decodeField(m *dyn.Message, f *idl.Field, b []byte, pos, wt int, opt DecodeOptions, depth int) (int, error) {
	// Packed repeated primitives arrive as length-delimited even though the
	// declared element wire type differs.
	if f.Repeated && !f.Map && wt == LengthDelm && f.Type.Packable() {
		payload, next, err := readBytes(b, pos)
		if err != nil {
			return pos, err
		}
		list, _ := m.Fields[f.Number].([]any)
		list, err = decodePacked(list, f.Type, payload)
		if err != nil {
			return pos, err
		}
		m.Set(f, list)
		return next, nil
	}
	if f.Map {
		if wt != LengthDelm {
			return pos, ErrInvalidWire
		}
		payload, next, err := readBytes(b, pos)
		if err != nil {
			return pos, err
		}
		entry, err := decodeEntry(f, payload, opt, depth)
		if err != nil {
			return pos, err
		}
		mm, _ := m.Fields[f.Number].(dyn.Map)
		if mm == nil {
			mm = dyn.Map{}
			m.Set(f, mm)
		}
		var k, v any
		if kv, ok := entry.Fields[1]; ok {
			k = kv
		} else {
			k = zeroScalar(f.MapKey)
		}
		if vv, ok := entry.Fields[2]; ok {
			v = vv
		} else if f.MapValue.Message != nil {
			v = dyn.New(f.MapValue.Message)
		} else {
			v = zeroValue(f.MapValue)
		}
		mm[k] = v
		return next, nil
	}
	if f.Repeated {
		var v any
		var err error
		v, pos, err = decodeValue(b, pos, wt, f.Type, opt, depth)
		if err != nil {
			return pos, err
		}
		list, _ := m.Fields[f.Number].([]any)
		m.Set(f, append(list, v))
		return pos, nil
	}
	v, next, err := decodeValue(b, pos, wt, f.Type, opt, depth)
	if err != nil {
		return pos, err
	}
	if f.Oneof != nil {
		for _, other := range f.Oneof.Fields {
			if other != f {
				delete(m.Fields, other.Number)
			}
		}
	}
	m.Set(f, v)
	return next, nil
}

func decodeEntry(f *idl.Field, payload []byte, opt DecodeOptions, depth int) (*dyn.Message, error) {
	ed := mapEntryDesc(f)
	entry := dyn.New(ed)
	if err := decodeInto(entry, payload, opt, depth+1); err != nil {
		return nil, err
	}
	return entry, nil
}

func decodePacked(dst []any, t idl.Type, payload []byte) ([]any, error) {
	pos := 0
	for pos < len(payload) {
		v, next, err := decodeValueNoTag(payload, pos, t)
		if err != nil {
			return nil, err
		}
		dst = append(dst, v)
		pos = next
	}
	return dst, nil
}

func decodeValue(b []byte, pos, wt int, t idl.Type, opt DecodeOptions, depth int) (any, int, error) {
	if t.Message != nil {
		if wt != LengthDelm {
			return nil, pos, ErrInvalidWire
		}
		payload, next, err := readBytes(b, pos)
		if err != nil {
			return nil, pos, err
		}
		child := dyn.New(t.Message)
		if err := decodeInto(child, payload, opt, depth+1); err != nil {
			return nil, pos, err
		}
		return child, next, nil
	}
	switch t.Scalar {
	case idl.TString:
		if wt != LengthDelm {
			return nil, pos, ErrInvalidWire
		}
		payload, next, err := readBytes(b, pos)
		if err != nil {
			return nil, pos, err
		}
		if !opt.AllowInvalidUTF8 && !utf8.Valid(payload) {
			return nil, pos, ErrInvalidUTF8
		}
		return string(payload), next, nil
	case idl.TBytes:
		if wt != LengthDelm {
			return nil, pos, ErrInvalidWire
		}
		payload, next, err := readBytes(b, pos)
		if err != nil {
			return nil, pos, err
		}
		return append([]byte(nil), payload...), next, nil
	}
	if wt != t.WireType() {
		return nil, pos, ErrInvalidWire
	}
	return decodeScalar(b, pos, t)
}

// decodeValueNoTag reads a packed element, which has no tag.
func decodeValueNoTag(b []byte, pos int, t idl.Type) (any, int, error) {
	return decodeScalar(b, pos, t)
}

func decodeScalar(b []byte, pos int, t idl.Type) (any, int, error) {
	if t.Enum != nil {
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		return int32(v), pos + n, nil
	}
	switch t.Scalar {
	case idl.TDouble:
		if len(b)-pos < 8 {
			return nil, pos, ErrTruncated
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b[pos:])), pos + 8, nil
	case idl.TFloat:
		if len(b)-pos < 4 {
			return nil, pos, ErrTruncated
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(b[pos:])), pos + 4, nil
	case idl.TInt32:
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		// Negative int32 values are sign-extended to ten-byte varints; the
		// low 32 bits are what the type carries.
		return int32(v), pos + n, nil
	case idl.TInt64:
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		return int64(v), pos + n, nil
	case idl.TUint32:
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		return uint32(v), pos + n, nil
	case idl.TUint64:
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		return v, pos + n, nil
	case idl.TSint32:
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		return unzigzag32(v), pos + n, nil
	case idl.TSint64:
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		return unzigzag64(v), pos + n, nil
	case idl.TFixed32:
		if len(b)-pos < 4 {
			return nil, pos, ErrTruncated
		}
		return binary.LittleEndian.Uint32(b[pos:]), pos + 4, nil
	case idl.TFixed64:
		if len(b)-pos < 8 {
			return nil, pos, ErrTruncated
		}
		return binary.LittleEndian.Uint64(b[pos:]), pos + 8, nil
	case idl.TSfixed32:
		if len(b)-pos < 4 {
			return nil, pos, ErrTruncated
		}
		return int32(binary.LittleEndian.Uint32(b[pos:])), pos + 4, nil
	case idl.TSfixed64:
		if len(b)-pos < 8 {
			return nil, pos, ErrTruncated
		}
		return int64(binary.LittleEndian.Uint64(b[pos:])), pos + 8, nil
	case idl.TBool:
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return nil, pos, truncErr(n)
		}
		return v != 0, pos + n, nil
	}
	return nil, pos, fmt.Errorf("unsupported scalar %v", t.Scalar)
}

func truncErr(n int) error {
	if n == 0 {
		return ErrTruncated
	}
	return ErrOverflow
}

func readBytes(b []byte, pos int) ([]byte, int, error) {
	l, n := binary.Uvarint(b[pos:])
	if n <= 0 {
		return nil, pos, truncErr(n)
	}
	pos += n
	if l > uint64(len(b)-pos) {
		return nil, pos, ErrLengthOverflow
	}
	return b[pos : pos+int(l)], pos + int(l), nil
}

// skipValue advances past a field value of the given wire type.
func skipValue(b []byte, pos, wt int) (int, error) {
	switch wt {
	case Varint:
		_, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return pos, truncErr(n)
		}
		return pos + n, nil
	case Fixed64:
		if len(b)-pos < 8 {
			return pos, ErrTruncated
		}
		return pos + 8, nil
	case Fixed32:
		if len(b)-pos < 4 {
			return pos, ErrTruncated
		}
		return pos + 4, nil
	case LengthDelm:
		_, next, err := readBytes(b, pos)
		return next, err
	case StartGroup:
		// Walk to the matching end group, tracking nesting.
		depth := 1
		for depth > 0 {
			if pos >= len(b) {
				return pos, ErrTruncated
			}
			tag, n := binary.Uvarint(b[pos:])
			if n <= 0 {
				return pos, truncErr(n)
			}
			pos += n
			w := int(tag & 7)
			switch w {
			case StartGroup:
				depth++
			case EndGroup:
				depth--
				if depth == 0 {
					return pos, nil
				}
			default:
				next, err := skipValue(b, pos, w)
				if err != nil {
					return pos, err
				}
				pos = next
			}
		}
		return pos, nil
	case EndGroup:
		return pos, ErrGroupUnsupported
	}
	return pos, fmt.Errorf("wire: unknown wire type %d", wt)
}

// SkipValue is exported for tests and diagnostics.
func SkipValue(b []byte, pos, wt int) (int, error) { return skipValue(b, pos, wt) }

func zeroScalar(t idl.ScalarType) any {
	switch t {
	case idl.TDouble:
		return float64(0)
	case idl.TFloat:
		return float32(0)
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int32(0)
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return int64(0)
	case idl.TUint32, idl.TFixed32:
		return uint32(0)
	case idl.TUint64, idl.TFixed64:
		return uint64(0)
	case idl.TBool:
		return false
	case idl.TString:
		return ""
	case idl.TBytes:
		return []byte(nil)
	}
	return nil
}

func zeroValue(t idl.Type) any {
	if t.Enum != nil {
		return int32(0)
	}
	if t.Message != nil {
		return dyn.New(t.Message)
	}
	return zeroScalar(t.Scalar)
}

// ZigZag32 and UnZigZag32 expose the encoding for tests.
func ZigZag32(x int32) uint64   { return zigzag32(x) }
func ZigZag64(x int64) uint64   { return zigzag64(x) }
func UnZigZag32(u uint64) int32 { return unzigzag32(u) }
func UnZigZag64(u uint64) int64 { return unzigzag64(u) }

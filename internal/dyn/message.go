// Package dyn provides untyped message values described by idl descriptors.
//
// Canonical Go representations:
//
//	double            float64
//	float             float32
//	int32/sint32/     int32
//	sfixed32
//	int64/sint64/     int64
//	sfixed64
//	uint32/fixed32    uint32
//	uint64/fixed64    uint64
//	bool              bool
//	string            string
//	bytes             []byte
//	enum              int32
//	message           *dyn.Message
//	repeated          []any
//	map               Map (map[any]any)
package dyn

import (
	"fmt"
	"math"
	"sort"

	"example.com/idlbridge/internal/idl"
)

// Map is a dynamic map value. Keys are canonical scalar values.
type Map map[any]any

// UnknownField preserves a field the decoder did not recognise.
type UnknownField struct {
	Number   int32
	WireType int
	Raw      []byte // tag and payload exactly as received
}

// Message is a dynamically typed protobuf message.
type Message struct {
	Desc    *idl.Message
	Fields  map[int32]any
	Unknown []UnknownField
	// JSONUnknown holds unknown JSON object members preserved when the
	// JSON codec runs with UnknownPreserve. It is not part of the wire
	// representation.
	JSONUnknown []byte
	// Origin carries request metadata such as {"headers": {...}} when the
	// message was built from an HTTP request. Backends can read it to
	// implement behaviour (the fake backend uses it for fault injection).
	Origin map[string]any
}

// New creates an empty message.
func New(desc *idl.Message) *Message {
	return &Message{Desc: desc, Fields: map[int32]any{}}
}

// Get returns the value for a field and whether it is present.
func (m *Message) Get(f *idl.Field) (any, bool) {
	v, ok := m.Fields[f.Number]
	return v, ok
}

// GetNumber returns the value for a field number.
func (m *Message) GetNumber(n int32) (any, bool) {
	v, ok := m.Fields[n]
	return v, ok
}

// Has reports whether the field is present.
func (m *Message) Has(f *idl.Field) bool {
	_, ok := m.Fields[f.Number]
	return ok
}

// Set stores a value, replacing any previous one.
func (m *Message) Set(f *idl.Field, v any) {
	if m.Fields == nil {
		m.Fields = map[int32]any{}
	}
	m.Fields[f.Number] = v
}

// Clear removes a field.
func (m *Message) Clear(f *idl.Field) {
	delete(m.Fields, f.Number)
}

// Len returns the number of set fields.
func (m *Message) Len() int { return len(m.Fields) }

// Clone performs a deep copy.
func Clone(m *Message) *Message {
	if m == nil {
		return nil
	}
	out := &Message{Desc: m.Desc, Fields: make(map[int32]any, len(m.Fields))}
	if len(m.JSONUnknown) > 0 {
		out.JSONUnknown = append([]byte(nil), m.JSONUnknown...)
	}
	if m.Origin != nil {
		out.Origin = make(map[string]any, len(m.Origin))
		for k, v := range m.Origin {
			out.Origin[k] = v
		}
	}
	for n, v := range m.Fields {
		out.Fields[n] = CloneValue(v)
	}
	if len(m.Unknown) > 0 {
		out.Unknown = make([]UnknownField, len(m.Unknown))
		for i, u := range m.Unknown {
			u.Raw = append([]byte(nil), u.Raw...)
			out.Unknown[i] = u
		}
	}
	return out
}

// CloneValue deep-copies a canonical value.
func CloneValue(v any) any {
	switch x := v.(type) {
	case *Message:
		return Clone(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CloneValue(e)
		}
		return out
	case []byte:
		return append([]byte(nil), x...)
	case Map:
		out := make(Map, len(x))
		for k, e := range x {
			out[CloneKey(k)] = CloneValue(e)
		}
		return out
	default:
		return v
	}
}

// CloneKey normalises map keys (they are scalars, so copying is enough).
func CloneKey(k any) any { return k }

// Equal compares two messages structurally. Float values compare bitwise so
// that NaN and signed zero round-trip exactly.
func Equal(a, b *Message) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Desc != b.Desc || len(a.Fields) != len(b.Fields) {
		return false
	}
	for n, av := range a.Fields {
		bv, ok := b.Fields[n]
		if !ok || !EqualValue(av, bv) {
			return false
		}
	}
	return true
}

// EqualValue compares canonical values.
func EqualValue(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case *Message:
		bv, ok := b.(*Message)
		return ok && Equal(av, bv)
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !EqualValue(av[i], bv[i]) {
				return false
			}
		}
		return true
	case []byte:
		bv, ok := b.([]byte)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
		return true
	case Map:
		bv, ok := b.(Map)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			o, ok := bv[k]
			if !ok || !EqualValue(v, o) {
				return false
			}
		}
		return true
	case float64:
		bv, ok := b.(float64)
		return ok && math.Float64bits(av) == math.Float64bits(bv)
	case float32:
		bv, ok := b.(float32)
		return ok && math.Float32bits(av) == math.Float32bits(bv)
	default:
		return a == b
	}
}

// IsDefault reports whether v equals the field's default value.
func IsDefault(f *idl.Field, v any) bool {
	switch x := v.(type) {
	case float64:
		return x == 0
	case float32:
		return x == 0
	case int32:
		return x == 0
	case int64:
		return x == 0
	case uint32:
		return x == 0
	case uint64:
		return x == 0
	case bool:
		return !x
	case string:
		return x == ""
	case []byte:
		return len(x) == 0
	case []any:
		return len(x) == 0
	case Map:
		return len(x) == 0
	case *Message:
		return x == nil
	case nil:
		return true
	}
	return false
}

// SortedMapKeys returns map keys in their canonical sort order.
func SortedMapKeys(m Map, keyType idl.ScalarType) []any {
	keys := make([]any, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return CompareScalar(keys[i], keys[j], keyType) < 0 })
	return keys
}

// CompareScalar orders two canonical scalar values of the given type.
func CompareScalar(a, b any, t idl.ScalarType) int {
	cmpI := func(x, y int64) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}
	cmpU := func(x, y uint64) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}
	switch t {
	case idl.TString:
		x, y := a.(string), b.(string)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case idl.TBool:
		x, y := a.(bool), b.(bool)
		if x == y {
			return 0
		}
		if !x {
			return -1
		}
		return 1
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return cmpI(int64(a.(int32)), int64(b.(int32)))
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return cmpI(a.(int64), b.(int64))
	case idl.TUint32, idl.TFixed32:
		return cmpU(uint64(a.(uint32)), uint64(b.(uint32)))
	case idl.TUint64, idl.TFixed64:
		return cmpU(a.(uint64), b.(uint64))
	}
	return 0
}

// String renders a value for diagnostics.
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case *Message:
		return "message(" + x.Desc.FullName + ")"
	case []any:
		return fmt.Sprintf("repeated[%d]", len(x))
	case Map:
		return fmt.Sprintf("map[%d]", len(x))
	case []byte:
		return fmt.Sprintf("bytes[%d]", len(x))
	default:
		return fmt.Sprint(x)
	}
}

// SetPath sets a value at a dotted chain of fields, allocating intermediate
// messages. The last field must not be repeated or a map.
func SetPath(m *Message, path []*idl.Field, v any) error {
	if len(path) == 0 {
		return fmt.Errorf("empty field path")
	}
	cur := m
	for i, f := range path {
		if i == len(path)-1 {
			if f.Repeated || f.Map {
				return fmt.Errorf("field %s is repeated and cannot be bound by a path variable", f.Name)
			}
			if f.Oneof != nil {
				for _, other := range f.Oneof.Fields {
					if other != f {
						delete(cur.Fields, other.Number)
					}
				}
			}
			cur.Set(f, v)
			return nil
		}
		if f.Type.Message == nil || f.Repeated || f.Map {
			return fmt.Errorf("field %s cannot contain a nested path", f.Name)
		}
		next, ok := cur.Get(f)
		if !ok || next == nil {
			nm := New(f.Type.Message)
			cur.Set(f, nm)
			cur = nm
			continue
		}
		nm, ok := next.(*Message)
		if !ok || nm == nil {
			nm = New(f.Type.Message)
			cur.Set(f, nm)
		}
		cur = nm
	}
	return nil
}

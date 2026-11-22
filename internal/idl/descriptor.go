// Package idl defines a schema model for a protobuf-flavoured interface
// description language and parses .proto files into that model.
//
// The model is deliberately independent of any third-party protobuf library:
// descriptors, type resolution, wire encoding and JSON mapping are all
// implemented in this repository.
package idl

import (
	"fmt"
	"sort"
	"strings"
)

// ScalarType enumerates the scalar types understood by the IDL.
type ScalarType uint8

const (
	TDouble ScalarType = iota + 1
	TFloat
	TInt32
	TInt64
	TUint32
	TUint64
	TSint32
	TSint64
	TFixed32
	TFixed64
	TSfixed32
	TSfixed64
	TBool
	TString
	TBytes
)

func (t ScalarType) String() string {
	switch t {
	case TDouble:
		return "double"
	case TFloat:
		return "float"
	case TInt32:
		return "int32"
	case TInt64:
		return "int64"
	case TUint32:
		return "uint32"
	case TUint64:
		return "uint64"
	case TSint32:
		return "sint32"
	case TSint64:
		return "sint64"
	case TFixed32:
		return "fixed32"
	case TFixed64:
		return "fixed64"
	case TSfixed32:
		return "sfixed32"
	case TSfixed64:
		return "sfixed64"
	case TBool:
		return "bool"
	case TString:
		return "string"
	case TBytes:
		return "bytes"
	default:
		return "invalid"
	}
}

// ScalarTypeByName resolves a scalar keyword.
func ScalarTypeByName(s string) (ScalarType, bool) {
	for t := TDouble; t <= TBytes; t++ {
		if t.String() == s {
			return t, true
		}
	}
	return 0, false
}

// WireType is the protobuf wire type used by a scalar.
func (t ScalarType) WireType() int {
	switch t {
	case TDouble, TFixed64, TSfixed64:
		return 1
	case TFloat, TFixed32, TSfixed32:
		return 5
	case TString, TBytes:
		return 2
	default:
		return 0
	}
}

// Packable reports whether repeated values of this type can use packed
// encoding. Only numeric and enum values can be packed; strings, bytes and
// messages are always length-delimited.
func (t ScalarType) Packable() bool {
	switch t {
	case TString, TBytes:
		return false
	default:
		return true
	}
}

func (t ScalarType) Is64Bit() bool {
	switch t {
	case TInt64, TUint64, TSint64, TFixed64, TSfixed64, TDouble:
		return true
	}
	return false
}

func (t ScalarType) IsUnsigned() bool {
	switch t {
	case TUint32, TUint64, TFixed32, TFixed64:
		return true
	}
	return false
}

func (t ScalarType) IsInteger() bool {
	switch t {
	case TInt32, TInt64, TUint32, TUint64, TSint32, TSint64,
		TFixed32, TFixed64, TSfixed32, TSfixed64:
		return true
	}
	return false
}

// Type is either a scalar, a message or an enum.
type Type struct {
	Scalar  ScalarType
	Message *Message
	Enum    *Enum
}

func (t Type) IsScalar() bool  { return t.Scalar != 0 }
func (t Type) IsMessage() bool { return t.Message != nil }
func (t Type) IsEnum() bool    { return t.Enum != nil }

func (t Type) String() string {
	switch {
	case t.Message != nil:
		return t.Message.FullName
	case t.Enum != nil:
		return t.Enum.FullName
	default:
		return t.Scalar.String()
	}
}

// WireType returns the wire type for singular values of this type.
func (t Type) WireType() int {
	if t.Enum != nil {
		return 0 // enums are varints
	}
	if t.Scalar != 0 {
		return t.Scalar.WireType()
	}
	return 2
}

// Packable reports whether a repeated field of this type is packed by
// default in proto3. Enums pack like integers; strings, bytes and messages
// never do.
func (t Type) Packable() bool {
	if t.Enum != nil {
		return true
	}
	if t.Message != nil || t.Scalar == 0 {
		return false
	}
	return t.Scalar.Packable()
}

// Field describes one field of a message.
type Field struct {
	Name     string // original proto name (snake_case by convention)
	JSONName string // lowerCamelCase name, or json_name override
	Number   int32
	Type     Type
	Repeated bool
	Optional bool // proto3 explicit presence (optional keyword)
	Map      bool
	MapKey   ScalarType
	// MapValue is the resolved value type when Map is true.
	MapValue Type
	// MapValueName is the unresolved value type name.
	MapValueName string

	Oneof *Oneof // containing oneof, or nil
	// Packed is tri-state: nil means proto3 default (packed for packable
	// repeated scalars), true/false come from a [packed = ...] option.
	Packed     *bool
	Deprecated bool

	// TypeName is the unresolved type name; kept for diagnostics.
	TypeName string
	// MapEntry is the synthetic entry message for a map field, built during
	// Link so encoders never have to allocate or cache descriptors.
	MapEntry *Message
	Index    int
}

// IsList reports whether the field repeats (including maps).
func (f *Field) IsList() bool { return f.Repeated || f.Map }

// HasPresence reports whether the field distinguishes absent from its
// default value.
func (f *Field) HasPresence() bool {
	if f.Map || f.Repeated {
		return false
	}
	if f.Oneof != nil || f.Optional {
		return true
	}
	return f.Type.IsMessage()
}

// IsPacked reports whether a repeated scalar/enum field uses packed
// encoding.
func (f *Field) IsPacked() bool {
	if !f.Repeated || f.Map {
		return false
	}
	if !f.Type.Packable() {
		return false
	}
	if f.Packed != nil {
		return *f.Packed
	}
	return true // proto3 default
}

// Default returns the zero/default value for the field's singular type.
func (f *Field) Default() any {
	t := f.Type
	if f.Map {
		t = f.MapValue
	}
	if t.Enum != nil {
		if len(t.Enum.Values) > 0 {
			return t.Enum.Values[0].Number
		}
		return int32(0)
	}
	if t.Message != nil {
		return (*Message)(nil)
	}
	switch t.Scalar {
	case TDouble:
		return float64(0)
	case TFloat:
		return float32(0)
	case TInt32, TSint32, TSfixed32:
		return int32(0)
	case TInt64, TSint64, TSfixed64:
		return int64(0)
	case TUint32, TFixed32:
		return uint32(0)
	case TUint64, TFixed64:
		return uint64(0)
	case TBool:
		return false
	case TString:
		return ""
	case TBytes:
		return []byte(nil)
	}
	return nil
}

// Oneof groups fields that share presence.
type Oneof struct {
	Name   string
	Fields []*Field
	Index  int
}

// Message describes a message type.
type Message struct {
	Name     string
	FullName string
	File     *File
	Fields   []*Field
	Oneofs   []*Oneof

	byNumber map[int32]*Field
	byName   map[string]*Field
}

// FieldByNumber returns the field with the given number.
func (m *Message) FieldByNumber(n int32) *Field {
	if m.byNumber == nil {
		m.index()
	}
	return m.byNumber[n]
}

// FieldByName resolves a proto field name or its JSON name.
func (m *Message) FieldByName(name string) *Field {
	if m.byName == nil {
		m.index()
	}
	return m.byName[name]
}

func (m *Message) index() {
	m.byNumber = make(map[int32]*Field, len(m.Fields))
	m.byName = make(map[string]*Field, len(m.Fields)*2)
	for _, f := range m.Fields {
		m.byNumber[f.Number] = f
		m.byName[f.Name] = f
		m.byName[f.JSONName] = f
	}
}

// SortedFields returns fields ordered by field number.
func (m *Message) SortedFields() []*Field {
	out := append([]*Field(nil), m.Fields...)
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// EnumValue is one named value of an enum.
type EnumValue struct {
	Name   string
	Number int32
}

// Enum describes an enum type.
type Enum struct {
	Name     string
	FullName string
	File     *File
	Values   []*EnumValue
	// AllowAlias marks enums where several names may share a number.
	AllowAlias bool

	byNumber map[int32]*EnumValue
	byName   map[string]*EnumValue
}

func (e *Enum) index() {
	e.byNumber = make(map[int32]*EnumValue, len(e.Values))
	e.byName = make(map[string]*EnumValue, len(e.Values))
	for _, v := range e.Values {
		e.byName[v.Name] = v
		if _, ok := e.byNumber[v.Number]; !ok {
			e.byNumber[v.Number] = v // first declared name is canonical
		}
	}
}

// ValueByNumber returns the canonical name for a number.
func (e *Enum) ValueByNumber(n int32) *EnumValue {
	if e.byNumber == nil {
		e.index()
	}
	return e.byNumber[n]
}

// ValueByName resolves an enum value name.
func (e *Enum) ValueByName(name string) *EnumValue {
	if e.byName == nil {
		e.index()
	}
	return e.byName[name]
}

// HTTPRule is a google.api.http annotation attached to an RPC.
type HTTPRule struct {
	Method       string // GET, POST, ...
	Path         string // path template
	Body         string // "" or "*" or a field path
	ResponseBody string
	Additional   []*HTTPRule
	// Order is the declaration order within the file, used as the final
	// tie-breaker between equally specific routes.
	Order int
	// Source records where the rule came from for diagnostics.
	Source string
}

// Method describes an RPC method.
type Method struct {
	Name            string
	FullName        string
	Service         *Service
	InputTypeName   string
	OutputTypeName  string
	Input           *Message
	Output          *Message
	ClientStreaming bool
	ServerStreaming bool
	HTTP            []*HTTPRule
	Deprecated      bool
	Order           int
}

// Service describes a service.
type Service struct {
	Name     string
	FullName string
	File     *File
	Methods  []*Method
}

// File is one parsed .proto file.
type File struct {
	Name     string // import path as written
	Path     string // filesystem path when loaded from disk
	Package  string
	Syntax   string
	Imports  []string
	Messages []*Message
	Enums    []*Enum
	Services []*Service
}

// Registry holds all linked types across a set of files.
type Registry struct {
	Messages map[string]*Message
	Enums    map[string]*Enum
	Files    []*File
	Services []*Service

	methods map[string]*Method
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	r := &Registry{
		Messages: map[string]*Message{},
		Enums:    map[string]*Enum{},
	}
	addBuiltins(r)
	return r
}

// Message returns a message by fully-qualified name.
func (r *Registry) Message(name string) *Message { return r.Messages[name] }

// Enum returns an enum by fully-qualified name.
func (r *Registry) Enum(name string) *Enum { return r.Enums[name] }

// Method returns a method by fully-qualified name.
func (r *Registry) Method(fullName string) *Method {
	if r.methods == nil {
		r.methods = make(map[string]*Method)
		for _, s := range r.Services {
			for _, m := range s.Methods {
				r.methods[m.FullName] = m
			}
		}
	}
	return r.methods[fullName]
}

// Methods returns all methods in declaration order.
func (r *Registry) Methods() []*Method {
	var out []*Method
	for _, s := range r.Services {
		out = append(out, s.Methods...)
	}
	return out
}

// ResolveType resolves a possibly-relative type name using protobuf scoping
// rules. scope is the full name of the enclosing message, if any; pkg is the
// file package.
func (r *Registry) ResolveType(name, pkg, scope string) (Type, bool) {
	if name == "" {
		return Type{}, false
	}
	candidates := typeCandidates(name, pkg, scope)
	for _, c := range candidates {
		if m, ok := r.Messages[c]; ok {
			return Type{Message: m}, true
		}
		if e, ok := r.Enums[c]; ok {
			return Type{Enum: e}, true
		}
	}
	return Type{}, false
}

func typeCandidates(name, pkg, scope string) []string {
	if strings.HasPrefix(name, ".") {
		return []string{name[1:]}
	}
	var out []string
	// Enclosing message scopes, innermost first, then package scopes.
	s := scope
	for s != "" {
		out = append(out, s+"."+name)
		if i := strings.LastIndexByte(s, '.'); i >= 0 {
			s = s[:i]
		} else {
			s = ""
		}
	}
	p := pkg
	for p != "" {
		out = append(out, p+"."+name)
		if i := strings.LastIndexByte(p, '.'); i >= 0 {
			p = p[:i]
		} else {
			p = ""
		}
	}
	out = append(out, name)
	return out
}

// JSONName converts a proto field name to its default lowerCamelCase JSON
// name, mirroring protobuf's algorithm.
func JSONName(protoName string) string {
	var b strings.Builder
	upper := false
	for _, r := range protoName {
		if r == '_' {
			upper = true
			continue
		}
		if upper && r >= 'a' && r <= 'z' {
			b.WriteRune(r - 'a' + 'A')
		} else {
			b.WriteRune(r)
		}
		upper = false
	}
	return b.String()
}

// SnakeName converts a lowerCamelCase JSON name back to the conventional
// snake_case form. Used for FieldMask.
func SnakeName(jsonName string) string {
	var b strings.Builder
	for i, r := range jsonName {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Link resolves all type references in the registry and validates the
// schema. It must be called after all files are added.
func (r *Registry) Link() error {
	for _, f := range r.Files {
		if err := r.linkFile(f); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) linkFile(f *File) error {
	for _, m := range f.Messages {
		if err := r.linkMessage(m); err != nil {
			return err
		}
	}
	for _, s := range f.Services {
		for _, m := range s.Methods {
			in, ok := r.ResolveType(m.InputTypeName, f.Package, "")
			if !ok || in.Message == nil {
				return fmt.Errorf("%s: rpc %s: unknown request type %q", f.Name, m.FullName, m.InputTypeName)
			}
			out, ok := r.ResolveType(m.OutputTypeName, f.Package, "")
			if !ok || out.Message == nil {
				return fmt.Errorf("%s: rpc %s: unknown response type %q", f.Name, m.FullName, m.OutputTypeName)
			}
			m.Input, m.Output = in.Message, out.Message
		}
	}
	return nil
}

func (r *Registry) linkMessage(m *Message) error {
	seenNum := map[int32]*Field{}
	seenName := map[string]*Field{}
	for _, f := range m.Fields {
		if f.Number <= 0 || f.Number > 536870911 {
			return fmt.Errorf("%s.%s: field %s has invalid number %d", m.FullName, f.Name, f.Name, f.Number)
		}
		if f.Number >= 19000 && f.Number <= 19999 {
			return fmt.Errorf("%s.%s: field number %d is reserved for protobuf internals", m.FullName, f.Name, f.Number)
		}
		if prev, ok := seenNum[f.Number]; ok {
			return fmt.Errorf("%s: field number %d used by both %s and %s", m.FullName, f.Number, prev.Name, f.Name)
		}
		if prev, ok := seenName[f.Name]; ok {
			return fmt.Errorf("%s: duplicate field name %q (number %d and %d)", m.FullName, f.Name, prev.Number, f.Number)
		}
		seenNum[f.Number] = f
		seenName[f.Name] = f
		if f.Map {
			if !validMapKey(f.MapKey) {
				return fmt.Errorf("%s.%s: invalid map key type %s", m.FullName, f.Name, f.MapKey)
			}
		}
		if f.Map && f.MapValueName != "" {
			vt, ok := r.ResolveType(f.MapValueName, m.File.Package, m.FullName)
			if !ok {
				return fmt.Errorf("%s.%s: unknown map value type %q", m.FullName, f.Name, f.MapValueName)
			}
			f.MapValue = vt
		}
		if f.TypeName == "" {
			continue
		}
		t, ok := r.ResolveType(f.TypeName, m.File.Package, m.FullName)
		if !ok {
			return fmt.Errorf("%s.%s: unknown type %q", m.FullName, f.Name, f.TypeName)
		}
		f.Type = t
		if f.Type.Message != nil && f.Type.Message.File == nil {
			// builtin, fine
		}
	}
	// Oneof bookkeeping.
	for _, o := range m.Oneofs {
		for _, f := range o.Fields {
			f.Oneof = o
		}
	}
	for _, f := range m.Fields {
		if f.Map {
			f.MapEntry = &Message{
				Name:     JSONName(f.Name) + "Entry",
				FullName: MapEntryName(m.FullName, f.Name),
				File:     m.File,
				Fields: []*Field{
					{Name: "key", JSONName: "key", Number: 1, Type: Type{Scalar: f.MapKey}},
					{Name: "value", JSONName: "value", Number: 2, Type: f.MapValue},
				},
			}
			f.MapEntry.index()
		}
	}
	m.index()
	return nil
}

func validMapKey(t ScalarType) bool {
	switch t {
	case TInt32, TInt64, TUint32, TUint64, TSint32, TSint64,
		TFixed32, TFixed64, TSfixed32, TSfixed64, TBool, TString:
		return true
	}
	return false
}

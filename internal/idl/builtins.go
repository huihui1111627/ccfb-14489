package idl

// Well-known type full names used by the JSON mapping.
const (
	TimestampName = "google.protobuf.Timestamp"
	DurationName  = "google.protobuf.Duration"
	FieldMaskName = "google.protobuf.FieldMask"
	EmptyName     = "google.protobuf.Empty"
	AnyName       = "google.protobuf.Any"
	StructName    = "google.protobuf.Struct"
	ValueName     = "google.protobuf.Value"
	ListValueName = "google.protobuf.ListValue"
	NullValueName = "google.protobuf.NullValue"
)

func addBuiltins(r *Registry) {
	mk := func(name string, fields ...*Field) *Message {
		m := &Message{Name: lastComponent(name), FullName: name, Fields: fields}
		for i, f := range fields {
			f.Index = i
		}
		m.index()
		r.Messages[name] = m
		return m
	}
	scalar := func(name string, n int32, t ScalarType, repeated bool) *Field {
		return &Field{Name: name, JSONName: JSONName(name), Number: n, Type: Type{Scalar: t}, Repeated: repeated}
	}
	msgField := func(name string, n int32, typ *Message) *Field {
		return &Field{Name: name, JSONName: JSONName(name), Number: n, Type: Type{Message: typ}}
	}
	enumField := func(name string, n int32, e *Enum) *Field {
		return &Field{Name: name, JSONName: JSONName(name), Number: n, Type: Type{Enum: e}}
	}

	ts := mk(TimestampName,
		scalar("seconds", 1, TInt64, false),
		scalar("nanos", 2, TInt32, false),
	)
	dur := mk(DurationName,
		scalar("seconds", 1, TInt64, false),
		scalar("nanos", 2, TInt32, false),
	)
	fm := mk(FieldMaskName, scalar("paths", 1, TString, true))
	mk(AnyName, scalar("type_url", 1, TString, false), scalar("value", 2, TBytes, false))
	mk(EmptyName)

	r.Enums[NullValueName] = &Enum{
		Name: "NullValue", FullName: NullValueName,
		Values: []*EnumValue{{Name: "NULL_VALUE", Number: 0}},
	}
	r.Enums[NullValueName].index()

	// google.protobuf.Value / Struct / ListValue are represented with the
	// well-known JSON rules; the descriptors exist so fields can reference
	// them, but the codec maps them directly to arbitrary JSON. Their field
	// layout follows the canonical proto definition.
	nv := r.Enums[NullValueName]
	listValue := &Message{Name: "ListValue", FullName: ListValueName}
	value := &Message{Name: "Value", FullName: ValueName}
	listValue.Fields = []*Field{{
		Name: "values", JSONName: "values", Number: 1, Type: Type{Message: value}, Repeated: true,
	}}
	value.Fields = []*Field{
		enumField("null_value", 1, nv),
		scalar("number_value", 2, TDouble, false),
		scalar("string_value", 3, TString, false),
		scalar("bool_value", 4, TBool, false),
		{Name: "struct_value", JSONName: "structValue", Number: 5, Type: Type{Message: nil}},
		msgField("list_value", 6, listValue),
	}
	structMsg := &Message{Name: "Struct", FullName: StructName}
	structMsg.Fields = []*Field{{
		Name: "fields", JSONName: "fields", Number: 1, Map: true,
		MapKey: TString, MapValue: Type{Message: value}, Repeated: true,
	}}
	structMsg.Fields[0].MapEntry = mk(MapEntryName(StructName, "fields"),
		scalar("key", 1, TString, false),
		msgField("value", 2, value),
	)
	value.Fields[4].Type = Type{Message: structMsg}
	listValue.index()
	value.index()
	structMsg.index()
	r.Messages[StructName] = structMsg
	r.Messages[ListValueName] = listValue
	r.Messages[ValueName] = value

	// Wrapper types. get_/set_ helpers live in the codec; descriptors only
	// need the value field.
	wrappers := map[string]ScalarType{
		"DoubleValue": TDouble, "FloatValue": TFloat,
		"Int64Value": TInt64, "UInt64Value": TUint64,
		"Int32Value": TInt32, "UInt32Value": TUint32,
		"BoolValue": TBool, "StringValue": TString, "BytesValue": TBytes,
	}
	for name, st := range wrappers {
		full := "google.protobuf." + name
		r.Messages[full] = mk(full, scalar("value", 1, st, false))
	}

	_ = ts
	_ = dur
	_ = fm
}

// MapEntryName is the synthetic full name of a map entry message, following
// protobuf's naming convention.
func MapEntryName(parent, field string) string {
	return parent + "." + JSONName(field) + "Entry"
}

func lastComponent(full string) string {
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '.' {
			return full[i+1:]
		}
	}
	return full
}

// WellKnownKind classifies a message for JSON special-casing.
type WellKnownKind uint8

const (
	NotWellKnown WellKnownKind = iota
	KindTimestamp
	KindDuration
	KindFieldMask
	KindAny
	KindEmpty
	KindStruct
	KindValue
	KindListValue
	KindWrapper
)

// WellKnown returns the special JSON mapping kind for a message.
func WellKnown(m *Message) WellKnownKind {
	if m == nil {
		return NotWellKnown
	}
	switch m.FullName {
	case TimestampName:
		return KindTimestamp
	case DurationName:
		return KindDuration
	case FieldMaskName:
		return KindFieldMask
	case AnyName:
		return KindAny
	case EmptyName:
		return KindEmpty
	case StructName:
		return KindStruct
	case ValueName:
		return KindValue
	case ListValueName:
		return KindListValue
	case "google.protobuf.DoubleValue", "google.protobuf.FloatValue",
		"google.protobuf.Int64Value", "google.protobuf.UInt64Value",
		"google.protobuf.Int32Value", "google.protobuf.UInt32Value",
		"google.protobuf.BoolValue", "google.protobuf.StringValue",
		"google.protobuf.BytesValue":
		return KindWrapper
	}
	return NotWellKnown
}

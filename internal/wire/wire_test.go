package wire

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
)

// testSchema is a small description covering every scalar, nesting, repeated,
// maps and enums.
const testSchema = `
syntax = "proto3";
package wiretest;

enum Color { COLOR_UNSPECIFIED = 0; RED = 1; GREEN = 2; BLUE = 3; }

message Inner {
  string label = 1;
  int32 count = 2;
}

message All {
  int32 i32 = 1;
  int64 i64 = 2;
  uint32 u32 = 3;
  uint64 u64 = 4;
  sint32 s32 = 5;
  sint64 s64 = 6;
  fixed32 f32 = 7;
  fixed64 f64 = 8;
  sfixed32 sf32 = 9;
  sfixed64 sf64 = 10;
  double d = 11;
  float f = 12;
  bool b = 13;
  string s = 14;
  bytes by = 15;
  Color color = 16;
  Inner inner = 17;
  repeated int32 packed = 18;
  repeated int32 unpacked = 19 [packed = false];
  repeated string names = 20;
  repeated Inner inners = 21;
  map<string, int32> counts = 22;
  map<int32, Inner> by_inner = 23;
}
`

func loadSchema(t *testing.T) *idl.Registry {
	t.Helper()
	reg := idl.NewRegistry()
	if _, err := idl.ParseFile(testSchema, "test.proto", "", reg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := reg.Link(); err != nil {
		t.Fatalf("link: %v", err)
	}
	return reg
}

func allMessage(t *testing.T, reg *idl.Registry) *dyn.Message {
	t.Helper()
	desc := reg.Message("wiretest.All")
	if desc == nil {
		t.Fatal("missing wiretest.All")
	}
	m := dyn.New(desc)
	m.Fields[1] = int32(-1)
	m.Fields[2] = int64(-2)
	m.Fields[3] = uint32(300)
	m.Fields[4] = uint64(1 << 40)
	m.Fields[5] = int32(-7)
	m.Fields[6] = int64(-8)
	m.Fields[7] = uint32(0xdeadbeef)
	m.Fields[8] = uint64(0x0102030405060708)
	m.Fields[9] = int32(-9)
	m.Fields[10] = int64(-10)
	m.Fields[11] = float64(1.5)
	m.Fields[12] = float32(-2.25)
	m.Fields[13] = true
	m.Fields[14] = "héllo"
	m.Fields[15] = []byte{0x00, 0xff, 0x10}
	m.Fields[16] = int32(2)
	inner := dyn.New(reg.Message("wiretest.Inner"))
	inner.Fields[1] = "x"
	inner.Fields[2] = int32(3)
	m.Fields[17] = inner
	m.Fields[18] = []any{int32(1), int32(-1), int32(2)}
	m.Fields[19] = []any{int32(5), int32(6)}
	m.Fields[20] = []any{"a", "b"}
	m.Fields[21] = []any{inner, inner}
	m.Fields[22] = dyn.Map{"k": int32(7)}
	m.Fields[23] = dyn.Map{int32(4): inner}
	return m
}

func TestRoundTripComprehensive(t *testing.T) {
	reg := loadSchema(t)
	m := allMessage(t, reg)
	raw, err := Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Unmarshal(raw, m.Desc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dyn.Equal(m, got) {
		t.Fatalf("round trip mismatch\n want %v\n got  %v", m, got)
	}
}

func TestZigZag(t *testing.T) {
	cases := []struct {
		in   int64
		want uint64
	}{
		{0, 0}, {-1, 1}, {1, 2}, {-2, 3}, {2, 4},
		{math.MaxInt32, 4294967294}, {math.MinInt32, 4294967295},
		{math.MaxInt64, math.MaxUint64 - 1}, {math.MinInt64, math.MaxUint64},
	}
	for _, tc := range cases {
		if got := ZigZag64(tc.in); got != tc.want {
			t.Errorf("ZigZag64(%d) = %d want %d", tc.in, got, tc.want)
		}
		if got := UnZigZag64(tc.want); got != tc.in {
			t.Errorf("UnZigZag64(%d) = %d want %d", tc.want, got, tc.in)
		}
	}
	for _, v := range []int32{0, -1, 1, math.MinInt32, math.MaxInt32} {
		if got := UnZigZag32(ZigZag32(v)); got != v {
			t.Errorf("zigzag32 round trip %d -> %d", v, got)
		}
	}
}

// TestExactVectors pins the wire format against hand-computed bytes.
func TestExactVectors(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.All")
	cases := []struct {
		name   string
		fields map[int32]any
		want   string
	}{
		{
			name:   "int32 -1 is ten bytes (sign extended)",
			fields: map[int32]any{1: int32(-1)},
			want:   "08ffffffffffffffffff01",
		},
		{
			name:   "int32 150",
			fields: map[int32]any{1: int32(150)},
			want:   "089601",
		},
		{
			name:   "sint32 -1 is one byte",
			fields: map[int32]any{5: int32(-1)},
			want:   "2801",
		},
		{
			name:   "sint32 1",
			fields: map[int32]any{5: int32(1)},
			want:   "2802",
		},
		{
			name:   "uint64 big",
			fields: map[int32]any{4: uint64(1 << 40)},
			want:   "20808080808020",
		},
		{
			name:   "fixed32",
			fields: map[int32]any{7: uint32(1)},
			want:   "3d01000000",
		},
		{
			name:   "fixed64",
			fields: map[int32]any{8: uint64(1)},
			want:   "410100000000000000",
		},
		{
			name:   "double 1.5",
			fields: map[int32]any{11: float64(1.5)},
			want:   "59" + "000000000000f83f",
		},
		{
			name:   "float -2.25",
			fields: map[int32]any{12: float32(-2.25)},
			want:   "65000010c0",
		},
		{
			name:   "bool true",
			fields: map[int32]any{13: true},
			want:   "6801",
		},
		{
			name:   "enum green",
			fields: map[int32]any{16: int32(2)},
			want:   "800102",
		},
		{
			name:   "string",
			fields: map[int32]any{14: "hi"},
			want:   "72026869",
		},
		{
			name:   "packed ints",
			fields: map[int32]any{18: []any{int32(1), int32(2), int32(3)}},
			want:   "920103010203",
		},
		{
			name:   "unpacked ints",
			fields: map[int32]any{19: []any{int32(1), int32(2)}},
			want:   "980101980102",
		},
		{
			name:   "nested message",
			fields: map[int32]any{17: innerMessage(t, reg)},
			want:   "8a01050a01781003",
		},
		{
			name:   "map entry",
			fields: map[int32]any{22: dyn.Map{"k": int32(7)}},
			want:   "b201050a016b1007",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := dyn.New(desc)
			for n, v := range tc.fields {
				m.Fields[n] = v
			}
			got, err := Marshal(m)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Fatalf("got %x want %s", got, tc.want)
			}
			back, err := Unmarshal(got, desc)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !dyn.Equal(m, back) {
				t.Fatalf("round trip mismatch")
			}
		})
	}
}

func innerMessage(t *testing.T, reg *idl.Registry) *dyn.Message {
	m := dyn.New(reg.Message("wiretest.Inner"))
	m.Fields[1] = "x"
	m.Fields[2] = int32(3)
	return m
}

func TestWireTypePrefixes(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.All")
	m := dyn.New(desc)
	m.Fields[1] = int32(0)
	m.Fields[13] = false
	m.Fields[14] = ""
	raw, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// A field present in the dynamic message is encoded even when it holds
	// the zero value; omission of defaults is the JSON layer's concern.
	want := []byte{0x08, 0x00, 0x68, 0x00, 0x72, 0x00}
	if !bytes.Equal(raw, want) {
		t.Fatalf("got %x want %x", raw, want)
	}
}

func TestUnknownFieldPreservedAndSkipped(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.Inner")
	// Field 99 varint, field 100 fixed64, field 101 length-delimited.
	var buf bytes.Buffer
	buf.Write([]byte{0x98, 0x06, 0x2a}) // 99: varint 42
	buf.Write([]byte{0xa1, 0x06})       // 100: fixed64
	buf.Write([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	buf.Write([]byte{0xaa, 0x06, 0x03, 'x', 'y', 'z'}) // 101: len 3
	buf.Write([]byte{0x0a, 0x01, 'a'})                 // field 1: "a"
	// Unpacked unknown packed field also has to be skippable.
	buf.Write([]byte{0xb2, 0x06, 0x02, 0x01, 0x02}) // 102: len 2

	m, err := Unmarshal(buf.Bytes(), desc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Fields[1] != "a" {
		t.Fatalf("known field lost: %v", m.Fields)
	}
	if len(m.Unknown) != 4 {
		t.Fatalf("unknown count = %d", len(m.Unknown))
	}
	out, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// Re-encoding appends unknowns after known fields; the known prefix must
	// match and the unknown bytes must all be present.
	if !bytes.Contains(out, []byte{0x98, 0x06, 0x2a}) {
		t.Fatalf("unknown varint lost: %x", out)
	}
	back, err := Unmarshal(out, desc)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Unknown) != 4 || back.Fields[1] != "a" {
		t.Fatalf("re-decode: %v %v", back.Fields, back.Unknown)
	}

	discarded, err := Decode(buf.Bytes(), desc, DecodeOptions{DiscardUnknown: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(discarded.Unknown) != 0 {
		t.Fatalf("discard did not drop unknowns")
	}
}

func TestPackAndUnpackMix(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.All")
	m := dyn.New(desc)
	m.Fields[18] = []any{int32(1), int32(2)}
	raw, _ := Marshal(m)
	if raw[0] != 0x92 || len(raw) != 5 {
		t.Fatalf("packed tag/length wrong: %x", raw)
	}
	// A decoder must accept unpacked encoding for a packed field and vice
	// versa, per the spec.
	unpacked := []byte{0x90, 0x01, 0x01, 0x90, 0x01, 0x02}
	got, err := Unmarshal(unpacked, desc)
	if err != nil {
		t.Fatalf("decode unpacked: %v", err)
	}
	list, _ := got.Fields[18].([]any)
	if len(list) != 2 || list[0].(int32) != 1 || list[1].(int32) != 2 {
		t.Fatalf("list = %v", list)
	}
}

func TestEmptyPackedFieldOmitted(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.All")
	m := dyn.New(desc)
	m.Fields[18] = []any{}
	raw, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("empty repeated should encode to nothing, got %x", raw)
	}
}

func TestTruncatedInputs(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.All")
	cases := [][]byte{
		{0x0a},             // string tag, missing length
		{0x0a, 0x05, 'a'},  // length exceeds remaining
		{0x0d, 0x01, 0x02}, // fixed32 truncated
		{0x08},             // varint tag, missing value
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, // varint overflow
		{0x00}, // field number 0
	}
	for i, in := range cases {
		if _, err := Unmarshal(in, desc); err == nil {
			t.Errorf("case %d (%x) should fail", i, in)
		}
	}
}

func TestInvalidUTF8(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.Inner")
	bad := []byte{0x0a, 0x02, 0xff, 0xfe}
	if _, err := Unmarshal(bad, desc); err == nil {
		t.Fatal("invalid UTF-8 should be rejected for string fields")
	}
	m, err := Decode(bad, desc, DecodeOptions{AllowInvalidUTF8: true})
	if err != nil {
		t.Fatalf("lenient decode: %v", err)
	}
	if m.Fields[1].(string) != "\xff\xfe" {
		t.Fatalf("got %q", m.Fields[1])
	}
}

func TestMapDeterministicOrder(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.All")
	m := dyn.New(desc)
	m.Fields[22] = dyn.Map{"b": int32(2), "a": int32(1), "c": int32(3)}
	first, err := Encode(m, EncodeOptions{Deterministic: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		next, _ := Encode(m, EncodeOptions{Deterministic: true})
		if !bytes.Equal(first, next) {
			t.Fatalf("map encoding is not deterministic")
		}
	}
	// Keys must be sorted: a before b before c.
	s := string(first)
	if !(strings.Index(s, "a") < strings.Index(s, "b") && strings.Index(s, "b") < strings.Index(s, "c")) {
		t.Fatalf("unexpected key order: %x", first)
	}
}

func TestNestedDepthLimit(t *testing.T) {
	// Build a synthetic recursive descriptor and a correctly nested
	// encoding of MaxDepth+5 levels.
	desc := &idl.Message{Name: "Node", FullName: "test.Node"}
	f := &idl.Field{Name: "next", JSONName: "next", Number: 1, Type: idl.Type{Message: desc}}
	desc.Fields = []*idl.Field{f}
	desc.FieldByNumber(1) // build the index
	buf := []byte{}
	for i := 0; i < MaxDepth+5; i++ {
		next := make([]byte, len(buf)+2)
		next[0] = 0x0a
		next[1] = byte(len(buf))
		copy(next[2:], buf)
		buf = next
	}
	if _, err := Unmarshal(buf, desc); err == nil {
		t.Fatal("expected a depth-limit error")
	}
}

// TestRandomRoundTrip fuzzes encode/decode across values of every type.
func TestRandomRoundTrip(t *testing.T) {
	reg := loadSchema(t)
	desc := reg.Message("wiretest.All")
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		m := randomMessage(rng, desc, 0)
		raw, err := Marshal(m)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		back, err := Unmarshal(raw, desc)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !dyn.Equal(m, back) {
			t.Fatalf("iteration %d: round trip mismatch\n%v\n%v", i, m, back)
		}
	}
}

func randomMessage(rng *rand.Rand, desc *idl.Message, depth int) *dyn.Message {
	m := dyn.New(desc)
	for _, f := range desc.Fields {
		if rng.Intn(3) == 0 {
			continue
		}
		switch {
		case f.Map:
			mm := dyn.Map{}
			n := rng.Intn(4)
			for j := 0; j < n; j++ {
				mm[randomValue(rng, idl.Type{Scalar: f.MapKey}, depth)] = randomValue(rng, f.MapValue, depth)
			}
			// An empty collection is indistinguishable from an absent one
			// on the wire, so leave it unset.
			for k, v := range mm {
				if v == nil || fmt.Sprintf("%v", v) == "<nil>" {
					delete(mm, k)
				}
			}
			if len(mm) > 0 {
				m.Set(f, mm)
			}
		case f.Repeated:
			n := rng.Intn(5)
			list := make([]any, 0, n)
			for j := 0; j < n; j++ {
				list = append(list, randomValue(rng, f.Type, depth))
			}
			list = removeNilMessages(list)
			if len(list) > 0 {
				m.Set(f, list)
			}
		default:
			v := randomValue(rng, f.Type, depth)
			if f.Type.Message == nil || v != nil {
				m.Set(f, v)
			}
		}
	}
	return m
}

func randomValue(rng *rand.Rand, t idl.Type, depth int) any {
	if t.Message != nil {
		if depth > 2 || rng.Intn(4) == 0 {
			return nil
		}
		v := randomMessage(rng, t.Message, depth+1)
		return v
	}
	if t.Enum != nil {
		return int32(rng.Intn(4))
	}
	switch t.Scalar {
	case idl.TDouble:
		return math.Float64frombits(rng.Uint64())
	case idl.TFloat:
		return math.Float32frombits(rng.Uint32())
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int32(rng.Uint32())
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return int64(rng.Uint64())
	case idl.TUint32, idl.TFixed32:
		return rng.Uint32()
	case idl.TUint64, idl.TFixed64:
		return rng.Uint64()
	case idl.TBool:
		return rng.Intn(2) == 1
	case idl.TString:
		return randomString(rng)
	case idl.TBytes:
		b := make([]byte, rng.Intn(8))
		rng.Read(b)
		return b
	}
	return nil
}

func randomString(rng *rand.Rand) string {
	n := rng.Intn(8)
	runes := make([]rune, n)
	for i := range runes {
		runes[i] = rune(0x20 + rng.Intn(0x2000))
	}
	return string(runes)
}

func BenchmarkEncode(b *testing.B) {
	reg := idl.NewRegistry()
	if _, err := idl.ParseFile(testSchema, "test.proto", "", reg); err != nil {
		b.Fatal(err)
	}
	if err := reg.Link(); err != nil {
		b.Fatal(err)
	}
	desc := reg.Message("wiretest.All")
	m := randomMessage(rand.New(rand.NewSource(4)), desc, 0)
	raw, err := Marshal(m)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	reg := idl.NewRegistry()
	if _, err := idl.ParseFile(testSchema, "test.proto", "", reg); err != nil {
		b.Fatal(err)
	}
	if err := reg.Link(); err != nil {
		b.Fatal(err)
	}
	desc := reg.Message("wiretest.All")
	m := randomMessage(rand.New(rand.NewSource(4)), desc, 0)
	raw, _ := Marshal(m)
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Unmarshal(raw, desc); err != nil {
			b.Fatal(err)
		}
	}
}

// removeNilMessages drops nil entries, which are indistinguishable from
// empty messages on the wire for a test focused on value round trips.
func removeNilMessages(list []any) []any {
	out := list[:0]
	for _, v := range list {
		if v == nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

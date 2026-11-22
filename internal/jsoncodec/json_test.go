package jsoncodec

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
)

const schema = `
syntax = "proto3";
package jsontest;

enum Mood { MOOD_UNSPECIFIED = 0; HAPPY = 1; SAD = 2; }

message Inner {
  string note = 1;
  int32 rank = 2;
}

message All {
  double d = 1;
  float f = 2;
  int32 i32 = 3;
  int64 i64 = 4;
  uint32 u32 = 5;
  uint64 u64 = 6;
  sint32 s32 = 7;
  sint64 s64 = 8;
  bool flag = 9;
  string text = 10;
  bytes blob = 11;
  Mood mood = 12;
  Inner inner = 13;
  repeated int64 numbers = 14;
  repeated Inner inners = 15;
  map<string, int32> counts = 16;
  map<int64, Inner> by_id = 17;
  oneof choice {
    string name = 18;
    int64 id = 19;
  }
  optional int32 maybe = 20;
  google.protobuf.Timestamp when = 21;
  google.protobuf.Duration how_long = 22;
  google.protobuf.Int64Value wrapped = 23;
  google.protobuf.FieldMask mask = 24;
  google.protobuf.Struct props = 25;
}
`

func load(t *testing.T) *idl.Registry {
	t.Helper()
	reg := idl.NewRegistry()
	if _, err := idl.ParseFile(schema, "t.proto", "", reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Link(); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestScalarMappings(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	m := dyn.New(desc)
	m.Fields[1] = 1.5
	m.Fields[2] = float32(2.5)
	m.Fields[3] = int32(-3)
	m.Fields[4] = int64(9007199254740993) // exceeds float64 integer precision
	m.Fields[5] = uint32(5)
	m.Fields[6] = uint64(math.MaxUint64)
	m.Fields[7] = int32(-7)
	m.Fields[8] = int64(math.MinInt64)
	m.Fields[9] = true
	m.Fields[10] = "héllo \"world\""
	m.Fields[11] = []byte{0, 1, 2, 0xff}
	m.Fields[12] = int32(2)

	out, err := Marshal(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["d"].(float64) != 1.5 || got["f"].(float64) != 2.5 {
		t.Fatalf("floats: %v", got)
	}
	if got["i64"] != "9007199254740993" {
		t.Fatalf("int64 must be a string: %#v", got["i64"])
	}
	if got["u64"] != "18446744073709551615" {
		t.Fatalf("uint64 must be a string: %#v", got["u64"])
	}
	if got["s64"] != "-9223372036854775808" {
		t.Fatalf("sint64: %#v", got["s64"])
	}
	if got["blob"] != "AAEC/w==" {
		t.Fatalf("bytes base64 = %#v", got["blob"])
	}
	if got["mood"] != "SAD" {
		t.Fatalf("enum name = %#v", got["mood"])
	}
	if got["i32"].(float64) != -3 {
		t.Fatalf("int32 should be a JSON number: %#v", got["i32"])
	}

	back, err := Unmarshal(out, desc, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dyn.Equal(m, back) {
		t.Fatalf("round trip mismatch\n%v\n%v", m, back)
	}
}

func TestDefaultsOmitted(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	m := dyn.New(desc)
	m.Fields[3] = int32(0)
	m.Fields[10] = ""
	m.Fields[14] = []any{}
	out, _ := Marshal(m, Options{})
	if string(out) != "{}" {
		t.Fatalf("defaults should be omitted, got %s", out)
	}
	out, _ = Marshal(m, Options{EmitDefaults: true})
	var got map[string]any
	json.Unmarshal(out, &got)
	if _, ok := got["i32"]; !ok {
		t.Fatalf("EmitDefaults should include i32: %s", out)
	}
}

func TestPresenceHonoured(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	m := dyn.New(desc)
	// optional int32 at zero must be emitted because it has presence.
	m.Fields[20] = int32(0)
	out, _ := Marshal(m, Options{})
	if !strings.Contains(string(out), `"maybe":0`) {
		t.Fatalf("optional zero must be emitted: %s", out)
	}
	// A oneof member at zero also has presence.
	m2 := dyn.New(desc)
	m2.Fields[19] = int64(0)
	out, _ = Marshal(m2, Options{})
	if !strings.Contains(string(out), `"id":"0"`) {
		t.Fatalf("oneof zero must be emitted: %s", out)
	}
}

func TestFieldNameVariants(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.Inner")
	for _, doc := range []string{
		`{"note":"a","rank":2}`,
		`{"note":"a","rank":2}`,
	} {
		if _, err := Unmarshal([]byte(doc), desc, Options{}); err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
	}
	// snake_case for a single-word field is identical; test a multi-word
	// field through the message below.
	multi := reg.Message("jsontest.All")
	m, err := Unmarshal([]byte(`{"how_long":"1.5s","maybe":3}`), multi, Options{})
	if err != nil {
		t.Fatalf("snake_case names: %v", err)
	}
	out, _ := Marshal(m, Options{})
	if !strings.Contains(string(out), `"howLong":"1.500s"`) {
		t.Fatalf("output should use camelCase: %s", out)
	}
	out, _ = Marshal(m, Options{UseProtoNames: true})
	if !strings.Contains(string(out), `"how_long"`) {
		t.Fatalf("UseProtoNames should emit snake_case: %s", out)
	}
}

func TestInt64AcceptsNumberOrString(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	for _, doc := range []string{`{"i64":"5"}`, `{"i64":5}`} {
		m, err := Unmarshal([]byte(doc), desc, Options{})
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if m.Fields[4].(int64) != 5 {
			t.Fatalf("%s -> %v", doc, m.Fields[4])
		}
	}
	for _, bad := range []string{`{"i64":"5.5"}`, `{"i64":"x"}`, `{"i64":1.5}`} {
		if _, err := Unmarshal([]byte(bad), desc, Options{}); err == nil {
			t.Errorf("%s should fail", bad)
		}
	}
}

func TestEnumHandling(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	m, err := Unmarshal([]byte(`{"mood":"HAPPY"}`), desc, Options{})
	if err != nil || m.Fields[12].(int32) != 1 {
		t.Fatalf("by name: %v %v", m, err)
	}
	m, err = Unmarshal([]byte(`{"mood":2}`), desc, Options{})
	if err != nil || m.Fields[12].(int32) != 2 {
		t.Fatalf("by number: %v %v", m, err)
	}
	if _, err := Unmarshal([]byte(`{"mood":"ANGRY"}`), desc, Options{}); err == nil {
		t.Fatal("unknown enum name should fail")
	}
	out, _ := Marshal(m, Options{UseEnumNumbers: true})
	if !strings.Contains(string(out), `"mood":2`) {
		t.Fatalf("UseEnumNumbers: %s", out)
	}
	// Unknown numeric enum values round trip as numbers.
	u := dyn.New(desc)
	u.Fields[12] = int32(99)
	out, _ = Marshal(u, Options{})
	if !strings.Contains(string(out), `"mood":99`) {
		t.Fatalf("unknown enum number: %s", out)
	}
}

func TestFloatSpecials(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	m := dyn.New(desc)
	m.Fields[1] = math.Inf(1)
	m.Fields[2] = float32(math.NaN())
	out, _ := Marshal(m, Options{})
	if !strings.Contains(string(out), `"d":"Infinity"`) || !strings.Contains(string(out), `"f":"NaN"`) {
		t.Fatalf("special floats: %s", out)
	}
	back, err := Unmarshal(out, desc, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !math.IsInf(back.Fields[1].(float64), 1) || !math.IsNaN(float64(back.Fields[2].(float32))) {
		t.Fatalf("round trip: %v", back.Fields)
	}
}

func TestUnknownFieldPolicy(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.Inner")
	doc := []byte(`{"note":"a","extra":{"x":1},"more":2}`)
	if _, err := Unmarshal(doc, desc, Options{}); err == nil {
		t.Fatal("UnknownReject should fail")
	}
	if _, err := Unmarshal(doc, desc, Options{Unknown: UnknownIgnore}); err != nil {
		t.Fatalf("UnknownIgnore: %v", err)
	}
	m, err := Unmarshal(doc, desc, Options{Unknown: UnknownPreserve})
	if err != nil {
		t.Fatalf("UnknownPreserve: %v", err)
	}
	preserved := UnknownJSONFields(m)
	if len(preserved) != 2 || preserved["more"] == nil || preserved["extra"] == nil {
		t.Fatalf("preserve = %v", preserved)
	}
	out, err := Marshal(m, Options{Unknown: UnknownPreserve})
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped map[string]any
	json.Unmarshal(out, &roundTripped)
	if roundTripped["extra"] == nil || roundTripped["note"] != "a" {
		t.Fatalf("preserved fields must survive re-encoding: %s", out)
	}
}

func TestDuplicateFieldRejected(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.Inner")
	// Both spellings of the same field must be rejected as duplicates.
	doc := []byte(`{"rank":1,"rank":2}`)
	if _, err := Unmarshal(doc, desc, Options{}); err == nil {
		t.Fatal("duplicate field should fail")
	}
}

func TestTimestampFormatting(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	cases := []struct {
		secs  int64
		nanos int32
		want  string
	}{
		{0, 0, `"1970-01-01T00:00:00Z"`},
		{1700000000, 0, `"2023-11-14T22:13:20Z"`},
		{1700000000, 120000000, `"2023-11-14T22:13:20.120Z"`},
		{1700000000, 120300000, `"2023-11-14T22:13:20.120300Z"`},
		{1700000000, 120300400, `"2023-11-14T22:13:20.120300400Z"`},
		{-1, 500000000, `"1969-12-31T23:59:59.500Z"`},
	}
	for _, tc := range cases {
		m := dyn.New(desc)
		ts := dyn.New(reg.Message("google.protobuf.Timestamp"))
		if tc.secs != 0 {
			ts.Fields[1] = tc.secs
		}
		if tc.nanos != 0 {
			ts.Fields[2] = tc.nanos
		}
		m.Fields[21] = ts
		out, err := Marshal(m, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"when":`+tc.want) {
			t.Fatalf("%d/%d: got %s want %s", tc.secs, tc.nanos, out, tc.want)
		}
		back, err := Unmarshal(out, desc, Options{})
		if err != nil {
			t.Fatalf("decode %s: %v", out, err)
		}
		if !dyn.Equal(m, back) {
			t.Fatalf("timestamp round trip: %v vs %v", m.Fields[21], back.Fields[21])
		}
	}
	if _, err := Unmarshal([]byte(`{"when":"not a time"}`), desc, Options{}); err == nil {
		t.Fatal("bad timestamp should fail")
	}
}

func TestDurationFormatting(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	cases := []struct {
		secs  int64
		nanos int32
		want  string
	}{
		{1, 0, `"1s"`},
		{1, 500000000, `"1.500s"`},
		{-1, -500000000, `"-1.500s"`},
		{0, -1000, `"-0.000001s"`},
		{2, 0, `"2s"`},
	}
	for _, tc := range cases {
		m := dyn.New(desc)
		d := dyn.New(reg.Message("google.protobuf.Duration"))
		if tc.secs != 0 {
			d.Fields[1] = tc.secs
		}
		if tc.nanos != 0 {
			d.Fields[2] = tc.nanos
		}
		m.Fields[22] = d
		out, _ := Marshal(m, Options{})
		if !strings.Contains(string(out), `"howLong":`+tc.want) {
			t.Fatalf("%d/%d: got %s want %s", tc.secs, tc.nanos, out, tc.want)
		}
		back, err := Unmarshal(out, desc, Options{})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !dyn.Equal(m, back) {
			t.Fatalf("duration round trip: %v vs %v", m.Fields[22], back.Fields[22])
		}
	}
	for _, bad := range []string{`{"howLong":"1"}`, `{"howLong":"1.1234567890s"}`, `{"howLong":"xs"}`} {
		if _, err := Unmarshal([]byte(bad), desc, Options{}); err == nil {
			t.Errorf("%s should fail", bad)
		}
	}
}

func TestWrappersAndFieldMask(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	m, err := Unmarshal([]byte(`{"wrapped":"42","mask":"note,rank"}`), desc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	w := m.Fields[23].(*dyn.Message)
	if w.Fields[1].(int64) != 42 {
		t.Fatalf("wrapper = %v", w.Fields)
	}
	mask := m.Fields[24].(*dyn.Message)
	if len(mask.Fields[1].([]any)) != 2 {
		t.Fatalf("mask = %v", mask.Fields)
	}
	out, _ := Marshal(m, Options{})
	if !strings.Contains(string(out), `"wrapped":"42"`) || !strings.Contains(string(out), `"mask":"note,rank"`) {
		t.Fatalf("out = %s", out)
	}
}

func TestStructMapping(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	doc := `{"props":{"a":1,"b":[true,null,"x"],"c":{"d":2.5}}}`
	m, err := Unmarshal([]byte(doc), desc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	json.Unmarshal(out, &got)
	json.Unmarshal([]byte(doc), &want)
	g, _ := json.Marshal(got["props"])
	w, _ := json.Marshal(want["props"])
	if string(g) != string(w) {
		t.Fatalf("struct round trip: %s vs %s", g, w)
	}
}

func TestOneofExclusivityJSON(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	// Both members in one document: last one wins is not allowed at the
	// document level, so the decoder must reject it.
	if _, err := Unmarshal([]byte(`{"name":"a","id":"2"}`), desc, Options{}); err == nil {
		t.Fatal("two oneof members should be rejected")
	}
}

func TestMapKeys(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	doc := `{"counts":{"a":1,"b":2},"byId":{"7":{"note":"x"},"-3":{"rank":1}}}`
	m, err := Unmarshal([]byte(doc), desc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Fields[16].(dyn.Map)) != 2 || len(m.Fields[17].(dyn.Map)) != 2 {
		t.Fatalf("maps = %v", m.Fields)
	}
	out, _ := Marshal(m, Options{})
	if !strings.Contains(string(out), `"byId"`) {
		t.Fatalf("camelCase map name: %s", out)
	}
	// Keys are emitted as strings, sorted numerically for int64 keys.
	if !strings.Contains(string(out), `"-3"`) || !strings.Contains(string(out), `"7"`) {
		t.Fatalf("int keys: %s", out)
	}
}

func TestNullRejectedForScalars(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	if _, err := Unmarshal([]byte(`{"i32":null}`), desc, Options{}); err == nil {
		t.Fatal("null scalar should fail")
	}
	// null for a message field is accepted and means absent.
	if _, err := Unmarshal([]byte(`{"inner":null}`), desc, Options{}); err != nil {
		t.Fatalf("null message: %v", err)
	}
}

func TestIndent(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.Inner")
	m := dyn.New(desc)
	m.Fields[1] = "a"
	out, _ := Marshal(m, Options{Indent: "  "})
	if string(out) != "{\n  \"note\": \"a\"\n}" {
		t.Fatalf("indent output: %q", out)
	}
}

func TestRepeatedAndNested(t *testing.T) {
	reg := load(t)
	desc := reg.Message("jsontest.All")
	doc := `{"numbers":["1","2","3"],"inners":[{"note":"a","rank":1},{"rank":2}]}`
	m, err := Unmarshal([]byte(doc), desc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	list := m.Fields[14].([]any)
	if len(list) != 3 || list[0].(int64) != 1 {
		t.Fatalf("numbers = %v", list)
	}
	inners := m.Fields[15].([]any)
	if len(inners) != 2 {
		t.Fatalf("inners = %v", inners)
	}
	out, _ := Marshal(m, Options{})
	if !strings.Contains(string(out), `"numbers":["1","2","3"]`) {
		t.Fatalf("out = %s", out)
	}
}

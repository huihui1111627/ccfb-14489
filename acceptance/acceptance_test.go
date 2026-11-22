// Package acceptance drives the complete gateway against hand-prepared
// expectations: route matching, request binding, JSON and binary conversion
// in both directions, and streaming/status behaviour.
//
// The expectations in expectations.json were computed by hand (and, for the
// byte vectors, cross-checked with the independent Python encoder in
// reference_encode.py). Nothing here was captured from the implementation, so
// a regression in either direction fails the suite.
package acceptance

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/gateway"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/jsoncodec"
	"example.com/idlbridge/internal/routing"
	"example.com/idlbridge/internal/transcode"
	"example.com/idlbridge/internal/wire"
)

func loadSchema(t *testing.T) *gateway.Schema {
	t.Helper()
	s, err := gateway.LoadDir(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	return s
}

type expectationFile struct {
	Routes   []routeExpectation   `json:"routes"`
	Bindings []bindingExpectation `json:"bindings"`
}

type routeExpectation struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	WantMethod string            `json:"wantMethod"`
	WantVars   map[string]string `json:"wantVars"`
	Why        string            `json:"why"`
}

type bindingExpectation struct {
	Name           string         `json:"name"`
	Method         string         `json:"method"`
	Path           string         `json:"path"`
	Query          string         `json:"query"`
	Body           string         `json:"body"`
	Why            string         `json:"why"`
	WantJSONFields map[string]any `json:"wantJSONFields"`
	WantNested     map[string]any `json:"wantNested"`
	WantHex        string         `json:"wantHex"`
	ExpectError    bool           `json:"expectError"`
}

func loadExpectations(t *testing.T) expectationFile {
	t.Helper()
	data, err := os.ReadFile("expectations.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex expectationFile
	if err := json.Unmarshal(data, &ex); err != nil {
		t.Fatalf("expectations.json: %v", err)
	}
	return ex
}

func TestRouteExpectations(t *testing.T) {
	schema := loadSchema(t)
	ex := loadExpectations(t)
	for _, tc := range ex.Routes {
		t.Run(tc.Method+" "+tc.Path, func(t *testing.T) {
			match, ok := schema.Table.Lookup(tc.Method, tc.Path)
			if tc.WantMethod == "" {
				if ok {
					b, _ := match.Route.Data.(*transcode.Binding)
					t.Fatalf("expected no match (%s), got %s", tc.Why, b.Method.FullName)
				}
				return
			}
			if !ok {
				t.Fatalf("expected %s (%s), got no match", tc.WantMethod, tc.Why)
			}
			b := match.Route.Data.(*transcode.Binding)
			if b.Method.FullName != tc.WantMethod {
				t.Fatalf("matched %s, want %s (%s)", b.Method.FullName, tc.WantMethod, tc.Why)
			}
			for k, want := range tc.WantVars {
				if got := match.Vars[k]; got != want {
					t.Fatalf("var %s = %q want %q", k, got, want)
				}
			}
			if len(match.Vars) != len(tc.WantVars) {
				t.Fatalf("extra vars: got %v want %v", match.Vars, tc.WantVars)
			}
		})
	}
}

func TestBindingExpectations(t *testing.T) {
	schema := loadSchema(t)
	ex := loadExpectations(t)
	for _, tc := range ex.Bindings {
		t.Run(tc.Name, func(t *testing.T) {
			match, ok := schema.Table.Lookup(tc.Method, tc.Path)
			if !ok {
				t.Fatalf("no route for %s %s", tc.Method, tc.Path)
			}
			b := match.Route.Data.(*transcode.Binding)
			query, err := routing.ParseQuery(tc.Query)
			if err != nil {
				t.Fatal(err)
			}
			msg, _, err := transcode.BindRequest(b, match, []byte(tc.Body), query, transcode.BindOptions{})
			if tc.ExpectError {
				if err == nil {
					t.Fatalf("expected a binding error (%s), got %v", tc.Why, msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			// Compare the JSON view against the expected subset.
			out, err := jsoncodec.Marshal(msg, jsoncodec.Options{})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			for k, want := range tc.WantJSONFields {
				gotV, ok := got[k]
				if !ok {
					t.Fatalf("missing JSON field %q in %s", k, out)
				}
				if !jsonEqual(gotV, want) {
					t.Fatalf("field %q = %v (%T), want %v (%T)", k, gotV, gotV, want, want)
				}
			}
			for path, want := range tc.WantNested {
				gotV := lookupJSONPath(got, path)
				if !jsonEqual(gotV, want) {
					t.Fatalf("nested %q = %v, want %v", path, gotV, want)
				}
			}
			if tc.WantHex != "" {
				raw, err := wire.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				if got := hex.EncodeToString(raw); got != tc.WantHex {
					t.Fatalf("wire = %s want %s", got, tc.WantHex)
				}
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func lookupJSONPath(root map[string]any, path string) any {
	var cur any = root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// TestWireReferenceVectors checks encodings produced by our encoder against
// bytes generated by the independent Python implementation.
func TestWireReferenceVectors(t *testing.T) {
	schema := loadSchema(t)
	reg := schema.Registry

	book := reg.Message("library.v1.Book")
	b := dyn.New(book)
	b.Fields[1] = "dune"
	b.Fields[2] = "Dune"
	b.Fields[3] = int64(1234567890123)
	b.Fields[4] = []any{"a"}
	b.Fields[5] = int32(1)
	raw, err := wire.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	const bookWant = "0a0464756e65120444756e6518cb89ec8ff7232201612801"
	if got := hex.EncodeToString(raw); got != bookWant {
		t.Fatalf("book wire mismatch\n got %s\nwant %s", got, bookWant)
	}
	back, err := wire.Unmarshal(raw, book)
	if err != nil {
		t.Fatal(err)
	}
	if !dyn.Equal(b, back) {
		t.Fatal("book round trip mismatch")
	}

	// Every scalar type in one message.
	bagDesc := reg.Message("library.v1.ScalarBag")
	bag := dyn.New(bagDesc)
	bag.Fields[1] = 1.5
	bag.Fields[2] = float32(-2.25)
	bag.Fields[3] = int32(-1)
	bag.Fields[4] = int64(-2)
	bag.Fields[5] = uint32(300)
	bag.Fields[6] = uint64(math.MaxUint64)
	bag.Fields[7] = int32(-7)
	bag.Fields[8] = int64(-8)
	bag.Fields[9] = uint32(0xDEADBEEF)
	bag.Fields[10] = uint64(0x0102030405060708)
	bag.Fields[11] = int32(-9)
	bag.Fields[12] = int64(-10)
	bag.Fields[13] = true
	bag.Fields[14] = "héllo"
	bag.Fields[15] = []byte{0x00, 0xff, 0x10}
	bag.Fields[16] = int32(2)
	bag.Fields[17] = []any{int32(1), int32(-1), int32(2)}
	bag.Fields[18] = []any{int32(5), int32(6)}
	raw, err = wire.Marshal(bag)
	if err != nil {
		t.Fatal(err)
	}
	const bagWant = "09000000000000f83f15000010c018ffffffffffffffffff0120feffffffffffffffff0128ac0230ffffffffffffffffff01380d400f4defbeadde5108070605040302015df7ffffff61f6ffffffffffffff6801720668c3a96c6c6f7a0300ff108001028a010c01ffffffffffffffffff0102900105900106"
	if got := hex.EncodeToString(raw); got != bagWant {
		t.Fatalf("scalarbag wire mismatch\n got %s\nwant %s", got, bagWant)
	}
	back, err = wire.Unmarshal(raw, bagDesc)
	if err != nil {
		t.Fatal(err)
	}
	if !dyn.Equal(bag, back) {
		t.Fatalf("scalarbag round trip mismatch")
	}

	// Duration and Timestamp payload encodings.
	durDesc := reg.Message("google.protobuf.Duration")
	dur := dyn.New(durDesc)
	dur.Fields[1] = int64(1)
	dur.Fields[2] = int32(500000000)
	raw, _ = wire.Marshal(dur)
	if got := hex.EncodeToString(raw); got != "08011080cab5ee01" {
		t.Fatalf("duration wire = %s", got)
	}

	tsDesc := reg.Message("google.protobuf.Timestamp")
	ts := dyn.New(tsDesc)
	ts.Fields[1] = int64(1700000000)
	ts.Fields[2] = int32(120000000)
	raw, _ = wire.Marshal(ts)
	if got := hex.EncodeToString(raw); got != "0880e2cfaa0610809c9c39" {
		t.Fatalf("timestamp wire = %s", got)
	}
}

// TestJSONReferenceDocuments pins the exact JSON emitted for a handful of
// hand-written messages, exercising names, 64-bit strings, enums, maps,
// timestamps and durations together.
func TestJSONReferenceDocuments(t *testing.T) {
	schema := loadSchema(t)
	reg := schema.Registry
	metaDesc := reg.Message("library.v1.CatalogMeta")
	m := dyn.New(metaDesc)
	ts := dyn.New(reg.Message("google.protobuf.Timestamp"))
	ts.Fields[1] = int64(1700000000)
	ts.Fields[2] = int32(120000000)
	m.Fields[1] = ts
	dur := dyn.New(reg.Message("google.protobuf.Duration"))
	dur.Fields[1] = int64(1)
	dur.Fields[2] = int32(500000000)
	m.Fields[2] = dur
	wrapped := dyn.New(reg.Message("google.protobuf.Int64Value"))
	wrapped.Fields[1] = int64(42)
	m.Fields[3] = wrapped
	curator := dyn.New(reg.Message("google.protobuf.StringValue"))
	curator.Fields[1] = "ada"
	m.Fields[4] = curator
	m.Fields[5] = dyn.Map{"env": "prod", "team": "lib"}

	out, err := jsoncodec.Marshal(m, jsoncodec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// encoding/json sorts object keys lexicographically, so the document is
	// stable; the expectation is written in that order.
	const want = `{"curator":"ada","labels":{"env":"prod","team":"lib"},"refreshInterval":"1.500s","totalBooks":"42","updatedAt":"2023-11-14T22:13:20.120Z"}`
	if string(out) != want {
		t.Fatalf("json mismatch\n got %s\nwant %s", out, want)
	}
	back, err := jsoncodec.Unmarshal([]byte(want), metaDesc, jsoncodec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !dyn.Equal(m, back) {
		t.Fatalf("json round trip mismatch")
	}

	// snake_case input for every field must produce the same message.
	snake := `{"updated_at":"2023-11-14T22:13:20.120Z","refresh_interval":"1.500s","total_books":"42","curator":"ada","labels":{"team":"lib","env":"prod"}}`
	back2, err := jsoncodec.Unmarshal([]byte(snake), metaDesc, jsoncodec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !dyn.Equal(m, back2) {
		t.Fatalf("snake_case JSON differs: %v", back2)
	}
}

// TestEnumAndOneofJSON pins enum names and oneof selection.
func TestEnumAndOneofJSON(t *testing.T) {
	schema := loadSchema(t)
	reg := schema.Registry
	choiceDesc := reg.Message("library.v1.ChoiceRequest")
	m := dyn.New(choiceDesc)
	m.Fields[2] = int64(77)
	m.Fields[4] = "note"
	out, _ := jsoncodec.Marshal(m, jsoncodec.Options{})
	if string(out) != `{"note":"note","shelfId":"77"}` {
		t.Fatalf("oneof json = %s", out)
	}
	// Selecting the other member drops the first.
	parsed, err := jsoncodec.Unmarshal([]byte(`{"bookName":"x"}`), choiceDesc, jsoncodec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.Fields[2]; ok {
		t.Fatal("oneof member not cleared")
	}
	if parsed.Fields[1] != "x" {
		t.Fatalf("bookName = %v", parsed.Fields[1])
	}
	out, _ = jsoncodec.Marshal(parsed, jsoncodec.Options{})
	if string(out) != `{"bookName":"x"}` {
		t.Fatalf("json = %s", out)
	}
}

// TestBinaryToJSONCrossConversion checks that a message produced only from
// binary input renders to the same JSON as the original.
func TestBinaryToJSONCrossConversion(t *testing.T) {
	schema := loadSchema(t)
	reg := schema.Registry
	allDesc := reg.Message("library.v1.ScalarBag")
	// Build from JSON, encode to binary, decode the binary, and compare the
	// JSON of both.
	doc := `{
	  "d": 1.5, "f": -2.25, "i32": -1, "i64": "-2", "u32": 300,
	  "u64": "18446744073709551615", "s32": -7, "s64": "-8",
	  "fx32": 3735928559, "fx64": "72623859790382856",
	  "sfx32": -9, "sfx64": "-10", "flag": true, "text": "héllo",
	  "blob": "AP8Q", "genre": "NONFICTION",
	  "packedInts": [1, -1, 2], "unpackedInts": [5, 6],
	  "byId": {"4": "four"}, "shelves": {"top": {"name": "t", "topic": "sci"}}
	}`
	m, err := jsoncodec.Unmarshal([]byte(doc), allDesc, jsoncodec.Options{})
	if err != nil {
		t.Fatalf("json -> message: %v", err)
	}
	raw, err := wire.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	fromBin, err := wire.Unmarshal(raw, allDesc)
	if err != nil {
		t.Fatalf("binary -> message: %v", err)
	}
	a, _ := jsoncodec.Marshal(m, jsoncodec.Options{})
	b, _ := jsoncodec.Marshal(fromBin, jsoncodec.Options{})
	if string(a) != string(b) {
		t.Fatalf("cross conversion differs\njson: %s\nbin:  %s", a, b)
	}
	if !dyn.Equal(m, fromBin) {
		t.Fatal("messages differ after binary round trip")
	}
}

// TestRandomJSONBinaryRoundTrip fuzzes the JSON <-> message <-> binary path.
func TestRandomJSONBinaryRoundTrip(t *testing.T) {
	schema := loadSchema(t)
	bagDesc := schema.Registry.Message("library.v1.ScalarBag")
	// deterministic pseudo-random messages built directly
	seed := uint64(12345)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for i := 0; i < 300; i++ {
		m := dyn.New(bagDesc)
		for _, f := range bagDesc.Fields {
			if next()%3 == 0 {
				continue
			}
			switch {
			case f.Map:
				mm := dyn.Map{}
				for j := 0; j < int(next()%3); j++ {
					var k any
					if f.MapKey == idl.TString {
						k = "k" + itoa(int(next()%10))
					} else {
						k = int64(next()%20) - 10
					}
					if f.MapValue.Message != nil {
						child := dyn.New(f.MapValue.Message)
						if f.MapValue.Message.FullName == "library.v1.Shelf" {
							child.Fields[1] = "n" + itoa(int(next()%90))
						}
						mm[k] = child
					} else {
						mm[k] = randomScalar(f.MapValue, next())
					}
				}
				if len(mm) > 0 {
					m.Set(f, mm)
				}
			case f.Repeated:
				if f.Type.Message != nil {
					continue
				}
				n := int(next() % 4)
				if n == 0 {
					continue
				}
				list := make([]any, 0, n)
				for j := 0; j < n; j++ {
					list = append(list, randomScalar(f.Type, next()))
				}
				m.Set(f, list)
			case f.Type.Message != nil:
				child := dyn.New(f.Type.Message)
				if f.Type.Message.FullName == "library.v1.Shelf" {
					child.Fields[1] = "n" + itoa(int(next()%90))
					child.Fields[2] = "t"
				}
				m.Set(f, child)
			default:
				m.Set(f, randomScalar(f.Type, next()))
			}
		}
		// EmitDefaults keeps explicitly set zero values (such as false and
		// 0) visible; without it proto3 presence rules would drop them and
		// the comparison would be stricter than the format.
		j1, err := jsoncodec.Marshal(m, jsoncodec.Options{EmitDefaults: true})
		if err != nil {
			t.Fatalf("iteration %d: marshal: %v", i, err)
		}
		parsed, err := jsoncodec.Unmarshal(j1, bagDesc, jsoncodec.Options{})
		if err != nil {
			t.Fatalf("iteration %d: unmarshal %s: %v", i, j1, err)
		}
		if !dyn.Equal(m, parsed) {
			t.Fatalf("iteration %d: json round trip mismatch\n%s", i, j1)
		}
		raw, err := wire.Marshal(m)
		if err != nil {
			t.Fatalf("iteration %d: wire: %v", i, err)
		}
		fromBin, err := wire.Unmarshal(raw, bagDesc)
		if err != nil {
			t.Fatalf("iteration %d: wire decode: %v", i, err)
		}
		if !dyn.Equal(m, fromBin) {
			t.Fatalf("iteration %d: binary round trip mismatch", i)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func fmtInt(n int) string { return strconv.Itoa(n) }

func randomScalar(t idl.Type, r uint64) any {
	if t.Enum != nil {
		return int32(r % 4)
	}
	switch t.Scalar {
	case idl.TDouble:
		return float64(r%1000) / 7
	case idl.TFloat:
		return float32(r%1000) / 7
	case idl.TInt32, idl.TSint32, idl.TSfixed32:
		return int32(r % 100000)
	case idl.TInt64, idl.TSint64, idl.TSfixed64:
		return int64(r % 100000)
	case idl.TUint32, idl.TFixed32:
		return uint32(r % 100000)
	case idl.TUint64, idl.TFixed64:
		return r % 100000
	case idl.TBool:
		return r%2 == 0
	case idl.TString:
		return "s" + fmtInt(int(r%1000))
	case idl.TBytes:
		return []byte{byte(r), byte(r >> 8)}
	}
	return nil
}

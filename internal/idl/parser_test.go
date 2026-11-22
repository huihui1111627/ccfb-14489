package idl

import (
	"strings"
	"testing"
)

const sample = `
syntax = "proto3";
package acme.v1;

import "google/protobuf/timestamp.proto";

option go_package = "example.com/acme/v1";

// A book.
message Book {
  string name = 1;
  int64 isbn = 2;
  Status status = 3;
  repeated string tags = 4;
  map<string, Organization> authors = 5;
  oneof owner {
    string user_id = 6;
    Organization org = 7;
  }
  optional bool featured = 8 [deprecated = true];
  repeated int32 nums = 9 [packed = false];
  google.protobuf.Timestamp published_at = 10 [json_name = "published"];
  Inner inner = 11;

  message Inner {
    string value = 1;
    Status status = 2; // resolves to the file-level enum via scoping
  }
}

enum Status {
  option allow_alias = true;
  STATUS_UNSPECIFIED = 0;
  ACTIVE = 1;
  ENABLED = 1;
}

message Organization {
  string id = 1;
}

service Books {
  rpc GetBook(Book) returns (Book) {
    option (google.api.http) = {
      get: "/v1/{name=books/*}"
      additional_bindings { get: "/v1/books" }
    };
  }
  rpc Watch(Book) returns (stream Book) {
    option (google.api.http) = { get: "/v1/watch" };
  }
  rpc Push(stream Book) returns (Organization);
}
`

func parseSample(t *testing.T) (*Registry, *File) {
	t.Helper()
	reg := NewRegistry()
	f, err := ParseFile(sample, "acme.proto", "", reg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := reg.Link(); err != nil {
		t.Fatalf("link: %v", err)
	}
	return reg, f
}

func TestParseStructure(t *testing.T) {
	reg, f := parseSample(t)
	if f.Package != "acme.v1" || f.Syntax != "proto3" {
		t.Fatalf("file = %+v", f)
	}
	book := reg.Message("acme.v1.Book")
	if book == nil {
		t.Fatal("Book not registered")
	}
	if len(book.Fields) != 11 {
		t.Fatalf("fields = %d", len(book.Fields))
	}
	// Field numbers, names and types.
	checks := []struct {
		num    int32
		name   string
		scalar ScalarType
	}{
		{1, "name", TString},
		{2, "isbn", TInt64},
		{4, "tags", TString},
		{8, "featured", TBool},
		{9, "nums", TInt32},
	}
	for _, c := range checks {
		fl := book.FieldByNumber(c.num)
		if fl == nil || fl.Name != c.name || fl.Type.Scalar != c.scalar {
			t.Fatalf("field %d = %+v", c.num, fl)
		}
	}
	if fl := book.FieldByNumber(3); fl.Type.Enum == nil || fl.Type.Enum.FullName != "acme.v1.Status" {
		t.Fatalf("status type = %+v", fl.Type)
	}
	if fl := book.FieldByNumber(5); !fl.Map || fl.MapKey != TString || fl.MapValue.Message == nil {
		t.Fatalf("authors = %+v", fl)
	}
	if fl := book.FieldByNumber(10); fl.Type.Message.FullName != TimestampName || fl.JSONName != "published" {
		t.Fatalf("timestamp = %+v", fl)
	}
	if fl := book.FieldByNumber(9); fl.IsPacked() {
		t.Fatal("nums should be unpacked")
	}
	if fl := book.FieldByNumber(4); fl.IsPacked() {
		t.Fatal("repeated strings are never packed")
	}
	if fl := book.FieldByNumber(8); !fl.Optional || !fl.HasPresence() {
		t.Fatal("optional field must have presence")
	}
	// Nested message and scope resolution.
	inner := reg.Message("acme.v1.Book.Inner")
	if inner == nil {
		t.Fatal("nested message missing")
	}
	if fl := inner.FieldByNumber(2); fl.Type.Enum == nil {
		t.Fatalf("nested scope resolution failed: %+v", fl)
	}
	// Oneofs.
	if len(book.Oneofs) != 1 || book.Oneofs[0].Name != "owner" {
		t.Fatalf("oneofs = %+v", book.Oneofs)
	}
	if book.FieldByNumber(6).Oneof == nil || book.FieldByNumber(7).Oneof == nil {
		t.Fatal("oneof members not linked")
	}
	// JSON names.
	if got := book.FieldByNumber(10).JSONName; got != "published" {
		t.Fatalf("json_name override = %q", got)
	}
	// Enum aliases.
	status := reg.Enum("acme.v1.Status")
	if !status.AllowAlias {
		t.Fatal("allow_alias not parsed")
	}
	if status.ValueByName("ENABLED") == nil || status.ValueByName("ENABLED").Number != 1 {
		t.Fatal("alias value missing")
	}
	if status.ValueByNumber(1).Name != "ACTIVE" {
		t.Fatalf("first name should be canonical: %v", status.ValueByNumber(1))
	}
}

func TestParseServicesAndRoutes(t *testing.T) {
	reg, _ := parseSample(t)
	svc := reg.Services[0]
	if svc.FullName != "acme.v1.Books" || len(svc.Methods) != 3 {
		t.Fatalf("service = %+v", svc)
	}
	get := svc.Methods[0]
	if get.Input.FullName != "acme.v1.Book" || get.Output.FullName != "acme.v1.Book" {
		t.Fatalf("types = %v -> %v", get.Input, get.Output)
	}
	if len(get.HTTP) != 1 || get.HTTP[0].Method != "GET" || get.HTTP[0].Path != "/v1/{name=books/*}" {
		t.Fatalf("http = %+v", get.HTTP)
	}
	if len(get.HTTP[0].Additional) != 1 || get.HTTP[0].Additional[0].Path != "/v1/books" {
		t.Fatalf("additional = %+v", get.HTTP[0].Additional)
	}
	if !svc.Methods[1].ServerStreaming {
		t.Fatal("Watch should be server streaming")
	}
	if !svc.Methods[2].ClientStreaming || svc.Methods[2].ServerStreaming {
		t.Fatal("Push should be client streaming")
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`syntax = "proto3"; message M { string a = 1; string b = 1; }`, "field number 1 used by both"},
		{`syntax = "proto3"; message M { Foo a = 1; string b = 2; }`, "unknown type"},
		{`syntax = "proto3"; message M { string a = 0; }`, "invalid number"},
		{`syntax = "proto3"; message M { string a = 19000; }`, "reserved for protobuf internals"},
		{`syntax = "proto3"; message M { string a = 1 }`, `expected ";"`},
		{`syntax = "proto3"; message M {`, "unexpected end of file"},
		{`syntax = "proto4";`, "unsupported syntax"},
		{`syntax = "proto3"; service S { rpc M(Nope) returns (Nope); }`, "unknown request type"},
		{`syntax = "proto3"; message M { map<double, string> m = 1; }`, "invalid map key type"},
		{`/* unterminated`, "unterminated block comment"},
	}
	for _, tc := range cases {
		reg := NewRegistry()
		_, err := ParseFile(tc.src, "bad.proto", "", reg)
		if err == nil {
			err = reg.Link()
		}
		if err == nil {
			t.Fatalf("%s: expected error containing %q", tc.src, tc.want)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q does not contain %q", tc.src, err, tc.want)
		}
	}
}

func TestJSONName(t *testing.T) {
	cases := map[string]string{
		"foo_bar":      "fooBar",
		"foo":          "foo",
		"_foo":         "Foo",
		"foo__bar":     "fooBar",
		"foo_1bar":     "foo1bar",
		"URL":          "URL",
		"a_b_c":        "aBC",
		"alreadyCamel": "alreadyCamel",
	}
	for in, want := range cases {
		if got := JSONName(in); got != want {
			t.Errorf("JSONName(%q) = %q want %q", in, got, want)
		}
	}
	if got := SnakeName("fooBar"); got != "foo_bar" {
		t.Errorf("SnakeName = %q", got)
	}
}

func TestCommentsAndStrings(t *testing.T) {
	src := `
syntax = "proto3";
// line comment
message M {
  /* block
     comment */
  string a = 1; // trailing
  string b = 2 [json_name = "b\"quoted"];
}`
	reg := NewRegistry()
	if _, err := ParseFile(src, "c.proto", "", reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Link(); err != nil {
		t.Fatal(err)
	}
	if got := reg.Message("M").FieldByNumber(2).JSONName; got != `b"quoted` {
		t.Fatalf("json_name = %q", got)
	}
}

func TestGroupedPackageScoping(t *testing.T) {
	src := `
syntax = "proto3";
package a.b;
message Outer { message Mid { message Leaf { int32 x = 1; } } }
message User {
  Outer.Mid.Leaf leaf = 1;
  Mid.Leaf relative = 2;
}`
	reg := NewRegistry()
	if _, err := ParseFile(src, "s.proto", "", reg); err != nil {
		t.Fatal(err)
	}
	// Mid is nested under Outer, not the package, so "Mid.Leaf" from User
	// is not resolvable and linking must fail with a clear message.
	if err := reg.Link(); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("expected scoping error, got %v", err)
	}
}

func TestWellKnownClassification(t *testing.T) {
	reg := NewRegistry()
	cases := map[string]WellKnownKind{
		TimestampName:                KindTimestamp,
		DurationName:                 KindDuration,
		FieldMaskName:                KindFieldMask,
		AnyName:                      KindAny,
		EmptyName:                    KindEmpty,
		StructName:                   KindStruct,
		ValueName:                    KindValue,
		ListValueName:                KindListValue,
		"google.protobuf.Int64Value": KindWrapper,
	}
	for name, want := range cases {
		if got := WellKnown(reg.Message(name)); got != want {
			t.Errorf("%s = %v want %v", name, got, want)
		}
	}
}

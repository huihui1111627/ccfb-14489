package transcode_test

import (
	"testing"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/gateway"
	"example.com/idlbridge/internal/routing"
	"example.com/idlbridge/internal/transcode"
)

func loadTestSchema(t *testing.T, src string) *gateway.Schema {
	t.Helper()
	sch, err := gateway.LoadSource("test.proto", src)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func TestPrecedencePathBodyQuery(t *testing.T) {
	schema := loadTestSchema(t, `
syntax = "proto3";
package p;
message Bag { string text = 1; int32 n = 2; }
message Req { string id = 1; Bag bag = 2; string extra = 3; }
message Resp { string id = 1; }
service S {
  rpc Echo(Req) returns (Resp) {
    option (google.api.http) = { post: "/v1/{bag.text}/x" body: "bag" };
  }
}`)
	match, ok := schema.Table.Lookup("POST", "/v1/frompath/x")
	if !ok {
		t.Fatal("no route")
	}
	b := match.Route.Data.(*transcode.Binding)
	query, _ := routing.ParseQuery("bag.text=fromquery&bag.n=7&extra=q&id=qid")
	body := []byte(`{"text":"frombody","n":1}`)
	msg, report, err := transcode.BindRequest(b, match, body, query, transcode.BindOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bag := msg.Fields[2].(*dyn.Message)
	if bag.Fields[1] != "frompath" {
		t.Fatalf("path must win: %v", bag.Fields[1])
	}
	if bag.Fields[2].(int32) != 1 {
		t.Fatalf("body wins over query: %v", bag.Fields[2])
	}
	if msg.Fields[3] != "q" || msg.Fields[1] != "qid" {
		t.Fatalf("query fills unset fields: %v", msg.Fields)
	}
	// path + two query fields; the body is reported separately (not as
	// per-field assignments) because it is one JSON document.
	if len(report.Assignments) != 3 {
		t.Fatalf("assignments = %+v", report.Assignments)
	}
	if !report.Body {
		t.Fatal("body should be marked present")
	}
}

func TestBodyProtectsTopLevelQuery(t *testing.T) {
	schema := loadTestSchema(t, `
syntax = "proto3";
package p;
message Req { string a = 1; string b = 2; }
message Resp { string a = 1; }
service S {
  rpc E(Req) returns (Resp) { option (google.api.http) = { post: "/v1/x" body: "*" }; }
}`)
	match, _ := schema.Table.Lookup("POST", "/v1/x")
	b := match.Route.Data.(*transcode.Binding)
	query, _ := routing.ParseQuery("a=fromquery&b=fromquery")
	msg, _, err := transcode.BindRequest(b, match, []byte(`{"a":"frombody"}`), query, transcode.BindOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Fields[1] != "frombody" {
		t.Fatalf("body must win: %v", msg.Fields[1])
	}
	if msg.Fields[2] != "fromquery" {
		t.Fatalf("query fills unset field: %v", msg.Fields[2])
	}
}

func TestBodyMessageSubfieldFromQuery(t *testing.T) {
	// body "bag" covers the whole bag message, but the query may still
	// address subfields the body left out (proto3 absent vs zero).
	schema := loadTestSchema(t, `
syntax = "proto3";
package p;
message Bag { string text = 1; int32 n = 2; }
message Req { Bag bag = 1; }
message Resp { string text = 1; }
service S {
  rpc E(Req) returns (Resp) { option (google.api.http) = { post: "/v1/x" body: "bag" }; }
}`)
	match, _ := schema.Table.Lookup("POST", "/v1/x")
	b := match.Route.Data.(*transcode.Binding)
	query, _ := routing.ParseQuery("bag.n=9")
	msg, _, err := transcode.BindRequest(b, match, []byte(`{"text":"t"}`), query, transcode.BindOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bag := msg.Fields[1].(*dyn.Message)
	if bag.Fields[1] != "t" || bag.Fields[2].(int32) != 9 {
		t.Fatalf("bag = %v", bag.Fields)
	}
}

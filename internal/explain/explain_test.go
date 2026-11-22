package explain

import (
	"fmt"
	"strings"
	"testing"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/wire"
)

func testDesc(t *testing.T) *idl.Message {
	t.Helper()
	const src = `
syntax = "proto3";
package ex;
enum Kind { KIND_UNSPECIFIED = 0; A = 1; B = 2; }
message Child { string name = 1; }
message Parent {
  int32 id = 1;
  string label = 2;
  repeated int32 nums = 3;
  Child child = 4;
  map<string, int32> counts = 5;
  Kind kind = 6;
}`
	reg := idl.NewRegistry()
	if _, err := idl.ParseFile(src, "ex.proto", "", reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Link(); err != nil {
		t.Fatal(err)
	}
	return reg.Message("ex.Parent")
}

func TestDecodeLayoutCoversEveryField(t *testing.T) {
	desc := testDesc(t)
	m := dyn.New(desc)
	m.Fields[1] = int32(7)
	m.Fields[2] = "hi"
	m.Fields[3] = []any{int32(1), int32(2), int32(3)}
	child := dyn.New(desc.FieldByNumber(4).Type.Message)
	child.Fields[1] = "kid"
	m.Fields[4] = child
	m.Fields[5] = dyn.Map{"k": int32(9)}
	m.Fields[6] = int32(2)

	raw, err := wire.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	info := Decode(raw, desc)
	if info.Error != "" {
		t.Fatalf("decode error: %s", info.Error)
	}
	var labels []string
	var walk func(*Node)
	walk = func(n *Node) {
		labels = append(labels, n.Label)
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, f := range info.Root.Children {
		walk(f)
	}
	joined := strings.Join(labels, ",")
	for _, want := range []string{"id", "label", "nums", "child", "counts", "kind", "packed element", "key", "value"} {
		if !strings.Contains(joined, want) {
			t.Errorf("layout missing %q: %s", want, joined)
		}
	}
	// Every node's offset+length must be inside the buffer.
	var verify func(*Node)
	verify = func(n *Node) {
		if n.Offset < 0 || n.Length < 0 || n.Offset+n.Length > len(raw) {
			t.Errorf("node %q out of bounds: %d+%d > %d", n.Label, n.Offset, n.Length, len(raw))
		}
		for _, c := range n.Children {
			verify(c)
		}
	}
	verify(info.Root)

	rendered := info.Render(raw)
	if !strings.Contains(rendered, "child (#4)") {
		t.Fatalf("rendered layout:\n%s", rendered)
	}
	// Nested messages must be indented.
	if !strings.Contains(rendered, "\n  +") {
		t.Fatalf("expected nested indentation:\n%s", rendered)
	}
}

func TestDecodeUnknownAndErrors(t *testing.T) {
	desc := testDesc(t)
	// Field 99 (unknown) + field 1.
	raw := []byte{0x98, 0x06, 0x2a, 0x08, 0x01}
	info := Decode(raw, desc)
	if info.Unknown != 1 {
		t.Fatalf("unknown count = %d", info.Unknown)
	}
	if !strings.Contains(info.Render(raw), "unknown") {
		t.Fatalf("render:\n%s", info.Render(raw))
	}
	// Truncated input reports an error rather than panicking.
	bad := []byte{0x12, 0x10, 0x61}
	if got := Decode(bad, desc); got.Error == "" {
		t.Fatal("expected a decode error for truncated input")
	}
}

func TestDecodeMatchesEncoderForKnownPrefix(t *testing.T) {
	desc := testDesc(t)
	m := dyn.New(desc)
	m.Fields[1] = int32(-1)
	raw, _ := wire.Marshal(m)
	if fmt.Sprintf("%x", raw) != "08ffffffffffffffffff01" {
		t.Fatalf("encoder output %x", raw)
	}
	info := Decode(raw, desc)
	if len(info.Root.Children) != 1 || info.Root.Children[0].Value != "-1" {
		t.Fatalf("decoded = %+v", info.Root.Children)
	}
}

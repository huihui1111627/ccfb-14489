// Package explain renders a byte-by-byte account of a protobuf wire message
// against its descriptor. It is used by the inspector page and by the
// acceptance tests to verify encodings in detail.
package explain

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"example.com/idlbridge/internal/idl"
)

// Node is one annotated region of the buffer.
type Node struct {
	// Offset and Length locate the bytes in the original buffer.
	Offset int
	Length int
	// Label is the human-readable name, e.g. "schema_version" or "tag".
	Label string
	// Value is the decoded value as text.
	Value string
	// Kind is "field", "tag", "value", "length", "unknown" or "error".
	Kind string
	// WireType and FieldNumber are set for fields.
	FieldNumber int32
	WireType    int
	// Indent controls nesting in the rendered output.
	Indent int
	// Children are nested nodes (submessage contents).
	Children []*Node
	// Error is non-empty for a decoding problem.
	Error string
}

// DecodeInfo summarises the whole buffer.
type DecodeInfo struct {
	Root    *Node
	Fields  []*Node
	Bytes   int
	Unknown int
	Error   string
}

// Decode parses buf according to desc and returns an annotated tree.
func Decode(buf []byte, desc *idl.Message) *DecodeInfo {
	info := &DecodeInfo{Bytes: len(buf)}
	root := &Node{Kind: "message", Label: desc.FullName, Length: len(buf)}
	info.Root = root
	pos := 0
	depth := 0
	for pos < len(buf) {
		n, next, err := decodeOne(buf, pos, desc, depth)
		if n != nil {
			root.Children = append(root.Children, n)
			info.Fields = append(info.Fields, n)
			if n.Kind == "unknown" {
				info.Unknown++
			}
			if n.Error != "" && info.Error == "" {
				info.Error = n.Error
			}
		}
		if err != nil {
			info.Error = err.Error()
			break
		}
		if next <= pos {
			info.Error = "decoder made no progress"
			break
		}
		pos = next
	}
	return info
}

func decodeOne(buf []byte, pos int, desc *idl.Message, depth int) (*Node, int, error) {
	if depth > 64 {
		return nil, pos, fmt.Errorf("nesting too deep")
	}
	tagStart := pos
	tag, n := binary.Uvarint(buf[pos:])
	if n <= 0 {
		return &Node{Offset: pos, Length: 1, Kind: "error", Error: "truncated tag", Indent: depth}, pos, nil
	}
	num := int32(tag >> 3)
	wt := int(tag & 7)
	tagNode := &Node{
		Offset: tagStart, Length: n, Label: "tag", Kind: "tag",
		Value:       fmt.Sprintf("field %d, wire type %d", num, wt),
		FieldNumber: num, WireType: wt, Indent: depth,
	}
	pos += n
	f := desc.FieldByNumber(num)
	name := fmt.Sprintf("field_%d", num)
	kind := "field"
	if f != nil {
		name = f.Name
	}
	if f == nil {
		kind = "unknown"
	}
	end, err := skip(buf, pos, wt)
	if err != nil {
		return &Node{Offset: tagStart, Length: len(buf) - tagStart, Kind: "error", Label: name,
			Error: err.Error(), Indent: depth}, len(buf), nil
	}
	raw := buf[pos:end]
	field := &Node{
		Offset: tagStart, Length: end - tagStart, Label: name, Kind: kind,
		FieldNumber: num, WireType: wt, Indent: depth,
		Children: []*Node{tagNode},
	}
	if f == nil {
		field.Value = fmt.Sprintf("%d bytes skipped (unknown field, wire type %d)", len(raw), wt)
		return field, end, nil
	}
	// Decode the payload according to the field type.
	payloadStart := pos
	switch {
	case f.Map && wt == 2:
		l, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			field.Error = "truncated length"
			return field, end, nil
		}
		field.Children = append(field.Children, lengthNode(pos, ln, l, depth+1))
		body := buf[pos+ln : pos+ln+int(l)]
		entry := entryMessage(f)
		info := decodeSub(body, entry, depth+1)
		info.Offset += pos + ln
		field.Children = append(field.Children, info)
		field.Value = fmt.Sprintf("map entry (%d bytes)", len(body))
	case f.Repeated && f.Type.Packable() && wt == 2:
		l, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			field.Error = "truncated length"
			return field, end, nil
		}
		field.Children = append(field.Children, lengthNode(pos, ln, l, depth+1))
		elemBytes := buf[pos+ln : pos+ln+int(l)]
		off := pos + ln
		p := 0
		var vals []string
		for p < len(elemBytes) {
			v, next, err := decodeScalarNode(elemBytes, p, f.Type, depth+1)
			if err != nil {
				field.Error = err.Error()
				break
			}
			v.Offset += off
			v.Label = "packed element"
			field.Children = append(field.Children, v)
			vals = append(vals, v.Value)
			p = next
		}
		field.Value = "[" + strings.Join(vals, ", ") + "]"
	case f.Type.Message != nil && wt == 2:
		l, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			field.Error = "truncated length"
			return field, end, nil
		}
		field.Children = append(field.Children, lengthNode(pos, ln, l, depth+1))
		body := buf[pos+ln : pos+ln+int(l)]
		child := decodeSub(body, f.Type.Message, depth+1)
		child.Offset += pos + ln
		field.Children = append(field.Children, child)
		field.Value = fmt.Sprintf("%s (%d bytes)", f.Type.Message.FullName, len(body))
	case wt == 2 && (f.Type.Scalar == idl.TString || f.Type.Scalar == idl.TBytes):
		l, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			field.Error = "truncated length"
			return field, end, nil
		}
		field.Children = append(field.Children, lengthNode(pos, ln, l, depth+1))
		body := buf[pos+ln : pos+ln+int(l)]
		vn := &Node{Offset: pos + ln, Length: len(body), Kind: "value", Label: "bytes", Indent: depth + 1}
		if f.Type.Scalar == idl.TString {
			if utf8.Valid(body) {
				vn.Value = fmt.Sprintf("%q", string(body))
			} else {
				vn.Value = fmt.Sprintf("%q (invalid UTF-8)", string(body))
			}
		} else {
			vn.Value = "0x" + fmt.Sprintf("%x", body)
		}
		field.Children = append(field.Children, vn)
		field.Value = vn.Value
	default:
		vn, _, err := decodeScalarNode(buf, payloadStart, f.Type, depth+1)
		if err != nil {
			field.Error = err.Error()
			return field, end, nil
		}
		field.Children = append(field.Children, vn)
		field.Value = vn.Value
	}
	if f.Repeated && f.Type.Packable() && wt == 2 {
		// already summarised
	} else if f.Repeated {
		field.Value = "[" + field.Value + "]"
	}
	return field, end, nil
}

func entryMessage(f *idl.Field) *idl.Message {
	if f.MapEntry != nil {
		return f.MapEntry
	}
	return &idl.Message{
		Name:     f.Name + "Entry",
		FullName: "map entry",
		Fields: []*idl.Field{
			{Name: "key", JSONName: "key", Number: 1, Type: idl.Type{Scalar: f.MapKey}},
			{Name: "value", JSONName: "value", Number: 2, Type: f.MapValue},
		},
	}
}

func lengthNode(pos, lenBytes int, l uint64, indent int) *Node {
	return &Node{
		Offset: pos, Length: lenBytes, Label: "length", Kind: "length", Indent: indent,
		Value: fmt.Sprintf("%d bytes", l),
	}
}

func decodeSub(buf []byte, desc *idl.Message, depth int) *Node {
	n := &Node{Kind: "message", Label: desc.FullName, Length: len(buf), Indent: depth}
	pos := 0
	for pos < len(buf) {
		child, next, err := decodeOne(buf, pos, desc, depth)
		if child != nil {
			n.Children = append(n.Children, child)
		}
		if err != nil || next <= pos {
			break
		}
		pos = next
	}
	return n
}

func decodeScalarNode(buf []byte, pos int, t idl.Type, indent int) (*Node, int, error) {
	n := &Node{Offset: pos, Kind: "value", Label: "value", Indent: indent}
	if t.Enum != nil {
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated enum varint")
		}
		n.Length = ln
		ev := t.Enum.ValueByNumber(int32(v))
		if ev != nil {
			n.Value = fmt.Sprintf("%s (%d)", ev.Name, v)
		} else {
			n.Value = fmt.Sprintf("%d", v)
		}
		return n, pos + ln, nil
	}
	switch t.Scalar {
	case idl.TDouble:
		if len(buf)-pos < 8 {
			return n, pos, fmt.Errorf("truncated double")
		}
		n.Length = 8
		n.Value = fmt.Sprint(math.Float64frombits(binary.LittleEndian.Uint64(buf[pos:])))
	case idl.TFloat:
		if len(buf)-pos < 4 {
			return n, pos, fmt.Errorf("truncated float")
		}
		n.Length = 4
		n.Value = fmt.Sprint(math.Float32frombits(binary.LittleEndian.Uint32(buf[pos:])))
	case idl.TInt32:
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated int32")
		}
		n.Length, n.Value = ln, fmt.Sprintf("%d", int32(v))
	case idl.TInt64:
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated int64")
		}
		n.Length, n.Value = ln, fmt.Sprintf("%d", int64(v))
	case idl.TUint32:
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated uint32")
		}
		n.Length, n.Value = ln, fmt.Sprintf("%d", uint32(v))
	case idl.TUint64:
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated uint64")
		}
		n.Length, n.Value = ln, fmt.Sprintf("%d", int64(v))
	case idl.TSint32:
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated sint32")
		}
		u := uint32(v)
		z := int32(u>>1) ^ -int32(u&1)
		n.Length, n.Value = ln, fmt.Sprintf("%d (zigzag %d)", z, v)
	case idl.TSint64:
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated sint64")
		}
		z := int64(v>>1) ^ -int64(v&1)
		n.Length, n.Value = ln, fmt.Sprintf("%d (zigzag %d)", z, v)
	case idl.TFixed32:
		if len(buf)-pos < 4 {
			return n, pos, fmt.Errorf("truncated fixed32")
		}
		n.Length = 4
		n.Value = fmt.Sprintf("%d (0x%08x)", binary.LittleEndian.Uint32(buf[pos:]), binary.LittleEndian.Uint32(buf[pos:]))
	case idl.TFixed64:
		if len(buf)-pos < 8 {
			return n, pos, fmt.Errorf("truncated fixed64")
		}
		n.Length = 8
		n.Value = fmt.Sprintf("%d (0x%016x)", binary.LittleEndian.Uint64(buf[pos:]), binary.LittleEndian.Uint64(buf[pos:]))
	case idl.TSfixed32:
		if len(buf)-pos < 4 {
			return n, pos, fmt.Errorf("truncated sfixed32")
		}
		n.Length = 4
		n.Value = fmt.Sprint(int32(binary.LittleEndian.Uint32(buf[pos:])))
	case idl.TSfixed64:
		if len(buf)-pos < 8 {
			return n, pos, fmt.Errorf("truncated sfixed64")
		}
		n.Length = 8
		n.Value = fmt.Sprint(int64(binary.LittleEndian.Uint64(buf[pos:])))
	case idl.TBool:
		v, ln := binary.Uvarint(buf[pos:])
		if ln <= 0 {
			return n, pos, fmt.Errorf("truncated bool")
		}
		n.Length, n.Value = ln, fmt.Sprintf("%t", v != 0)
	default:
		return n, pos, fmt.Errorf("unsupported type %v", t.Scalar)
	}
	return n, pos + n.Length, nil
}

func skip(buf []byte, pos, wt int) (int, error) {
	switch wt {
	case 0:
		_, n := binary.Uvarint(buf[pos:])
		if n <= 0 {
			return pos, fmt.Errorf("truncated varint")
		}
		return pos + n, nil
	case 1:
		if len(buf)-pos < 8 {
			return pos, fmt.Errorf("truncated fixed64")
		}
		return pos + 8, nil
	case 2:
		l, n := binary.Uvarint(buf[pos:])
		if n <= 0 {
			return pos, fmt.Errorf("truncated length")
		}
		if l > uint64(len(buf)-pos-n) {
			return pos, fmt.Errorf("length %d exceeds remaining bytes", l)
		}
		return pos + n + int(l), nil
	case 5:
		if len(buf)-pos < 4 {
			return pos, fmt.Errorf("truncated fixed32")
		}
		return pos + 4, nil
	}
	return pos, fmt.Errorf("unsupported wire type %d", wt)
}

// Render produces a plain-text listing with offsets, hex bytes and labels.
func (d *DecodeInfo) Render(buf []byte) string {
	var b strings.Builder
	var walk func(n *Node, depth int)
	walk = func(n *Node, depth int) {
		indent := strings.Repeat("  ", depth)
		raw := ""
		if n.Offset >= 0 && n.Offset < len(buf) && n.Length > 0 {
			end := n.Offset + n.Length
			if end > len(buf) {
				end = len(buf)
			}
			raw = hex(buf[n.Offset:end])
		}
		label := n.Label
		if n.FieldNumber != 0 {
			label = fmt.Sprintf("%s (#%d)", n.Label, n.FieldNumber)
		}
		line := fmt.Sprintf("%s%-8s %-28s %-16s %s", indent,
			fmt.Sprintf("+%d", n.Offset), raw, label, n.Value)
		if n.Error != "" {
			line += "  !! " + n.Error
		}
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteByte('\n')
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	if d.Root != nil {
		for _, c := range d.Root.Children {
			walk(c, 0)
		}
	}
	if d.Error != "" {
		fmt.Fprintf(&b, "decode error: %s\n", d.Error)
	}
	return b.String()
}

// hex renders at most 16 bytes; long values are truncated so one record stays
// on one line. The full bytes remain available through the hex view.
func hex(b []byte) string {
	const max = 16
	var sb strings.Builder
	shown := b
	truncated := false
	if len(shown) > max {
		shown = shown[:max]
		truncated = true
	}
	for i, x := range shown {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02x", x)
	}
	if truncated {
		fmt.Fprintf(&sb, " …(%d)", len(b))
	}
	return sb.String()
}

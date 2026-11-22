package idl

import (
	"fmt"
	"strings"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokNumber
	tokString
	tokPunct
)

type token struct {
	kind tokenKind
	text string
	line int
	col  int
}

func (t token) String() string {
	if t.kind == tokEOF {
		return "<eof>"
	}
	if t.kind == tokString {
		return fmt.Sprintf("%q", t.text)
	}
	return t.text
}

type lexer struct {
	src  string
	pos  int
	line int
	col  int
	file string
}

func newLexer(src, file string) *lexer {
	return &lexer{src: src, line: 1, col: 1, file: file}
}

func (l *lexer) errf(format string, args ...any) error {
	return fmt.Errorf("%s:%d:%d: %s", l.file, l.line, l.col, fmt.Sprintf(format, args...))
}

func (l *lexer) advance(n int) {
	for i := 0; i < n && l.pos < len(l.src); i++ {
		if l.src[l.pos] == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
}

func (l *lexer) skipSpaceAndComments() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			l.advance(1)
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.advance(1)
			}
		case c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*':
			l.advance(2)
			for {
				if l.pos+1 >= len(l.src) {
					return l.errf("unterminated block comment")
				}
				if l.src[l.pos] == '*' && l.src[l.pos+1] == '/' {
					l.advance(2)
					break
				}
				l.advance(1)
			}
		default:
			return nil
		}
	}
	return nil
}

// isIdentStart and isIdentCont accept ASCII plus any non-ASCII byte. The
// latter lets interface descriptions use UTF-8 identifiers (for example the
// field names in a translated path template such as {book_id=...}); protobuf
// itself restricts identifiers to ASCII, but accepting Unicode here makes the
// gateway usable with descriptions written in other languages.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (l *lexer) next() (token, error) {
	if err := l.skipSpaceAndComments(); err != nil {
		return token{}, err
	}
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, line: l.line, col: l.col}, nil
	}
	line, col := l.line, l.col
	c := l.src[l.pos]
	switch {
	case isIdentStart(c):
		start := l.pos
		for l.pos < len(l.src) && isIdentCont(l.src[l.pos]) {
			l.advance(1)
		}
		return token{kind: tokIdent, text: l.src[start:l.pos], line: line, col: col}, nil
	case c >= '0' && c <= '9' || (c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] >= '0' && l.src[l.pos+1] <= '9'):
		start := l.pos
		l.advance(1)
		for l.pos < len(l.src) {
			d := l.src[l.pos]
			if (d >= '0' && d <= '9') || d == '.' || d == 'x' || d == 'X' ||
				(d >= 'a' && d <= 'f') || (d >= 'A' && d <= 'F') {
				l.advance(1)
				continue
			}
			break
		}
		return token{kind: tokNumber, text: l.src[start:l.pos], line: line, col: col}, nil
	case c == '"' || c == '\'':
		quote := c
		l.advance(1)
		var b strings.Builder
		for {
			if l.pos >= len(l.src) {
				return token{}, l.errf("unterminated string literal")
			}
			d := l.src[l.pos]
			if d == quote {
				l.advance(1)
				break
			}
			if d == '\\' && l.pos+1 < len(l.src) {
				l.advance(1)
				e := l.src[l.pos]
				switch e {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '\\':
					b.WriteByte('\\')
				case '\'':
					b.WriteByte('\'')
				case '"':
					b.WriteByte('"')
				default:
					b.WriteByte('\\')
					b.WriteByte(e)
				}
				l.advance(1)
				continue
			}
			b.WriteByte(d)
			l.advance(1)
		}
		return token{kind: tokString, text: b.String(), line: line, col: col}, nil
	default:
		// Multi-character punctuation used by proto syntax.
		if l.pos+1 < len(l.src) {
			two := l.src[l.pos : l.pos+2]
			switch two {
			case "//", "/*":
				// handled by skip; unreachable
			}
		}
		l.advance(1)
		return token{kind: tokPunct, text: string(c), line: line, col: col}, nil
	}
}

// parser is a recursive-descent parser for the supported proto subset.
type parser struct {
	lex    *lexer
	peeked *token
	file   *File
	reg    *Registry
}

func newParser(src, name, path string, reg *Registry) *parser {
	return &parser{
		lex:  newLexer(src, name),
		file: &File{Name: name, Path: path},
		reg:  reg,
	}
}

func (p *parser) errAt(t token, format string, args ...any) error {
	return fmt.Errorf("%s:%d:%d: %s", p.file.Name, t.line, t.col, fmt.Sprintf(format, args...))
}

func (p *parser) peek() (token, error) {
	if p.peeked == nil {
		t, err := p.lex.next()
		if err != nil {
			return token{}, err
		}
		p.peeked = &t
	}
	return *p.peeked, nil
}

func (p *parser) next() (token, error) {
	t, err := p.peek()
	if err != nil {
		return token{}, err
	}
	p.peeked = nil
	return t, nil
}

func (p *parser) expect(text string) (token, error) {
	t, err := p.next()
	if err != nil {
		return token{}, err
	}
	if t.text != text {
		return token{}, p.errAt(t, "expected %q, got %s", text, t)
	}
	return t, nil
}

func (p *parser) expectIdent() (token, error) {
	t, err := p.next()
	if err != nil {
		return token{}, err
	}
	if t.kind != tokIdent {
		return token{}, p.errAt(t, "expected identifier, got %s", t)
	}
	return t, nil
}

// expectFullIdent consumes a possibly dotted identifier.
func (p *parser) expectFullIdent(allowLeadingDot bool) (string, error) {
	var b strings.Builder
	t, err := p.peek()
	if err != nil {
		return "", err
	}
	if allowLeadingDot && t.text == "." {
		p.next()
		b.WriteByte('.')
	}
	t, err = p.expectIdent()
	if err != nil {
		return "", err
	}
	b.WriteString(t.text)
	for {
		t, err := p.peek()
		if err != nil {
			return "", err
		}
		if t.text != "." {
			break
		}
		p.next()
		id, err := p.expectIdent()
		if err != nil {
			return "", err
		}
		b.WriteByte('.')
		b.WriteString(id.text)
	}
	return b.String(), nil
}

// ParseFile parses a single .proto source file into the registry. Type
// references are resolved by Registry.Link.
func ParseFile(src, name, path string, reg *Registry) (*File, error) {
	p := newParser(src, name, path, reg)
	if err := p.parseFile(); err != nil {
		return nil, err
	}
	reg.Files = append(reg.Files, p.file)
	return p.file, nil
}

func (p *parser) parseFile() error {
	syntax := "proto2"
	for {
		t, err := p.peek()
		if err != nil {
			return err
		}
		if t.kind == tokEOF {
			break
		}
		switch t.text {
		case "syntax":
			p.next()
			if _, err := p.expect("="); err != nil {
				return err
			}
			s, err := p.next()
			if err != nil {
				return err
			}
			if s.kind != tokString {
				return p.errAt(s, "syntax must be a string")
			}
			syntax = s.text
			if syntax != "proto2" && syntax != "proto3" {
				return p.errAt(s, "unsupported syntax %q", syntax)
			}
			if _, err := p.expect(";"); err != nil {
				return err
			}
			p.file.Syntax = syntax
		case "package":
			p.next()
			name, err := p.expectFullIdent(false)
			if err != nil {
				return err
			}
			p.file.Package = name
			if _, err := p.expect(";"); err != nil {
				return err
			}
		case "import":
			p.next()
			kind, err := p.peek()
			if err != nil {
				return err
			}
			if kind.text == "public" || kind.text == "weak" {
				p.next()
			}
			s, err := p.next()
			if err != nil {
				return err
			}
			if s.kind != tokString {
				return p.errAt(s, "import path must be a string")
			}
			p.file.Imports = append(p.file.Imports, s.text)
			if _, err := p.expect(";"); err != nil {
				return err
			}
		case "option":
			if err := p.skipStatement(); err != nil {
				return err
			}
		case "message":
			if err := p.parseMessage(p.file.Package, ""); err != nil {
				return err
			}
		case "enum":
			if err := p.parseEnum(p.file.Package, ""); err != nil {
				return err
			}
		case "service":
			if err := p.parseService(); err != nil {
				return err
			}
		case ";":
			p.next()
		default:
			return p.errAt(t, "unexpected %s at top level", t)
		}
	}
	return nil
}

// skipStatement consumes tokens until a semicolon at brace depth zero.
func (p *parser) skipStatement() error {
	depth := 0
	for {
		t, err := p.next()
		if err != nil {
			return err
		}
		if t.kind == tokEOF {
			return p.errAt(t, "unexpected end of file in statement")
		}
		switch t.text {
		case "{", "[":
			depth++
		case "}", "]":
			depth--
		case ";":
			if depth <= 0 {
				return nil
			}
		}
	}
}

func qualify(pkg, scope, name string) string {
	var parts []string
	if pkg != "" {
		parts = append(parts, pkg)
	}
	if scope != "" {
		parts = append(parts, scope)
	}
	parts = append(parts, name)
	return strings.Join(parts, ".")
}

func (p *parser) parseMessage(pkg, scope string) error {
	if _, err := p.expect("message"); err != nil {
		return err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return err
	}
	msg := &Message{
		Name:     nameTok.text,
		FullName: qualify(pkg, scope, nameTok.text),
		File:     p.file,
	}
	p.file.Messages = append(p.file.Messages, msg)
	p.reg.Messages[msg.FullName] = msg
	innerScope := msg.FullName
	if i := strings.LastIndexByte(msg.FullName, '.'); i >= 0 {
		innerScope = msg.FullName[i+1:]
	}
	// innerScope relative to package; use full minus package prefix for
	// nested declarations.
	scopeRel := msg.FullName
	if pkg != "" {
		scopeRel = strings.TrimPrefix(msg.FullName, pkg+".")
	}

	if _, err := p.expect("{"); err != nil {
		return err
	}
	_ = innerScope
	oneofIdx := 0
	for {
		t, err := p.peek()
		if err != nil {
			return err
		}
		if t.text == "}" {
			p.next()
			break
		}
		if t.kind == tokEOF {
			return p.errAt(t, "unexpected end of file in message %s", msg.Name)
		}
		switch t.text {
		case ";":
			p.next()
		case "message":
			if err := p.parseMessage(pkg, scopeRel); err != nil {
				return err
			}
		case "enum":
			if err := p.parseEnum(pkg, scopeRel); err != nil {
				return err
			}
		case "oneof":
			p.next()
			nameTok, err := p.expectIdent()
			if err != nil {
				return err
			}
			o := &Oneof{Name: nameTok.text, Index: oneofIdx}
			oneofIdx++
			if _, err := p.expect("{"); err != nil {
				return err
			}
			for {
				t, err := p.peek()
				if err != nil {
					return err
				}
				if t.text == "}" {
					p.next()
					break
				}
				if t.kind == tokEOF {
					return p.errAt(t, "unexpected end of file in oneof %s", o.Name)
				}
				if t.text == "option" {
					if err := p.skipStatement(); err != nil {
						return err
					}
					continue
				}
				f, err := p.parseField(msg, pkg, scopeRel)
				if err != nil {
					return err
				}
				if f != nil {
					f.Oneof = o
					o.Fields = append(o.Fields, f)
				}
			}
			msg.Oneofs = append(msg.Oneofs, o)
		case "option", "reserved", "extensions", "extend":
			if err := p.skipStatement(); err != nil {
				return err
			}
		default:
			f, err := p.parseField(msg, pkg, scopeRel)
			if err != nil {
				return err
			}
			_ = f
		}
	}
	return nil
}

func (p *parser) parseField(msg *Message, pkg, scope string) (*Field, error) {
	start, err := p.peek()
	if err != nil {
		return nil, err
	}
	label := ""
	if start.kind == tokIdent && (start.text == "repeated" || start.text == "optional" || start.text == "required") {
		label = start.text
		p.next()
	}

	field := &Field{Index: len(msg.Fields)}
	if label == "optional" {
		field.Optional = true
	}
	if label == "repeated" {
		field.Repeated = true
	}

	t, err := p.peek()
	if err != nil {
		return nil, err
	}
	if t.text == "map" {
		p.next()
		if _, err := p.expect("<"); err != nil {
			return nil, err
		}
		kt, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		kst, ok := ScalarTypeByName(kt.text)
		if !ok {
			return nil, p.errAt(kt, "invalid map key type %q", kt.text)
		}
		field.Map = true
		field.MapKey = kst
		field.Repeated = true
		if _, err := p.expect(","); err != nil {
			return nil, err
		}
		vt, err := p.expectFullIdent(true)
		if err != nil {
			return nil, err
		}
		if st, ok := ScalarTypeByName(vt); ok {
			field.MapValue = Type{Scalar: st}
		} else {
			field.MapValueName = vt
		}
		if _, err := p.expect(">"); err != nil {
			return nil, err
		}
	} else {
		typeName, err := p.expectFullIdent(true)
		if err != nil {
			return nil, err
		}
		if st, ok := ScalarTypeByName(typeName); ok {
			field.Type = Type{Scalar: st}
		} else {
			field.TypeName = typeName
		}
	}

	nameTok, err := p.expectIdent()
	if err != nil {
		return nil, err
	}
	field.Name = nameTok.text
	field.JSONName = JSONName(nameTok.text)
	if _, err := p.expect("="); err != nil {
		return nil, err
	}
	numTok, err := p.next()
	if err != nil {
		return nil, err
	}
	if numTok.kind != tokNumber {
		return nil, p.errAt(numTok, "expected field number, got %s", numTok)
	}
	var num int64
	if _, err := fmt.Sscanf(numTok.text, "%d", &num); err != nil {
		return nil, p.errAt(numTok, "invalid field number %q", numTok.text)
	}
	field.Number = int32(num)

	// Options.
	t, err = p.peek()
	if err != nil {
		return nil, err
	}
	if t.text == "[" {
		p.next()
		for {
			if err := p.parseFieldOption(field); err != nil {
				return nil, err
			}
			t, err := p.peek()
			if err != nil {
				return nil, err
			}
			if t.text == "," {
				p.next()
				continue
			}
			if t.text == "]" {
				p.next()
				break
			}
			return nil, p.errAt(t, "expected , or ] in field options, got %s", t)
		}
	}
	if _, err := p.expect(";"); err != nil {
		return nil, err
	}
	msg.Fields = append(msg.Fields, field)
	return field, nil
}

func (p *parser) parseFieldOption(f *Field) error {
	name, err := p.expectFullIdent(false)
	if err != nil {
		return err
	}
	if _, err := p.expect("="); err != nil {
		return err
	}
	val, err := p.next()
	if err != nil {
		return err
	}
	switch name {
	case "packed":
		b := val.text == "true"
		f.Packed = &b
	case "json_name":
		if val.kind != tokString {
			return p.errAt(val, "json_name must be a string")
		}
		f.JSONName = val.text
	case "deprecated":
		f.Deprecated = val.text == "true"
	default:
		// Unknown option: consume the value which was already read.
		if val.text == "{" {
			depth := 1
			for depth > 0 {
				t, err := p.next()
				if err != nil {
					return err
				}
				if t.kind == tokEOF {
					return p.errAt(t, "unexpected eof in option value")
				}
				if t.text == "{" {
					depth++
				} else if t.text == "}" {
					depth--
				}
			}
		}
	}
	return nil
}

func (p *parser) parseEnum(pkg, scope string) error {
	if _, err := p.expect("enum"); err != nil {
		return err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return err
	}
	e := &Enum{
		Name:     nameTok.text,
		FullName: qualify(pkg, scope, nameTok.text),
		File:     p.file,
	}
	p.file.Enums = append(p.file.Enums, e)
	p.reg.Enums[e.FullName] = e
	if _, err := p.expect("{"); err != nil {
		return err
	}
	for {
		t, err := p.peek()
		if err != nil {
			return err
		}
		if t.text == "}" {
			p.next()
			break
		}
		if t.kind == tokEOF {
			return p.errAt(t, "unexpected end of file in enum %s", e.Name)
		}
		switch t.text {
		case "option":
			p.next()
			// Look for allow_alias.
			if err := p.skipOptionAfterKeyword(e); err != nil {
				return err
			}
		case ";":
			p.next()
		case "reserved":
			if err := p.skipStatement(); err != nil {
				return err
			}
		default:
			vt, err := p.expectIdent()
			if err != nil {
				return err
			}
			if _, err := p.expect("="); err != nil {
				return err
			}
			numTok, err := p.next()
			if err != nil {
				return err
			}
			if numTok.kind != tokNumber {
				return p.errAt(numTok, "expected enum number, got %s", numTok)
			}
			var n int64
			if _, err := fmt.Sscanf(numTok.text, "%d", &n); err != nil {
				return p.errAt(numTok, "invalid enum number %q", numTok.text)
			}
			// Optional [deprecated = true] etc.
			nt, err := p.peek()
			if err != nil {
				return err
			}
			if nt.text == "[" {
				if err := p.skipBracketList(); err != nil {
					return err
				}
			}
			if _, err := p.expect(";"); err != nil {
				return err
			}
			e.Values = append(e.Values, &EnumValue{Name: vt.text, Number: int32(n)})
		}
	}
	e.index()
	return nil
}

func (p *parser) skipOptionAfterKeyword(e *Enum) error {
	name, err := p.expectFullIdent(false)
	if err != nil {
		return err
	}
	if _, err := p.expect("="); err != nil {
		return err
	}
	val, err := p.next()
	if err != nil {
		return err
	}
	if name == "allow_alias" && val.text == "true" {
		e.AllowAlias = true
	}
	if val.text == "{" {
		depth := 1
		for depth > 0 {
			t, err := p.next()
			if err != nil {
				return err
			}
			if t.kind == tokEOF {
				return p.errAt(t, "unexpected eof in option")
			}
			if t.text == "{" {
				depth++
			} else if t.text == "}" {
				depth--
			}
		}
	}
	if _, err := p.expect(";"); err != nil {
		return err
	}
	return nil
}

func (p *parser) skipBracketList() error {
	if _, err := p.expect("["); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t, err := p.next()
		if err != nil {
			return err
		}
		if t.kind == tokEOF {
			return p.errAt(t, "unexpected eof in list")
		}
		switch t.text {
		case "[":
			depth++
		case "]":
			depth--
		}
	}
	return nil
}

func (p *parser) parseService() error {
	if _, err := p.expect("service"); err != nil {
		return err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return err
	}
	svc := &Service{
		Name:     nameTok.text,
		FullName: qualify(p.file.Package, "", nameTok.text),
		File:     p.file,
	}
	p.file.Services = append(p.file.Services, svc)
	p.reg.Services = append(p.reg.Services, svc)
	if _, err := p.expect("{"); err != nil {
		return err
	}
	for {
		t, err := p.peek()
		if err != nil {
			return err
		}
		if t.text == "}" {
			p.next()
			break
		}
		if t.kind == tokEOF {
			return p.errAt(t, "unexpected end of file in service %s", svc.Name)
		}
		switch t.text {
		case ";":
			p.next()
		case "option":
			if err := p.skipStatement(); err != nil {
				return err
			}
		case "rpc":
			if err := p.parseRPC(svc); err != nil {
				return err
			}
		default:
			return p.errAt(t, "unexpected %s in service", t)
		}
	}
	return nil
}

func (p *parser) parseRPC(svc *Service) error {
	if _, err := p.expect("rpc"); err != nil {
		return err
	}
	nameTok, err := p.expectIdent()
	if err != nil {
		return err
	}
	m := &Method{
		Name:     nameTok.text,
		FullName: svc.FullName + "." + nameTok.text,
		Service:  svc,
		Order:    len(svc.Methods),
	}
	if _, err := p.expect("("); err != nil {
		return err
	}
	if t, err := p.peek(); err != nil {
		return err
	} else if t.text == "stream" {
		p.next()
		m.ClientStreaming = true
	}
	in, err := p.expectFullIdent(true)
	if err != nil {
		return err
	}
	m.InputTypeName = in
	if _, err := p.expect(")"); err != nil {
		return err
	}
	if _, err := p.expect("returns"); err != nil {
		return err
	}
	if _, err := p.expect("("); err != nil {
		return err
	}
	if t, err := p.peek(); err != nil {
		return err
	} else if t.text == "stream" {
		p.next()
		m.ServerStreaming = true
	}
	out, err := p.expectFullIdent(true)
	if err != nil {
		return err
	}
	m.OutputTypeName = out
	if _, err := p.expect(")"); err != nil {
		return err
	}

	t, err := p.peek()
	if err != nil {
		return err
	}
	if t.text == "{" {
		p.next()
		for {
			t, err := p.peek()
			if err != nil {
				return err
			}
			if t.text == "}" {
				p.next()
				break
			}
			if t.kind == tokEOF {
				return p.errAt(t, "unexpected end of file in rpc %s", m.Name)
			}
			if t.text != "option" {
				return p.errAt(t, "expected option in rpc body, got %s", t)
			}
			p.next()
			switch {
			case p.leadingParenOption():
				rule, err := p.parseHTTPOption()
				if err != nil {
					return err
				}
				if rule != nil {
					m.HTTP = append(m.HTTP, rule)
				}
			default:
				if err := p.skipStatement(); err != nil {
					return err
				}
			}
		}
	} else if t.text == ";" {
		p.next()
	} else {
		return p.errAt(t, "expected { or ; after rpc signature, got %s", t)
	}
	svc.Methods = append(svc.Methods, m)
	return nil
}

// leadingParenOption reports whether the upcoming token is "(".
func (p *parser) leadingParenOption() bool {
	t, err := p.peek()
	return err == nil && t.text == "("
}

// parseHTTPOption parses:
//
//	(google.api.http) = { get: "/path" body: "*" additional_bindings { ... } };
//
// The parser is tolerant of the annotation's exact spelling and accepts the
// short forms get/post/put/patch/delete/custom.
func (p *parser) parseHTTPOption() (*HTTPRule, error) {
	if _, err := p.expect("("); err != nil {
		return nil, err
	}
	name, err := p.expectFullIdent(false)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(")"); err != nil {
		return nil, err
	}
	if _, err := p.expect("="); err != nil {
		return nil, err
	}
	if name != "google.api.http" {
		// Unknown method option: skip the value and the semicolon.
		if err := p.skipLooseValueAndSemi(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	t, err := p.peek()
	if err != nil {
		return nil, err
	}
	if t.text != "{" {
		return nil, p.errAt(t, "expected { for google.api.http rule, got %s", t)
	}
	rule, err := p.parseHTTPRuleBody()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(";"); err != nil {
		return nil, err
	}
	return rule, nil
}

func (p *parser) parseHTTPRuleBody() (*HTTPRule, error) {
	if _, err := p.expect("{"); err != nil {
		return nil, err
	}
	rule := &HTTPRule{}
	for {
		t, err := p.peek()
		if err != nil {
			return nil, err
		}
		if t.text == "}" {
			p.next()
			break
		}
		if t.kind == tokEOF {
			return nil, p.errAt(t, "unexpected eof in http rule")
		}
		if t.text == "," {
			p.next()
			continue
		}
		key, err := p.expectIdent()
		if err != nil {
			return nil, err
		}
		switch key.text {
		case "get", "post", "put", "patch", "delete", "head", "options":
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			val, err := p.next()
			if err != nil {
				return nil, err
			}
			if val.kind != tokString {
				return nil, p.errAt(val, "http method path must be a string")
			}
			rule.Method = strings.ToUpper(key.text)
			rule.Path = val.text
		case "body":
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			val, err := p.next()
			if err != nil {
				return nil, err
			}
			if val.kind != tokString {
				return nil, p.errAt(val, "body must be a string")
			}
			rule.Body = val.text
		case "response_body":
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			val, err := p.next()
			if err != nil {
				return nil, err
			}
			if val.kind != tokString {
				return nil, p.errAt(val, "response_body must be a string")
			}
			rule.ResponseBody = val.text
		case "additional_bindings":
			sub, err := p.parseHTTPRuleBody()
			if err != nil {
				return nil, err
			}
			rule.Additional = append(rule.Additional, sub)
		case "custom":
			// custom { kind: "..." path: "..." } is accepted; kind is
			// treated as the HTTP method.
			sub, err := p.parseHTTPRuleBody()
			if err != nil {
				return nil, err
			}
			if sub.Method == "" {
				sub.Method = "CUSTOM"
			}
			*rule = *sub
		case "selector":
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			if _, err := p.next(); err != nil {
				return nil, err
			}
		default:
			// Unknown key with a scalar or nested value.
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			if err := p.skipLooseValue(); err != nil {
				return nil, err
			}
		}
	}
	return rule, nil
}

func (p *parser) skipLooseValue() error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.text == "{" {
		depth := 1
		for depth > 0 {
			t, err := p.next()
			if err != nil {
				return err
			}
			if t.kind == tokEOF {
				return p.errAt(t, "unexpected eof in value")
			}
			if t.text == "{" {
				depth++
			} else if t.text == "}" {
				depth--
			}
		}
	}
	return nil
}

func (p *parser) skipLooseValueAndSemi() error {
	if err := p.skipLooseValue(); err != nil {
		return err
	}
	if t, err := p.peek(); err != nil {
		return err
	} else if t.text == ";" {
		p.next()
	}
	return nil
}

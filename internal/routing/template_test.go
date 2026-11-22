package routing

import (
	"strings"
	"testing"
)

func mustCompile(t *testing.T, method, tmpl string) *Route {
	t.Helper()
	r, err := Compile(method, tmpl, "test.Handler")
	if err != nil {
		t.Fatalf("Compile(%q, %q): %v", method, tmpl, err)
	}
	return r
}

func TestMatchSimple(t *testing.T) {
	r := mustCompile(t, "GET", "/v1/shelves/{shelf}/books/{book}")
	m, ok := r.Match("GET", "/v1/shelves/scifi/books/dune")
	if !ok {
		t.Fatal("no match")
	}
	if m.Vars["shelf"] != "scifi" || m.Vars["book"] != "dune" {
		t.Fatalf("vars = %v", m.Vars)
	}
	if len(m.PathParams) != 2 {
		t.Fatalf("path params = %v", m.PathParams)
	}
}

func TestMatchMultiSegmentVariable(t *testing.T) {
	// The example from the task: a variable whose pattern spans several
	// segments.
	r := mustCompile(t, "GET", "/v1/{book=shelves/*/books/*}")
	m, ok := r.Match("GET", "/v1/shelves/fiction/books/dune")
	if !ok {
		t.Fatal("no match")
	}
	if got := m.Vars["book"]; got != "fiction/dune" {
		t.Fatalf("book = %q", got)
	}
	if got := m.Segments["book"]; len(got) != 2 || got[0] != "fiction" || got[1] != "dune" {
		t.Fatalf("segments = %v", got)
	}
	if _, ok := r.Match("GET", "/v1/shelves/fiction/dvds/dune"); ok {
		t.Fatal("pattern must not match a different literal")
	}
	if _, ok := r.Match("GET", "/v1/shelves/fiction/books/dune/extra"); ok {
		t.Fatal("extra trailing segment must not match")
	}
}

func TestMatchMultiSegmentInMiddle(t *testing.T) {
	r := mustCompile(t, "GET", "/v1/{name=shelves/*/books/*}/chapters/{chapter}")
	m, ok := r.Match("GET", "/v1/shelves/a/books/b/chapters/c1")
	if !ok {
		t.Fatal("no match")
	}
	if m.Vars["name"] != "a/b" || m.Vars["chapter"] != "c1" {
		t.Fatalf("vars = %v", m.Vars)
	}
}

func TestMatchCatchAll(t *testing.T) {
	r := mustCompile(t, "GET", "/v1/files/{path=**}:download")
	for _, tc := range []struct{ path, want string }{
		{"/v1/files/a:download", "a"},
		{"/v1/files/a/b/c.txt:download", "a/b/c.txt"},
	} {
		m, ok := r.Match("GET", tc.path)
		if !ok {
			t.Fatalf("%s: no match", tc.path)
		}
		if m.Vars["path"] != tc.want {
			t.Fatalf("%s: path = %q want %q", tc.path, m.Vars["path"], tc.want)
		}
		if m.Verb != "download" {
			t.Fatalf("%s: verb = %q", tc.path, m.Verb)
		}
	}
	if _, ok := r.Match("GET", "/v1/files/a"); ok {
		t.Fatal("missing verb must not match")
	}
	if _, ok := r.Match("GET", "/v1/files/a:view"); ok {
		t.Fatal("wrong verb must not match")
	}
}

func TestCatchAllInMiddle(t *testing.T) {
	r := mustCompile(t, "GET", "/v1/{prefix=**}/tail/{id}")
	m, ok := r.Match("GET", "/v1/a/b/tail/x")
	if !ok {
		t.Fatal("no match")
	}
	if m.Vars["prefix"] != "a/b" || m.Vars["id"] != "x" {
		t.Fatalf("vars = %v", m.Vars)
	}
}

func TestVerbs(t *testing.T) {
	withVerb := mustCompile(t, "GET", "/v1/shelves/{shelf}/books:watch")
	m, ok := withVerb.Match("GET", "/v1/shelves/s/books:watch")
	if !ok || m.Verb != "watch" {
		t.Fatalf("ok=%v match=%+v", ok, m)
	}
	if _, ok := withVerb.Match("GET", "/v1/shelves/s/books"); ok {
		t.Fatal("verb-less path must not match a verb route")
	}
	noVerb := mustCompile(t, "GET", "/v1/shelves/{shelf}/books")
	if _, ok := noVerb.Match("GET", "/v1/shelves/s/books:watch"); ok {
		t.Fatal("verb path must not match a verb-less route")
	}
}

func TestMethodIsCaseInsensitive(t *testing.T) {
	r := mustCompile(t, "POST", "/v1/x")
	if _, ok := r.Match("post", "/v1/x"); !ok {
		t.Fatal("lower-case method should match")
	}
	if _, ok := r.Match("GET", "/v1/x"); ok {
		t.Fatal("different method must not match")
	}
}

func TestPrecedenceLiteralBeatsWildcard(t *testing.T) {
	table := NewTable()
	if _, err := table.Add("GET", "/v1/{name=**}", "catchall"); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Add("GET", "/v1/search", "literal"); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Add("GET", "/v1/{q}", "one"); err != nil {
		t.Fatal(err)
	}
	m, ok := table.Lookup("GET", "/v1/search")
	if !ok || m.Route.Handler != "literal" {
		t.Fatalf("matched %v", m)
	}
	m, _ = table.Lookup("GET", "/v1/other")
	if m.Route.Handler != "one" {
		t.Fatalf("wildcard precedence: matched %s", m.Route.Handler)
	}
	m, _ = table.Lookup("GET", "/v1/other/deeper")
	if m.Route.Handler != "catchall" {
		t.Fatalf("catch-all precedence: matched %s", m.Route.Handler)
	}
}

func TestPrecedencePatternBeatsPlainWildcard(t *testing.T) {
	table := NewTable()
	if _, err := table.Add("GET", "/v1/{name}", "plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Add("GET", "/v1/{name=shelves/*/books/*}", "pattern"); err != nil {
		t.Fatal(err)
	}
	m, ok := table.Lookup("GET", "/v1/shelves/a/books/b")
	if !ok || m.Route.Handler != "pattern" {
		t.Fatalf("expected pattern route, got %v", m)
	}
}

func TestPrecedenceDeclarationOrderBreaksTies(t *testing.T) {
	table := NewTable()
	table.Add("GET", "/v1/{a}/x", "first")
	table.Add("GET", "/v1/{b}/x", "second")
	m, _ := table.Lookup("GET", "/v1/z/x")
	if m.Route.Handler != "first" {
		t.Fatalf("tie should favour declaration order, got %s", m.Route.Handler)
	}
}

func TestPercentDecoding(t *testing.T) {
	r := mustCompile(t, "GET", "/v1/shelves/{shelf}")
	m, ok := r.Match("GET", "/v1/shelves/sci%20fi")
	if !ok {
		t.Fatal("no match")
	}
	if m.Vars["shelf"] != "sci fi" {
		t.Fatalf("decoded = %q", m.Vars["shelf"])
	}
	// Plus signs stay literal in a path segment.
	m, _ = r.Match("GET", "/v1/shelves/a+b")
	if m.Vars["shelf"] != "a+b" {
		t.Fatalf("plus = %q", m.Vars["shelf"])
	}
}

func TestMalformedTemplates(t *testing.T) {
	for _, tmpl := range []string{
		"v1/x",     // no leading slash
		"/v1/{",    // unclosed brace
		"/v1/x}",   // stray brace
		"/v1/{=*}", // empty variable name
		"/v1/{}",   // empty variable
		"/v1//x",   // empty segment
		"/v1/{a=b", // unclosed pattern
	} {
		if _, err := Compile("GET", tmpl, "h"); err == nil {
			t.Errorf("Compile(%q) should fail", tmpl)
		}
	}
}

func TestSplitTemplateKeepsPatternTogether(t *testing.T) {
	segs, verb, err := Parse("/v1/{name=shelves/*/books/*}")
	if err != nil {
		t.Fatal(err)
	}
	if verb != "" || len(segs) != 2 {
		t.Fatalf("segs=%v verb=%q", segs, verb)
	}
}

func TestVariablesReport(t *testing.T) {
	segs, _, err := Parse("/v1/{name=shelves/*/books/*}/{rest=**}/{id}")
	if err != nil {
		t.Fatal(err)
	}
	vars := Variables(segs)
	if len(vars) != 3 {
		t.Fatalf("vars = %+v", vars)
	}
	if vars[0].Name != "name" || vars[0].Segments != 4 {
		t.Fatalf("name: %+v", vars[0])
	}
	if vars[1].Name != "rest" || !vars[1].CatchAll || vars[1].Segments != -1 {
		t.Fatalf("rest: %+v", vars[1])
	}
	if vars[2].Name != "id" || vars[2].Segments != 1 {
		t.Fatalf("id: %+v", vars[2])
	}
}

func TestQueryKeyParsing(t *testing.T) {
	type qcomp = QueryComponent
	cases := []struct {
		key  string
		want []qcomp
	}{
		{"a", []qcomp{{Name: "a"}}},
		{"a.b.c", []qcomp{{Name: "a"}, {Name: "b"}, {Name: "c"}}},
		{"a[b][c]", []qcomp{{Name: "a"}, {Name: "b"}, {Name: "c"}}},
		{"items[]", []qcomp{{Name: "items"}, {AppendElem: true}}},
		{"items[2].name", []qcomp{{Name: "items"}, {Name: "2", Index: 2, IsIndex: true}, {Name: "name"}}},
		{"labels[env]", []qcomp{{Name: "labels"}, {Name: "env"}}},
		{"a[b].c[d]", []qcomp{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}},
	}
	for _, tc := range cases {
		got, err := ParseQueryKey(tc.key)
		if err != nil {
			t.Fatalf("%q: %v", tc.key, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %v want %v", tc.key, got, tc.want)
		}
		for i := range got {
			if got[i].Name != tc.want[i].Name || got[i].IsIndex != tc.want[i].IsIndex ||
				got[i].Index != tc.want[i].Index || got[i].AppendElem != tc.want[i].AppendElem {
				t.Fatalf("%q component %d: got %+v want %+v", tc.key, i, got[i], tc.want[i])
			}
		}
	}
	for _, bad := range []string{"", ".a", "a.", "a[b", "a]b"} {
		if _, err := ParseQueryKey(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestExamplePath(t *testing.T) {
	for _, tmpl := range []string{
		"/v1/{name=shelves/*/books/*}",
		"/v1/files/{path=**}:download",
		"/v1/{parent=shelves/*}/books",
	} {
		ex := ExamplePath(tmpl)
		r := mustCompile(t, "GET", tmpl)
		if _, ok := r.Match("GET", ex); !ok {
			t.Errorf("ExamplePath(%q) = %q does not match its own template", tmpl, ex)
		}
	}
}

func TestRouteScoreStrings(t *testing.T) {
	// Guard against an accidental ordering regression: a deeper, fully
	// literal template must outrank a shallow catch-all.
	a := mustCompile(t, "GET", "/v1/books/{id}")
	b := mustCompile(t, "GET", "/v1/{path=**}")
	if !Less(a, b) {
		t.Fatal("specific route should sort first")
	}
	if Less(b, a) {
		t.Fatal("ordering is not antisymmetric")
	}
	if !strings.Contains(a.Template, "{id}") {
		t.Fatal("sanity")
	}
}

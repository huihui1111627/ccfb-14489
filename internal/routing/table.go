package routing

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Entry is a compiled route plus the application payload it dispatches to.
type Entry struct {
	Route *Route
	// Method is the fully-qualified RPC method name.
	Method string
	// Data is free-form application data (the gateway stores its own
	// binding record here).
	Data any
}

// Table is an ordered set of routes with deterministic precedence.
type Table struct {
	entries  []*Entry
	byMethod map[string][]*Entry
}

// NewTable creates an empty routing table.
func NewTable() *Table {
	return &Table{byMethod: map[string][]*Entry{}}
}

// Add compiles and inserts a route. Declaration order is preserved and used
// as the final tie-breaker.
func (t *Table) Add(method, template, handler string) (*Entry, error) {
	r, err := Compile(method, template, handler)
	if err != nil {
		return nil, err
	}
	r.order = len(t.entries)
	e := &Entry{Route: r, Method: handler}
	t.entries = append(t.entries, e)
	t.byMethod[r.Method] = append(t.byMethod[r.Method], e)
	return e, nil
}

// Entries returns all routes in precedence order.
func (t *Table) Entries() []*Entry {
	out := append([]*Entry(nil), t.entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Route.Method != out[j].Route.Method {
			return out[i].Route.Method < out[j].Route.Method
		}
		return Less(out[i].Route, out[j].Route)
	})
	return out
}

// Lookup finds the most specific route for method+path. Candidates that match
// with different specificity are ranked; declaration order breaks ties.
func (t *Table) Lookup(method, path string) (*Match, bool) {
	cands := t.byMethod[strings.ToUpper(method)]
	var best *Match
	for _, e := range cands {
		m, ok := e.Route.Match(method, path)
		if !ok {
			continue
		}
		if best == nil || Less(m.Route, best.Route) {
			best = m
		}
	}
	return best, best != nil
}

// Candidates returns every route whose method and literal prefix could match,
// most specific first. Used by the inspector to explain near-misses.
func (t *Table) Candidates(method, path string) []*Entry {
	cands := t.byMethod[strings.ToUpper(method)]
	out := append([]*Entry(nil), cands...)
	sort.SliceStable(out, func(i, j int) bool { return Less(out[i].Route, out[j].Route) })
	return out
}

// ParseQuery parses a raw query string into ordered key/value pairs. Repeated
// keys are preserved. Both "a.b" and "a[b]" forms are accepted; the gateway
// decides which path syntax a key uses.
func ParseQuery(rawQuery string) ([]Param, error) {
	if rawQuery == "" {
		return nil, nil
	}
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Param
	for _, k := range keys {
		for _, v := range vals[k] {
			out = append(out, Param{Key: k, Value: v})
		}
	}
	return out, nil
}

// Param is one query parameter occurrence.
type Param struct {
	Key   string
	Value string
}

// DecodePathValue percent-decodes one path segment. Unlike url.QueryUnescape,
// '+' is kept as a literal plus, which is the correct interpretation for
// path segments.
func DecodePathValue(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	return url.PathUnescape(s)
}

// SplitPath splits an escaped request path into decoded segments.
func SplitPath(p string) ([]string, error) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil, nil
	}
	raw := strings.Split(p, "/")
	out := make([]string, len(raw))
	for i, s := range raw {
		d, err := DecodePathValue(s)
		if err != nil {
			return nil, fmt.Errorf("invalid escape in path segment %q: %w", s, err)
		}
		out[i] = d
	}
	return out, nil
}

// QueryComponent is one component of a query parameter key.
type QueryComponent struct {
	Name string
	// Index is set when the component is a numeric subscript on a repeated
	// message field, e.g. "items[2].name".
	Index   int
	IsIndex bool
	// AppendElem marks a bare "[]" suffix asking to append a new element.
	AppendElem bool
}

// ParseQueryKey splits a query key into components. Both dotted and
// bracketed forms are accepted, and they may be mixed:
//
//	a.b.c        nested message fields
//	a[b][c]      nested message fields
//	items[]      repeated field, one element per occurrence
//	items[2]     indexed repeated element
//	labels[env]  map entry
//	a[b].c[d]    mixed
func ParseQueryKey(key string) ([]QueryComponent, error) {
	if key == "" {
		return nil, fmt.Errorf("empty query parameter name")
	}
	var comps []QueryComponent
	i := 0
	readName := func() (string, error) {
		start := i
		for i < len(key) && key[i] != '.' && key[i] != '[' && key[i] != ']' {
			i++
		}
		if start == i {
			return "", fmt.Errorf("empty component in query key %q", key)
		}
		return key[start:i], nil
	}
	name, err := readName()
	if err != nil {
		return nil, err
	}
	comps = append(comps, QueryComponent{Name: name})
	for i < len(key) {
		switch key[i] {
		case '.':
			i++
			name, err := readName()
			if err != nil {
				return nil, err
			}
			comps = append(comps, QueryComponent{Name: name})
		case '[':
			j := strings.IndexByte(key[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("unclosed '[' in query key %q", key)
			}
			inner := key[i+1 : i+j]
			i += j + 1
			if inner == "" {
				comps = append(comps, QueryComponent{AppendElem: true})
				continue
			}
			if n, err := strconv.Atoi(inner); err == nil {
				comps = append(comps, QueryComponent{Name: inner, Index: n, IsIndex: true})
			} else {
				comps = append(comps, QueryComponent{Name: inner})
			}
		case ']':
			return nil, fmt.Errorf("unbalanced ']' in query key %q", key)
		default:
			return nil, fmt.Errorf("malformed query key %q", key)
		}
	}
	return comps, nil
}

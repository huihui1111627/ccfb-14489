// Package routing implements google.api.http path templates: parsing, matching
// and deterministic precedence ordering.
//
// A template is a sequence of segments separated by '/'. A segment is one of:
//
//	literal                 e.g. v1, shelves
//	{variable}              e.g. {shelf}
//	{variable=pattern}      e.g. {shelf_id=shelves/*}
//
// Variables may expand to several segments ("multi-segment"), either through
// a wildcard pattern or a trailing "**" which matches zero or more segments.
// Both forms may appear in the middle of a template:
//
//	v1/books/{name=shelves/*/books/*}/chapters/{chapter}
//	files/{path=**}:download
//
// A verb suffix (":action") is allowed on the last segment.
package routing

import (
	"fmt"
	"sort"
	"strings"
)

// ErrNoMatch is returned when no route matches a request.
type ErrNoMatch struct {
	Method string
	Path   string
	Reason string
}

func (e *ErrNoMatch) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("no route for %s %s: %s", e.Method, e.Path, e.Reason)
	}
	return fmt.Sprintf("no route for %s %s", e.Method, e.Path)
}

// VarSegment is one captured segment of a variable expansion.
type VarSegment struct {
	Field string // field path inside the request message
	Value string // decoded path segment
}

// Match is the result of a successful route match.
type Match struct {
	Route *Route
	// Vars is keyed by field path (e.g. "book.name"). For multi-segment
	// variables it holds the segments joined by "/".
	Vars map[string]string
	// Segments preserves the raw segmentation for multi-segment variables.
	Segments map[string][]string
	// Verb is the ":action" suffix, if any.
	Verb string
	// PathParams lists variables in template order.
	PathParams []VarSegment
}

// Route is one compiled HTTP binding.
type Route struct {
	Method   string // HTTP verb
	Template string

	Handler string // fully-qualified method name
	// Extra carries application data (the gateway stores the method and
	// service here).
	Data any

	Body         string
	ResponseBody string

	segments []segment
	verb     string
	// minSegs and maxSegs bound how many path segments can match; -1 for
	// maxSegs means unbounded (a catch-all). Used to reject candidates
	// without walking them.
	minSegs int
	maxSegs int
	// score is the sort key when several routes match the same path.
	score           scoreKey
	order           int
	HasWildcardStar bool
}

type segment struct {
	literal    string
	varField   string
	pattern    []segment // nested pattern for {x=a/*/b/**}
	isCatchAll bool
}

type scoreKey struct {
	// prefix is the number of literal segments before the first wildcard.
	// A longer literal prefix wins: "/v1/search" must outrank "/v1/{q}" and
	// a template with honest literal segments outranks a catch-all that
	// happens to be longer.
	prefix int
	// wildcard is the number of wildcard segments in the template (fewer is
	// better; a literal beats a wildcard).
	wildcard int
	// vars counts variable captures (more path variables make a template
	// more specific: {name=shelves/*} beats {id=*}).
	vars int
	// catchAll penalises "**".
	catchAll int
	// verbBonus is 1 when a verb is present, so "GET /v1/x:a" is preferred
	// over "GET /v1/x".
	verbBonus int
	// segments is the template depth (more segments is more specific).
	segments int
	order    int
}

// Parse compiles a path template.
func Parse(template string) ([]segment, string, error) {
	if template == "" || template[0] != '/' {
		return nil, "", fmt.Errorf("path template %q must start with '/'", template)
	}
	body, verb := template, ""
	if i := strings.LastIndexByte(body, ':'); i >= 0 && !strings.Contains(body[i:], "/") {
		verb = body[i+1:]
		body = body[:i]
	}
	raw, err := splitTemplatePath(strings.TrimPrefix(body, "/"))
	if err != nil {
		return nil, "", err
	}
	segs, err := parseSegments(raw, false)
	if err != nil {
		return nil, "", err
	}
	return segs, verb, nil
}

// splitTemplatePath splits on '/' but keeps "{var=a/*/b/*}" together: a '/'
// inside braces is part of the variable's pattern, not a segment separator.
func splitTemplatePath(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unmatched '}' in path %q", p)
			}
		case '/':
			if depth == 0 {
				out = append(out, p[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unmatched '{' in path %q", p)
	}
	out = append(out, p[start:])
	return out, nil
}

func parseSegments(raw []string, nested bool) ([]segment, error) {
	var out []segment
	for _, s := range raw {
		if s == "" {
			return nil, fmt.Errorf("empty path segment in template")
		}
		if strings.HasPrefix(s, "{") {
			if !strings.HasSuffix(s, "}") {
				return nil, fmt.Errorf("unclosed variable in segment %q", s)
			}
			inner := s[1 : len(s)-1]
			field, pattern := inner, "*"
			if i := strings.IndexByte(inner, '='); i >= 0 {
				field, pattern = inner[:i], inner[i+1:]
			}
			if field == "" {
				return nil, fmt.Errorf("empty variable name in segment %q", s)
			}
			for _, r := range field {
				if !(r == '_' || r == '.' ||
					r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
					r >= 0x80) {
					return nil, fmt.Errorf("invalid variable name %q", field)
				}
			}
			seg := segment{varField: field}
			if pattern != "*" && pattern != "**" {
				sub, err := parseSegments(strings.Split(pattern, "/"), true)
				if err != nil {
					return nil, fmt.Errorf("variable %q: %w", field, err)
				}
				seg.pattern = sub
			}
			if pattern == "**" {
				seg.isCatchAll = true
			}
			out = append(out, seg)
			continue
		}
		if strings.ContainsAny(s, "{}") {
			return nil, fmt.Errorf("malformed segment %q", s)
		}
		if s == "*" || s == "**" {
			out = append(out, segment{isCatchAll: s == "**"})
			continue
		}
		out = append(out, segment{literal: s})
	}
	return out, nil
}

// Compile builds a Route from a template and handler name.
func Compile(method, template, handler string) (*Route, error) {
	method = strings.ToUpper(method)
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CUSTOM":
	default:
		return nil, fmt.Errorf("unsupported HTTP method %q", method)
	}
	segs, verb, err := Parse(template)
	if err != nil {
		return nil, err
	}
	r := &Route{Method: method, Template: template, Handler: handler, segments: segs, verb: verb}
	r.minSegs, r.maxSegs = segmentBounds(segs)
	r.computeScore()
	return r, nil
}

func (r *Route) computeScore() {
	var sc scoreKey
	sc.segments = len(r.segments)
	sc.prefix = literalPrefixLen(r.segments)
	if r.verb != "" {
		sc.verbBonus = 1
	}
	var walk func(segs []segment)
	walk = func(segs []segment) {
		for _, s := range segs {
			switch {
			case s.varField != "":
				sc.vars++
				if len(s.pattern) > 0 {
					walk(s.pattern)
				}
				if s.isCatchAll || hasCatchAll(s.pattern) {
					sc.catchAll++
				}
			case s.isCatchAll:
				sc.wildcard++
				sc.catchAll++
			case s.literal == "":
				sc.wildcard++
			}
		}
	}
	walk(r.segments)
	r.score = sc
}

func hasCatchAll(segs []segment) bool {
	for _, s := range segs {
		if s.isCatchAll {
			return true
		}
	}
	return false
}

// Score is exposed for diagnostics.
type Score struct {
	Wildcards int
	Vars      int
	CatchAll  int
	HasVerb   bool
	Segments  int
	DeclOrder int
}

func (r *Route) Score() Score {
	return Score{
		Wildcards: r.score.wildcard,
		Vars:      r.score.vars,
		CatchAll:  r.score.catchAll,
		HasVerb:   r.score.verbBonus == 1,
		Segments:  r.score.segments,
		DeclOrder: r.order,
	}
}

// Less reports whether route a is more specific than b. The ordering is a
// total order, so ties are broken by declaration order, then template text.
func Less(a, b *Route) bool {
	if a.Method != b.Method {
		return a.Method < b.Method
	}
	as, bs := a.score, b.score
	if as.prefix != bs.prefix {
		return as.prefix > bs.prefix
	}
	if as.catchAll != bs.catchAll {
		return as.catchAll < bs.catchAll
	}
	if as.wildcard != bs.wildcard {
		return as.wildcard < bs.wildcard
	}
	if as.vars != bs.vars {
		return as.vars > bs.vars
	}
	if as.verbBonus != bs.verbBonus {
		return as.verbBonus > bs.verbBonus
	}
	if as.segments != bs.segments {
		return as.segments > bs.segments
	}
	if a.order != b.order {
		return a.order < b.order
	}
	return a.Template < b.Template
}

// Sort orders routes from most to least specific.
func Sort(routes []*Route) {
	sort.SliceStable(routes, func(i, j int) bool { return Less(routes[i], routes[j]) })
}

// Match attempts to match method+path. path must begin with '/'. The query
// string must already be removed.
func (r *Route) Match(method, path string) (*Match, bool) {
	if r.Method != strings.ToUpper(method) {
		return nil, false
	}
	raw := path
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	raw = strings.TrimPrefix(raw, "/")
	var parts []string
	if raw != "" {
		// A quick length check rejects most candidates. Escaped slashes can
		// raise the effective segment count, so the upper bound is only
		// applied when the path has no %2F.
		n := strings.Count(raw, "/") + 1
		escapedSlash := strings.Contains(strings.ToLower(raw), "%2f")
		if n < r.minSegs && !escapedSlash {
			return nil, false
		}
		if !escapedSlash && r.maxSegs >= 0 && n > r.maxSegs {
			return nil, false
		}
		// Split on raw '/' first, then decode. An escaped slash (%2F)
		// decodes to '/' inside a single segment; if no route matches that
		// way, Match retries with escaped slashes treated as separators so
		// a whole multi-segment variable can arrive percent-encoded.
		rawParts := strings.Split(raw, "/")
		for _, part := range rawParts {
			if part == "" {
				return nil, false
			}
			decoded, err := DecodePathValue(part)
			if err != nil {
				return nil, false
			}
			parts = append(parts, decoded)
		}
	}
	// Verb handling: an action suffix is part of the final segment.
	if len(parts) > 0 && r.verb != "" {
		last := parts[len(parts)-1]
		if i := strings.LastIndexByte(last, ':'); i >= 0 {
			if last[i+1:] == r.verb {
				parts[len(parts)-1] = last[:i]
			} else {
				return nil, false
			}
		} else {
			return nil, false
		}
	} else if len(parts) > 0 && r.verb == "" {
		// A verb-bearing request cannot match a verb-less route.
		if strings.Contains(parts[len(parts)-1], ":") {
			return nil, false
		}
	}
	// A verb-only path ("/v1/files/:download") has an empty final segment
	// and must not match a catch-all.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		return nil, false
	}
	vars := map[string]string{}
	segs := map[string][]string{}
	pos, ok := matchSegs(r.segments, parts, 0, vars, segs)
	if !ok || pos != len(parts) {
		// Retry with escaped slashes expanded into separators. This lets a
		// client send an entire multi-segment variable as one escaped
		// segment, which some HTTP clients do.
		expanded := expandEscapedSlashes(raw)
		if expanded != nil {
			vars = map[string]string{}
			segs = map[string][]string{}
			pos, ok = matchSegs(r.segments, expanded, 0, vars, segs)
			if !ok || pos != len(expanded) {
				return nil, false
			}
		} else {
			return nil, false
		}
	}
	m := &Match{Route: r, Vars: vars, Segments: segs, Verb: r.verb}
	for _, seg := range r.segments {
		if seg.varField == "" {
			continue
		}
		for _, v := range segs[seg.varField] {
			m.PathParams = append(m.PathParams, VarSegment{Field: seg.varField, Value: v})
		}
	}
	return m, true
}

// matchSegs matches template segments against path parts starting at pos and
// returns the position after the match.
func matchSegs(tmpl []segment, parts []string, pos int, vars map[string]string, segs map[string][]string) (int, bool) {
	for i := 0; i < len(tmpl); i++ {
		s := tmpl[i]
		if s.literal != "" {
			if pos >= len(parts) || parts[pos] != s.literal {
				return pos, false
			}
			pos++
			continue
		}
		if s.varField != "" {
			// Determine the expansion length. Patterns are matched greedily
			// but the *following* template segments must still line up, so
			// try each possible length from longest to shortest.
			rest := tmpl[i+1:]
			// Pattern with no trailing ** has a fixed length.
			if len(s.pattern) > 0 && !hasCatchAll(s.pattern) {
				n := patternLength(s.pattern)
				if pos+n > len(parts) {
					return pos, false
				}
				ok, captured := matchPatternCollect(s.pattern, parts[pos:pos+n])
				if !ok {
					return pos, false
				}
				vars[s.varField] = strings.Join(captured, "/")
				segs[s.varField] = captured
				pos += n
				continue
			}
			// Variable with "*" or "**": try longest first so that a
			// trailing catch-all inside the pattern stays greedy.
			maxN := len(parts) - pos
			if len(s.pattern) == 0 && !s.isCatchAll {
				// Plain {var}: single segment.
				maxN = 1
			}
			for n := maxN; n >= 0; n-- {
				if !s.isCatchAll && len(s.pattern) == 0 && n != 1 {
					continue
				}
				if !s.isCatchAll && len(s.pattern) > 0 {
					if ok, _ := matchPatternCollect(s.pattern, parts[pos:pos+n]); !ok {
						continue
					}
				}
				end, ok := matchSegs(rest, parts, pos+n, vars, segs)
				if !ok || end != len(parts) {
					continue
				}
				var captured []string
				switch {
				case s.isCatchAll:
					captured = append([]string(nil), parts[pos:pos+n]...)
				case len(s.pattern) > 0:
					_, captured = matchPatternCollect(s.pattern, parts[pos:pos+n])
				default:
					captured = []string{parts[pos]}
				}
				vars[s.varField] = strings.Join(captured, "/")
				segs[s.varField] = captured
				return end, true
			}
			return pos, false
		}
		// Bare "*" or "**".
		if s.isCatchAll {
			rest := tmpl[i+1:]
			for n := len(parts) - pos; n >= 0; n-- {
				end, ok := matchSegs(rest, parts, pos+n, vars, segs)
				if ok && end == len(parts) {
					return end, true
				}
			}
			return pos, false
		}
		if pos >= len(parts) {
			return pos, false
		}
		pos++
	}
	return pos, true
}

// patternLength returns the number of path segments a pattern consumes, or
// -1 when it contains a catch-all and is therefore variable length. Nested
// variables expand to the length of their own pattern, so "a/{b=c/*}/d" is a
// four-segment pattern.
func patternLength(segs []segment) int {
	n := 0
	for _, s := range segs {
		switch {
		case s.isCatchAll:
			return -1
		case s.varField != "" && len(s.pattern) > 0:
			sub := patternLength(s.pattern)
			if sub < 0 {
				return -1
			}
			n += sub
		default:
			n++
		}
	}
	return n
}

// literalPrefixLen returns the number of leading literal segments in a
// template, used to rank templates that differ only in their wildcard tail.
func literalPrefixLen(segs []segment) int {
	n := 0
	for _, s := range segs {
		if s.literal == "" {
			break
		}
		n++
	}
	return n
}

// matchPatternCollect matches literal patterns such as shelves/*/books/* and
// returns the wildcard captures. A bare "*" consumes one segment; "**"
// consumes the rest. The capture is returned in the second result along with
// a success flag so an empty capture is not confused with a failure.
func matchPatternCollect(tmpl []segment, parts []string) (bool, []string) {
	if len(parts) == 0 {
		return len(tmpl) == 0, nil
	}
	captured := make([]string, 0, len(parts))
	pi := 0
	for _, s := range tmpl {
		if s.literal != "" {
			if pi >= len(parts) || parts[pi] != s.literal {
				return false, nil
			}
			pi++
			continue
		}
		if s.isCatchAll {
			captured = append(captured, parts[pi:]...)
			pi = len(parts)
			continue
		}
		if pi >= len(parts) {
			return false, nil
		}
		captured = append(captured, parts[pi])
		pi++
	}
	if pi != len(parts) {
		return false, nil
	}
	return true, captured
}

// Variable describes one template variable found while parsing.
type Variable struct {
	Name     string
	Segments int // number of path segments the variable expands to
	CatchAll bool
}

// Variables returns every variable in a parsed template, in declaration order,
// with the number of segments each expands to. A pattern of literal/* pairs
// expands to the pattern's length; "**" expands to zero or more segments and
// is reported with Segments = -1.
func Variables(segs []segment) []Variable {
	var out []Variable
	var walk func([]segment)
	walk = func(ss []segment) {
		for _, s := range ss {
			if s.varField == "" {
				continue
			}
			v := Variable{Name: s.varField}
			switch {
			case s.isCatchAll:
				v.CatchAll = true
				v.Segments = -1
			case len(s.pattern) == 0:
				v.Segments = 1
			default:
				v.Segments = patternLength(s.pattern)
				if v.Segments < 0 {
					v.CatchAll = true
					v.Segments = -1
				}
			}
			out = append(out, v)
		}
	}
	walk(segs)
	return out
}

// ExamplePath fills a template with sample values, for documentation and the
// inspector's "try it" links.
func ExamplePath(template string) string {
	segs, verb, err := Parse(template)
	if err != nil {
		return template
	}
	var parts []string
	var walk func([]segment)
	walk = func(ss []segment) {
		for _, s := range ss {
			switch {
			case s.literal != "":
				parts = append(parts, s.literal)
			case s.varField != "" && len(s.pattern) > 0:
				walk(s.pattern)
			case s.varField != "":
				if s.isCatchAll {
					parts = append(parts, "multi", "segment")
				} else {
					parts = append(parts, "example")
				}
			case s.isCatchAll:
				parts = append(parts, "multi", "segment")
			default:
				parts = append(parts, "example")
			}
		}
	}
	walk(segs)
	p := "/" + strings.Join(parts, "/")
	if verb != "" {
		p += ":" + verb
	}
	return p
}

// expandEscapedSlashes re-splits a raw path on both '/' and '%2F'/'%2f',
// yielding decoded segments. It returns nil when the path contains no escaped
// slash.
func expandEscapedSlashes(raw string) []string {
	lower := strings.ToLower(raw)
	if !strings.Contains(lower, "%2f") {
		return nil
	}
	// Normalise the escaped slash to a separator before splitting, so the
	// split sees a uniform separator. Decoding then happens per segment.
	normalised := strings.NewReplacer("%2F", "/", "%2f", "/").Replace(raw)
	parts := strings.Split(normalised, "/")
	for i, p := range parts {
		if p == "" {
			return nil
		}
		d, err := DecodePathValue(p)
		if err != nil {
			return nil
		}
		parts[i] = d
	}
	return parts
}

// segmentBounds computes how many path segments a template can consume. A
// negative max means unbounded.
func segmentBounds(segs []segment) (min, max int) {
	for _, s := range segs {
		switch {
		case s.isCatchAll:
			return min, -1
		case s.varField != "" && len(s.pattern) > 0:
			subMin, subMax := segmentBounds(s.pattern)
			min += subMin
			if subMax < 0 {
				return min, -1
			}
			max += subMax
		case s.varField != "":
			min++
			max++
		default:
			// literals and bare "*"
			min++
			max++
		}
	}
	return min, max
}

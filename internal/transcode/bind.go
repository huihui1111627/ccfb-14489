package transcode

import (
	"fmt"
	"sort"
	"strings"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/jsoncodec"
	"example.com/idlbridge/internal/routing"
)

// BindOptions controls request binding.
type BindOptions struct {
	JSON jsoncodec.Options
	// IgnoreUnknownQuery drops query parameters that do not name a field
	// instead of failing.
	IgnoreUnknownQuery bool
}

// BindRequest builds a request message for a binding.
//
// Precedence follows the HTTP transcoding spec: path variables win, then the
// JSON body, then query parameters. A field bound by the path is never
// overwritten; query parameters for fields already set by the body are
// ignored. The returned Report records every assignment in order so the
// inspector can show where each field came from.
func BindRequest(b *Binding, match *routing.Match, body []byte, query []routing.Param, opt BindOptions) (*dyn.Message, *BindReport, error) {
	msg := dyn.New(b.Method.Input)
	report := &BindReport{}
	// Fields bound by the path, keyed by the leaf field so that a variable
	// targeting "bag.text" protects that leaf from the body and the query
	// while leaving siblings writable.
	pathSet := map[*idl.Field]bool{}
	for _, chain := range b.PathFields {
		pathSet[chain[len(chain)-1]] = true
	}
	// Leaf fields the body wrote. Query parameters may not overwrite
	// those; subfields the body left unset remain fillable.
	bodySet := map[*idl.Field]bool{}

	if len(body) > 0 {
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" && trimmed != "null" {
			if !b.BodyStar && b.BodyField == nil {
				return nil, report, &BindingError{Kind: "body", Msg: fmt.Sprintf(
					"method %s does not accept a request body (the rule has no body)", b.Method.FullName)}
			}
			if err := applyBody(b, msg, body, opt); err != nil {
				return nil, report, err
			}
			if b.BodyStar {
				collectLeaves(msg, bodySet)
			} else if b.BodyField != nil {
				if sub, ok := msg.Fields[b.BodyField.Number].(*dyn.Message); ok && sub != nil {
					collectLeaves(sub, bodySet)
				} else {
					bodySet[b.BodyField] = true
				}
			}
			report.Body = true
		}
	}
	if err := applyPath(b, msg, match, pathSet, report); err != nil {
		return nil, report, err
	}
	if err := applyQuery(b, msg, query, pathSet, bodySet, report, opt); err != nil {
		return nil, report, err
	}
	return msg, report, nil
}

// collectLeaves records every scalar leaf present in m, descending into
// message fields. Repeated and map fields are recorded wholesale because
// they have no single writable leaf.
func collectLeaves(m *dyn.Message, out map[*idl.Field]bool) {
	for _, f := range m.Desc.Fields {
		v, ok := m.Fields[f.Number]
		if !ok {
			continue
		}
		if f.Type.Message != nil && !f.Repeated && !f.Map {
			sub, ok := v.(*dyn.Message)
			if ok && sub != nil {
				collectLeaves(sub, out)
				continue
			}
		}
		out[f] = true
	}
}

// Assignment records one field value written during binding.
type Assignment struct {
	Source string `json:"source"`          // "path", "body" or "query"
	Param  string `json:"param,omitempty"` // original parameter name, if any
	Field  string `json:"field"`           // dotted proto field path
	Value  string `json:"value"`           // human-readable value
}

// BindReport describes how a request was bound.
type BindReport struct {
	Body        bool
	Assignments []Assignment
}

func (r *BindReport) add(a Assignment) { r.Assignments = append(r.Assignments, a) }

func applyBody(b *Binding, msg *dyn.Message, body []byte, opt BindOptions) error {
	if !b.BodyStar && b.BodyField == nil {
		return &BindingError{Kind: "body", Msg: fmt.Sprintf(
			"method %s does not accept a request body (the rule has no body)", b.Method.FullName)}
	}
	if b.BodyStar {
		parsed, err := jsoncodec.Unmarshal(body, b.Method.Input, opt.JSON)
		if err != nil {
			return &BindingError{Kind: "body", Msg: err.Error()}
		}
		for n, v := range parsed.Fields {
			msg.Fields[n] = v
		}
		msg.Unknown = append(msg.Unknown, parsed.Unknown...)
		return nil
	}
	f := b.BodyField
	parsed, err := jsoncodec.UnmarshalField(f, body, opt.JSON)
	if err != nil {
		return &BindingError{Kind: "body", Msg: err.Error()}
	}
	msg.Set(f, parsed)
	return nil
}

func applyPath(b *Binding, msg *dyn.Message, match *routing.Match, set map[*idl.Field]bool, report *BindReport) error {
	names := make([]string, 0, len(b.PathFields))
	for name := range b.PathFields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		chain := b.PathFields[name]
		raw, ok := match.Vars[name]
		if !ok {
			continue
		}
		leaf := chain[len(chain)-1]
		var val any
		var err error
		// A multi-segment variable may legitimately expand to zero segments
		// (a trailing catch-all) or to several; a single real segment still
		// parses as a normal scalar, so only the multi-segment case needs
		// the string/bytes restriction.
		if b.PathSegments[name] != 1 && match.Segments[name] != nil && len(match.Segments[name]) != 1 {
			// Multi-segment bindings target string or bytes fields; other
			// types cannot meaningfully represent several segments.
			switch {
			case leaf.Type.Message == nil && (leaf.Type.Scalar == idl.TString || leaf.Type.Scalar == idl.TBytes):
				val, err = jsoncodec.StringToValue(leaf.Type, raw, jsoncodec.Options{})
			default:
				return &BindingError{Kind: "path", Msg: fmt.Sprintf(
					"path variable %q expands to %d segments and must bind a string or bytes field, not %s",
					name, b.PathSegments[name], leaf.Type)}
			}
		} else {
			val, err = jsoncodec.StringToValue(leaf.Type, raw, jsoncodec.Options{})
		}
		if err != nil {
			return &BindingError{Kind: "path", Field: name, Msg: err.Error()}
		}
		if err := dyn.SetPath(msg, chain, val); err != nil {
			return &BindingError{Kind: "path", Field: name, Msg: err.Error()}
		}
		report.add(Assignment{Source: "path", Param: name, Field: joinFieldNames(chain), Value: jsoncodec.ValueString(val)})
	}
	return nil
}

func joinFieldNames(chain []*idl.Field) string {
	parts := make([]string, len(chain))
	for i, f := range chain {
		parts[i] = f.Name
	}
	return strings.Join(parts, ".")
}

func applyQuery(b *Binding, msg *dyn.Message, query []routing.Param, pathSet, bodySet map[*idl.Field]bool, report *BindReport, opt BindOptions) error {
	// Preserve the order in which keys first appear so assignment reports
	// are stable, then process all values for a key together.
	byKey := map[string][]routing.Param{}
	var keyOrder []string
	for _, p := range query {
		if _, ok := byKey[p.Key]; !ok {
			keyOrder = append(keyOrder, p.Key)
		}
		byKey[p.Key] = append(byKey[p.Key], p)
	}
	sort.Strings(keyOrder)
	for _, key := range keyOrder {
		vals := byKey[key]
		comps, err := routing.ParseQueryKey(key)
		if err != nil {
			if opt.IgnoreUnknownQuery {
				continue
			}
			return &BindingError{Kind: "query", Field: key, Msg: err.Error()}
		}
		err = bindQueryPath(b.Method.Input, msg, comps, vals, pathSet, bodySet, report, opt, key)
		if err != nil {
			if opt.IgnoreUnknownQuery {
				continue
			}
			return err
		}
	}
	return nil
}

func bindQueryPath(desc *idl.Message, msg *dyn.Message, comps []routing.QueryComponent, vals []routing.Param, pathSet, bodySet map[*idl.Field]bool, report *BindReport, opt BindOptions, key string) error {
	if len(comps) == 0 {
		return &BindingError{Kind: "query", Field: key, Msg: "empty field path"}
	}
	c := comps[0]
	f := lookupField(desc, c.Name)
	if f == nil {
		return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf("message %s has no field %q", desc.FullName, c.Name)}
	}
	// Path-bound leaves win over everything; body-written leaves win over
	// query parameters. Protected only at the leaf, so a query parameter
	// may still fill a subfield the body left absent.
	if pathSet[f] {
		return nil
	}
	if bodySet[f] {
		return nil
	}
	rest := comps[1:]

	switch {
	case f.Map:
		// The key comes from the bracket, and any remaining components
		// address fields of a message-valued entry.
		if len(rest) == 0 || rest[0].AppendElem {
			return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf(
				"map field %s needs exactly one key, e.g. %s[somekey]=value", f.Name, f.Name)}
		}
		k, err := jsoncodec.StringToValue(idl.Type{Scalar: f.MapKey}, rest[0].Name, opt.JSON)
		if err != nil {
			return &BindingError{Kind: "query", Field: key, Msg: "invalid map key: " + err.Error()}
		}
		mm, _ := msg.Fields[f.Number].(dyn.Map)
		if mm == nil {
			mm = dyn.Map{}
		}
		if len(rest) == 1 {
			if len(vals) > 1 {
				return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf(
					"map field %s given more than once for the same key", f.Name)}
			}
			v, err := jsoncodec.StringToValue(f.MapValue, vals[0].Value, opt.JSON)
			if err != nil {
				return &BindingError{Kind: "query", Field: key, Msg: err.Error()}
			}
			mm[k] = v
			msg.Set(f, mm)
			report.add(Assignment{Source: "query", Param: key, Field: f.Name + "[" + rest[0].Name + "]", Value: jsoncodec.ValueString(v)})
			return nil
		}
		if f.MapValue.Message == nil {
			return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf(
				"map field %s has scalar values and no sub-fields", f.Name)}
		}
		entry, _ := mm[k].(*dyn.Message)
		if entry == nil {
			entry = dyn.New(f.MapValue.Message)
			mm[k] = entry
		}
		msg.Set(f, mm)
		return bindQueryPath(f.MapValue.Message, entry, rest[1:], vals, pathSet, bodySet, report, opt, key)

	case f.Repeated && f.Type.Message == nil:
		// "items[]" is an explicit append marker; it is equivalent to
		// repeating the plain key.
		if len(rest) == 1 && rest[0].AppendElem {
			rest = nil
		}
		if len(rest) != 0 {
			return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf("field %s is a repeated scalar and has no sub-fields", f.Name)}
		}
		list, _ := msg.Fields[f.Number].([]any)
		for _, p := range vals {
			v, err := jsoncodec.StringToValue(f.Type, p.Value, opt.JSON)
			if err != nil {
				return &BindingError{Kind: "query", Field: key, Msg: err.Error()}
			}
			list = append(list, v)
			report.add(Assignment{Source: "query", Param: key, Field: f.Name, Value: jsoncodec.ValueString(v)})
		}
		msg.Set(f, list)
		return nil

	case f.Repeated && f.Type.Message != nil:
		// Repeated message: either "items.name=v" (each key appends or
		// extends), "items[].name=v", or "items[2].name=v".
		if len(rest) == 0 {
			return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf(
				"repeated message field %s needs a sub-field, e.g. %s.name=value", f.Name, f.Name)}
		}
		idx := -1
		if rest[0].IsIndex {
			idx = rest[0].Index
			rest = rest[1:]
		} else if rest[0].AppendElem {
			rest = rest[1:]
		}
		list, _ := msg.Fields[f.Number].([]any)
		if idx >= 0 {
			for len(list) <= idx {
				list = append(list, dyn.New(f.Type.Message))
			}
		} else if len(list) == 0 {
			list = append(list, dyn.New(f.Type.Message))
		}
		child, _ := list[len(list)-1].(*dyn.Message)
		if child == nil {
			child = dyn.New(f.Type.Message)
			list[len(list)-1] = child
		}
		msg.Set(f, list)
		if err := bindQueryPath(f.Type.Message, child, rest, vals, pathSet, bodySet, report, opt, key); err != nil {
			return err
		}
		return nil

	case f.Type.Message != nil:
		// Singular message: recurse.
		if len(rest) == 0 {
			return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf(
				"message field %s needs a sub-field, e.g. %s.name=value", f.Name, f.Name)}
		}
		child, _ := msg.Fields[f.Number].(*dyn.Message)
		if child == nil {
			child = dyn.New(f.Type.Message)
			msg.Set(f, child)
		}
		return bindQueryPath(f.Type.Message, child, rest, vals, pathSet, bodySet, report, opt, key)
	}

	// Singular scalar or enum leaf.
	if len(rest) != 0 {
		return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf("field %s has no sub-fields", f.Name)}
	}
	if pathSet[f] {
		return nil
	}
	if len(vals) > 1 {
		return &BindingError{Kind: "query", Field: key, Msg: fmt.Sprintf(
			"singular field given %d times; use a repeated field for multiple values", len(vals))}
	}
	v, err := jsoncodec.StringToValue(f.Type, vals[0].Value, opt.JSON)
	if err != nil {
		return &BindingError{Kind: "query", Field: key, Msg: err.Error()}
	}
	if f.Oneof != nil {
		for _, other := range f.Oneof.Fields {
			if other != f {
				delete(msg.Fields, other.Number)
			}
		}
	}
	msg.Set(f, v)
	report.add(Assignment{Source: "query", Param: key, Field: f.Name, Value: jsoncodec.ValueString(v)})
	return nil
}

func lookupField(m *idl.Message, name string) *idl.Field {
	if f := m.FieldByName(name); f != nil {
		return f
	}
	if f := m.FieldByName(idl.JSONName(name)); f != nil {
		return f
	}
	if f := m.FieldByName(idl.SnakeName(name)); f != nil {
		return f
	}
	return nil
}

// BindingError describes a request binding failure.
type BindingError struct {
	Kind  string // "path", "query", "body"
	Field string
	Msg   string
}

func (e *BindingError) Error() string {
	if e.Field != "" {
		return e.Kind + " parameter " + e.Field + ": " + e.Msg
	}
	return e.Kind + ": " + e.Msg
}

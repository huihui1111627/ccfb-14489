package transcode

import (
	"fmt"
	"strings"

	"example.com/idlbridge/internal/dyn"
	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/routing"
)

// Binding is one compiled HTTP rule attached to an RPC method.
type Binding struct {
	Method *idl.Method
	Rule   *idl.HTTPRule

	Route *routing.Route

	// BodyField is the request field that receives the JSON body, or nil
	// when the body is ignored. BodyStar means the whole request message.
	BodyField *idl.Field
	BodyStar  bool
	// ResponseField, when set, narrows the response to one field.
	ResponseField *idl.Field

	// PathFields maps a path variable's dotted name to the resolved field
	// chain. Variables with a pattern expand into several segments and are
	// still bound to a single leaf field.
	PathFields map[string][]*idl.Field
	// PathSegments records how many segments each variable consumes
	// (-1 for a catch-all).
	PathSegments map[string]int

	// QueryFields lists fields eligible for query binding: every leaf field
	// not already bound by the path or covered by the body.
	QueryFields []*idl.Field
	// queryIndex maps lower-case proto and JSON names to fields, populated
	// lazily with flatten where needed.
	queryIndex map[string]*idl.Field

	// Order is the declaration order across the whole API, used for
	// deterministic tie-breaking.
	Order int
}

// BuildBindings compiles every HTTP rule in the registry into a routing table
// and a binding list.
func BuildBindings(reg *idl.Registry) (*routing.Table, []*Binding, error) {
	table := routing.NewTable()
	var out []*Binding
	order := 0
	for _, m := range reg.Methods() {
		var add func(rule *idl.HTTPRule) error
		add = func(rule *idl.HTTPRule) error {
			b, err := compileBinding(m, rule)
			if err != nil {
				return err
			}
			b.Order = order
			order++
			entry, err := table.Add(rule.Method, rule.Path, m.FullName)
			if err != nil {
				return fmt.Errorf("%s: %w", m.FullName, err)
			}
			b.Route = entry.Route
			entry.Route.Data = b
			entry.Data = b
			out = append(out, b)
			for _, sub := range rule.Additional {
				if err := add(sub); err != nil {
					return err
				}
			}
			return nil
		}
		for _, rule := range m.HTTP {
			if err := add(rule); err != nil {
				return nil, nil, err
			}
		}
	}
	return table, out, nil
}

func compileBinding(m *idl.Method, rule *idl.HTTPRule) (*Binding, error) {
	if rule.Method == "" {
		return nil, fmt.Errorf("%s: http rule %q has no method", m.FullName, rule.Path)
	}
	b := &Binding{
		Method:       m,
		Rule:         rule,
		PathFields:   map[string][]*idl.Field{},
		PathSegments: map[string]int{},
	}
	segs, _, err := routing.Parse(rule.Path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.FullName, err)
	}
	for _, v := range routing.Variables(segs) {
		chain, err := ResolveFieldPath(m.Input, v.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: path variable %q: %w", m.FullName, v.Name, err)
		}
		if chain[len(chain)-1].IsList() {
			return nil, fmt.Errorf("%s: path variable %q binds repeated field %s", m.FullName, v.Name, chain[len(chain)-1].Name)
		}
		b.PathFields[v.Name] = chain
		b.PathSegments[v.Name] = v.Segments
	}

	// Body binding.
	switch rule.Body {
	case "":
	case "*":
		b.BodyStar = true
	default:
		chain, err := ResolveFieldPath(m.Input, rule.Body)
		if err != nil {
			return nil, fmt.Errorf("%s: body field %q: %w", m.FullName, rule.Body, err)
		}
		if chain[len(chain)-1].Type.Message == nil {
			return nil, fmt.Errorf("%s: body field %q is not a message", m.FullName, rule.Body)
		}
		b.BodyField = chain[len(chain)-1]
	}

	if rule.ResponseBody != "" && rule.ResponseBody != "*" {
		chain, err := ResolveFieldPath(m.Output, rule.ResponseBody)
		if err != nil {
			return nil, fmt.Errorf("%s: response_body %q: %w", m.FullName, rule.ResponseBody, err)
		}
		b.ResponseField = chain[len(chain)-1]
	}

	// Query binding covers every field not bound by path or body.
	bound := map[*idl.Field]bool{}
	for _, chain := range b.PathFields {
		bound[chain[0]] = true
	}
	if b.BodyStar {
		for _, f := range m.Input.Fields {
			bound[f] = true
		}
	} else if b.BodyField != nil {
		bound[b.BodyField] = true
	}
	for _, f := range m.Input.Fields {
		if bound[f] {
			continue
		}
		b.QueryFields = append(b.QueryFields, f)
	}
	return b, nil
}

// ResolveFieldPath resolves a dotted protobuf field path such as
// "book.name" against a message. Each component may be either the proto name
// or the JSON name; both are accepted.
func ResolveFieldPath(m *idl.Message, path string) ([]*idl.Field, error) {
	if path == "" {
		return nil, fmt.Errorf("empty field path")
	}
	var chain []*idl.Field
	cur := m
	parts := strings.Split(path, ".")
	for i, part := range parts {
		f := cur.FieldByName(part)
		if f == nil {
			f = cur.FieldByName(idl.JSONName(part))
		}
		if f == nil {
			f = cur.FieldByName(idl.SnakeName(part))
		}
		if f == nil {
			names := make([]string, 0, len(cur.Fields))
			for _, cf := range cur.Fields {
				names = append(names, cf.Name)
			}
			return nil, fmt.Errorf("message %s has no field %q (fields: %s)",
				cur.FullName, part, strings.Join(names, ", "))
		}
		chain = append(chain, f)
		if i == len(parts)-1 {
			break
		}
		if f.Type.Message == nil {
			return nil, fmt.Errorf("field %s is not a message and cannot be traversed", f.Name)
		}
		if f.Repeated || f.Map {
			return nil, fmt.Errorf("field %s is repeated and cannot be traversed", f.Name)
		}
		cur = f.Type.Message
	}
	return chain, nil
}

// SetPathValue stores a canonical value at a resolved field chain.
func SetPathValue(m *dyn.Message, chain []*idl.Field, v any) error {
	return dyn.SetPath(m, chain, v)
}

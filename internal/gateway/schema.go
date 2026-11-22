package gateway

import (
	"fmt"
	"strings"

	"example.com/idlbridge/internal/idl"
	"example.com/idlbridge/internal/routing"
	"example.com/idlbridge/internal/transcode"
)

// Schema is a loaded API description together with compiled routing.
type Schema struct {
	Registry *idl.Registry
	Files    []*idl.File
	Table    *routing.Table
	Bindings []*transcode.Binding
	// BindingsByRoute maps a route to its binding.
	BindingsByRoute map[*routing.Route]*transcode.Binding
	// Methods lists methods that have at least one HTTP rule.
	Methods []*idl.Method
}

// NewSchema compiles a registry into routable form.
func NewSchema(reg *idl.Registry, files []*idl.File) (*Schema, error) {
	table, bindings, err := transcode.BuildBindings(reg)
	if err != nil {
		return nil, err
	}
	s := &Schema{
		Registry:        reg,
		Files:           files,
		Table:           table,
		Bindings:        bindings,
		BindingsByRoute: map[*routing.Route]*transcode.Binding{},
	}
	seen := map[*idl.Method]bool{}
	for _, b := range bindings {
		s.BindingsByRoute[b.Route] = b
		if !seen[b.Method] {
			seen[b.Method] = true
			s.Methods = append(s.Methods, b.Method)
		}
	}
	return s, nil
}

// LoadDir loads all .proto files under dir and compiles them.
func LoadDir(dir string) (*Schema, error) {
	res, err := transcode.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	return NewSchema(res.Registry, res.Files)
}

// LoadSource compiles a single in-memory description. Imports of well-known
// types resolve against the builtin registry; other imports are an error.
func LoadSource(name, src string) (*Schema, error) {
	reg := idl.NewRegistry()
	f, err := idl.ParseFile(src, name, name, reg)
	if err != nil {
		return nil, err
	}
	for _, imp := range f.Imports {
		if !strings.HasPrefix(imp, "google/protobuf/") {
			return nil, fmt.Errorf("%s: import %q is not available for in-memory descriptions", name, imp)
		}
	}
	if err := reg.Link(); err != nil {
		return nil, err
	}
	return NewSchema(reg, []*idl.File{f})
}

// FindMethod returns a method by fully-qualified or bare name.
func (s *Schema) FindMethod(name string) *idl.Method {
	if m := s.Registry.Method(name); m != nil {
		return m
	}
	var found *idl.Method
	for _, m := range s.Methods {
		if m.Name == name {
			if found != nil {
				return nil // ambiguous
			}
			found = m
		}
	}
	return found
}

// BindingsFor returns every HTTP binding of a method, in rule order.
func (s *Schema) BindingsFor(m *idl.Method) []*transcode.Binding {
	var out []*transcode.Binding
	for _, b := range s.Bindings {
		if b.Method == m {
			out = append(out, b)
		}
	}
	return out
}

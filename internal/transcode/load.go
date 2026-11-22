// Package transcode turns HTTP requests into backend calls and backend
// responses into HTTP responses. It owns the HTTP/JSON <-> binary transcoding
// rules: route matching, path/query/body binding, status mapping and
// server-streaming chunk emission.
package transcode

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/idlbridge/internal/idl"
)

// LoadResult describes a loaded schema set.
type LoadResult struct {
	Registry *idl.Registry
	Files    []*idl.File
}

// LoadDir parses every .proto file under dir (recursively, sorted by path)
// into one registry and links type references.
func LoadDir(dir string) (*LoadResult, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".proto") {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return LoadFiles(paths)
}

// LoadFiles parses the given files. Relative file names are preserved as the
// import name; the directory is searched for imports that are not listed
// explicitly.
func LoadFiles(paths []string) (*LoadResult, error) {
	reg := idl.NewRegistry()
	var files []*idl.File
	byName := map[string]bool{}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		f, err := idl.ParseFile(string(src), filepath.Base(p), p, reg)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		byName[filepath.Base(p)] = true
		byName[p] = true
	}
	// Resolve imports that were not supplied explicitly by searching next to
	// the parsed files.
	for _, f := range files {
		for _, imp := range f.Imports {
			if byName[imp] {
				continue
			}
			found := ""
			for _, p := range paths {
				cand := filepath.Join(filepath.Dir(p), imp)
				if _, err := os.Stat(cand); err == nil {
					found = cand
					break
				}
				cand = filepath.Join(filepath.Dir(p), filepath.Base(imp))
				if _, err := os.Stat(cand); err == nil {
					found = cand
					break
				}
			}
			if found == "" {
				// Well-known imports are provided by the builtin registry.
				if strings.HasPrefix(imp, "google/protobuf/") {
					byName[imp] = true
					continue
				}
				return nil, fmt.Errorf("%s: import %q not found", f.Path, imp)
			}
			src, err := os.ReadFile(found)
			if err != nil {
				return nil, err
			}
			nf, err := idl.ParseFile(string(src), imp, found, reg)
			if err != nil {
				return nil, err
			}
			files = append(files, nf)
			byName[imp] = true
			byName[found] = true
		}
	}
	if err := reg.Link(); err != nil {
		return nil, err
	}
	return &LoadResult{Registry: reg, Files: files}, nil
}

// LoadSource parses one in-memory source file. Useful for tests and the
// inspector page.
func LoadSource(name, src string) (*idl.File, error) {
	reg := idl.NewRegistry()
	f, err := idl.ParseFile(src, name, name, reg)
	if err != nil {
		return nil, err
	}
	if err := reg.Link(); err != nil {
		return nil, err
	}
	return f, nil
}

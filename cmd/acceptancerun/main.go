// Command acceptancerun replays the hand-prepared expectations in
// acceptance/expectations.json and prints each case next to the observed
// result. It is a human-readable companion to the Go acceptance tests.
//
//	go run ./cmd/acceptancerun -schema ./testdata -expect ./acceptance/expectations.json
package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"example.com/idlbridge/internal/gateway"
	"example.com/idlbridge/internal/jsoncodec"
	"example.com/idlbridge/internal/routing"
	"example.com/idlbridge/internal/transcode"
	"example.com/idlbridge/internal/wire"
)

type expectationFile struct {
	Description string               `json:"description"`
	Routes      []routeExpectation   `json:"routes"`
	Bindings    []bindingExpectation `json:"bindings"`
}

type routeExpectation struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	WantMethod string            `json:"wantMethod"`
	WantVars   map[string]string `json:"wantVars"`
	Why        string            `json:"why"`
}

type bindingExpectation struct {
	Name           string         `json:"name"`
	Method         string         `json:"method"`
	Path           string         `json:"path"`
	Query          string         `json:"query"`
	Body           string         `json:"body"`
	Why            string         `json:"why"`
	WantJSONFields map[string]any `json:"wantJSONFields"`
	WantNested     map[string]any `json:"wantNested"`
	WantHex        string         `json:"wantHex"`
	ExpectError    bool           `json:"expectError"`
}

func main() {
	schemaDir := flag.String("schema", "./testdata", "schema directory")
	expectPath := flag.String("expect", "./acceptance/expectations.json", "expectations file")
	flag.Parse()

	schema, err := gateway.LoadDir(*schemaDir)
	if err != nil {
		fail("load schema: %v", err)
	}
	data, err := os.ReadFile(*expectPath)
	if err != nil {
		fail("read expectations: %v", err)
	}
	var ex expectationFile
	if err := json.Unmarshal(data, &ex); err != nil {
		fail("parse expectations: %v", err)
	}

	failures := 0
	fmt.Printf("schema: %s (%d files, %d methods, %d routes)\n",
		*schemaDir, len(schema.Files), len(schema.Methods), len(schema.Bindings))
	fmt.Printf("expectations: %s\n\n%s\n\n", filepath.Base(*expectPath), ex.Description)

	fmt.Println("== 路由匹配 ==")
	for _, tc := range ex.Routes {
		match, ok := schema.Table.Lookup(tc.Method, tc.Path)
		gotMethod := ""
		gotVars := map[string]string{}
		if ok {
			b, _ := match.Route.Data.(*transcode.Binding)
			gotMethod = b.Method.FullName
			gotVars = match.Vars
		}
		status := "PASS"
		if gotMethod != tc.WantMethod || !sameVars(gotVars, tc.WantVars) {
			status = "FAIL"
			failures++
		}
		fmt.Printf("  %s  %s %s\n", status, tc.Method, tc.Path)
		if status == "FAIL" {
			fmt.Printf("        want %q vars=%v\n        got  %q vars=%v\n", tc.WantMethod, tc.WantVars, gotMethod, gotVars)
		} else if gotMethod == "" {
			fmt.Printf("        (无匹配) %s\n", tc.Why)
		} else {
			fmt.Printf("        -> %s %v\n", gotMethod, sortedVars(gotVars))
		}
	}

	fmt.Println("\n== 请求映射 ==")
	for _, tc := range ex.Bindings {
		fmt.Printf("  · %s\n", tc.Name)
		match, ok := schema.Table.Lookup(tc.Method, tc.Path)
		if !ok {
			fmt.Printf("    FAIL 无路由 %s %s\n", tc.Method, tc.Path)
			failures++
			continue
		}
		b := match.Route.Data.(*transcode.Binding)
		query, err := routing.ParseQuery(tc.Query)
		if err != nil {
			fmt.Printf("    FAIL 查询串: %v\n", err)
			failures++
			continue
		}
		msg, _, bindErr := transcode.BindRequest(b, match, []byte(tc.Body), query, transcode.BindOptions{})
		if tc.ExpectError {
			if bindErr == nil {
				fmt.Printf("    FAIL 期望错误，但绑定成功\n")
				failures++
			} else {
				fmt.Printf("    PASS 按预期拒绝: %v\n", bindErr)
			}
			continue
		}
		if bindErr != nil {
			fmt.Printf("    FAIL 绑定失败: %v\n", bindErr)
			failures++
			continue
		}
		doc, err := jsoncodec.Marshal(msg, jsoncodec.Options{})
		if err != nil {
			fmt.Printf("    FAIL JSON: %v\n", err)
			failures++
			continue
		}
		var got map[string]any
		json.Unmarshal(doc, &got)
		okCase := true
		for k, want := range tc.WantJSONFields {
			if !jsonEqual(got[k], want) {
				fmt.Printf("    FAIL %s = %v, 期望 %v\n", k, got[k], want)
				okCase = false
			}
		}
		for path, want := range tc.WantNested {
			if !jsonEqual(lookup(got, path), want) {
				fmt.Printf("    FAIL %s = %v, 期望 %v\n", path, lookup(got, path), want)
				okCase = false
			}
		}
		raw, _ := wire.Marshal(msg)
		gotHex := hex.EncodeToString(raw)
		if tc.WantHex != "" && gotHex != tc.WantHex {
			fmt.Printf("    FAIL 二进制 = %s, 期望 %s\n", gotHex, tc.WantHex)
			okCase = false
		}
		if okCase {
			fmt.Printf("    PASS %s\n", doc)
			fmt.Printf("         二进制 %d 字节: %s\n", len(raw), gotHex)
		} else {
			failures++
		}
	}

	fmt.Printf("\n%d 项失败\n", failures)
	if failures > 0 {
		os.Exit(1)
	}
}

func sameVars(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func sortedVars(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func lookup(root map[string]any, path string) any {
	var cur any = root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

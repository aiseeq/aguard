package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/guide"
	"github.com/aiseeq/aguard/internal/ioroute"
	"github.com/aiseeq/aguard/internal/remotebroken"
)

func sourceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root not found")
		}
		dir = parent
	}
}

func parseSource(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func declaredStringConstants(t *testing.T, dir, prefix string) []string {
	t.Helper()
	var values []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		f := parseSource(t, filepath.Join(dir, entry.Name()))
		ast.Inspect(f, func(n ast.Node) bool {
			g, ok := n.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				return true
			}
			for _, spec := range g.Specs {
				v := spec.(*ast.ValueSpec)
				for i, name := range v.Names {
					if !strings.HasPrefix(name.Name, prefix) || len(v.Values) == 0 {
						continue
					}
					lit, ok := v.Values[min(i, len(v.Values)-1)].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err == nil {
						values = append(values, value)
					}
				}
			}
			return true
		})
	}
	return values
}

func nightKeyPrefixes(t *testing.T, root string) []string {
	t.Helper()
	paths := []string{filepath.Join(root, "internal/night/night.go"), filepath.Join(root, "cmd/aguard/night.go"), filepath.Join(root, "cmd/aguard/night_fallback.go")}
	set := map[string]bool{}
	definitions := map[string][]ast.Expr{}
	constantSpecs := map[*ast.ValueSpec]bool{}
	functionReturns := map[string][]ast.Expr{}
	var keyExprs []ast.Expr
	allPaths := append([]string(nil), paths...)
	keySource := map[string]bool{}
	for _, path := range paths {
		keySource[path] = true
	}
	for _, dir := range []string{filepath.Join(root, "internal/night"), filepath.Join(root, "cmd/aguard")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				allPaths = append(allPaths, filepath.Join(dir, entry.Name()))
			}
		}
	}
	seenPath := map[string]bool{}
	for _, path := range allPaths {
		if seenPath[path] {
			continue
		}
		seenPath[path] = true
		f := parseSource(t, path)
		collectKeys := keySource[path]
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.GenDecl:
				if x.Tok != token.CONST {
					break
				}
				for _, spec := range x.Specs {
					v := spec.(*ast.ValueSpec)
					constantSpecs[v] = true
					for i, name := range v.Names {
						if len(v.Values) > 0 {
							definitions[name.Name] = append(definitions[name.Name], v.Values[min(i, len(v.Values)-1)])
						}
					}
				}
			case *ast.FuncDecl:
				if x.Body != nil {
					ast.Inspect(x.Body, func(n ast.Node) bool {
						if r, ok := n.(*ast.ReturnStmt); ok {
							functionReturns[x.Name.Name] = append(functionReturns[x.Name.Name], r.Results...)
						}
						return true
					})
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); collectKeys && ok && id.Name == "Key" {
					keyExprs = append(keyExprs, x.Value)
				}
			case *ast.AssignStmt:
				if !collectKeys {
					break
				}
				for i, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "key" && len(x.Rhs) > 0 {
						keyExprs = append(keyExprs, x.Rhs[min(i, len(x.Rhs)-1)])
					}
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Key" && len(x.Rhs) > 0 {
						keyExprs = append(keyExprs, x.Rhs[min(i, len(x.Rhs)-1)])
					}
					if id, ok := lhs.(*ast.Ident); ok && len(x.Rhs) > 0 {
						rhs := x.Rhs[min(i, len(x.Rhs)-1)]
						definitions[id.Name] = append(definitions[id.Name], rhs)
						if fn, ok := rhs.(*ast.FuncLit); ok {
							ast.Inspect(fn.Body, func(n ast.Node) bool {
								if r, ok := n.(*ast.ReturnStmt); ok {
									functionReturns[id.Name] = append(functionReturns[id.Name], r.Results...)
								}
								return true
							})
						}
					}
				}
			case *ast.ValueSpec:
				if constantSpecs[x] {
					break
				}
				for i, name := range x.Names {
					if !collectKeys {
						continue
					}
					if len(x.Values) > 0 {
						rhs := x.Values[min(i, len(x.Values)-1)]
						definitions[name.Name] = append(definitions[name.Name], rhs)
						if fn, ok := rhs.(*ast.FuncLit); ok {
							ast.Inspect(fn.Body, func(n ast.Node) bool {
								if r, ok := n.(*ast.ReturnStmt); ok {
									functionReturns[name.Name] = append(functionReturns[name.Name], r.Results...)
								}
								return true
							})
						}
					}
				}
			}
			return true
		})
	}
	var visit func(ast.Expr, map[string]bool)
	visit = func(expr ast.Expr, seen map[string]bool) {
		switch x := expr.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				value, err := strconv.Unquote(x.Value)
				if err == nil && strings.Contains(value, ":") {
					set[strings.SplitN(value, ":", 2)[0]+":"] = true
				}
			}
		case *ast.BinaryExpr:
			visit(x.X, seen)
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(x.Args) > 0 {
				visit(x.Args[0], seen)
			}
			if id, ok := x.Fun.(*ast.Ident); ok {
				for _, result := range functionReturns[id.Name] {
					visit(result, seen)
				}
			}
		case *ast.Ident:
			if seen[x.Name] {
				return
			}
			seen[x.Name] = true
			for _, def := range definitions[x.Name] {
				visit(def, seen)
			}
		}
	}
	for _, expr := range keyExprs {
		visit(expr, map[string]bool{})
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func remoteNoticeSamples(t *testing.T, root string) []remotebroken.Notice {
	t.Helper()
	f := parseSource(t, filepath.Join(root, "internal/remotebroken/remote.go"))
	fragments := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil && (strings.Contains(value, ":") || value == "\x00") {
			fragments[value] = true
		}
		return true
	})
	var samples []remotebroken.Notice
	for value := range fragments {
		switch value {
		case "first:":
			samples = append(samples, remotebroken.Notice{Key: value + "host"})
		case "summary:":
			samples = append(samples, remotebroken.Notice{Key: value + "host", Summary: true})
		case "\x00":
			samples = append(samples, remotebroken.Notice{Key: "project" + value + "host"})
		}
	}
	return samples
}

func TestGuideMapsCoverSourceValues(t *testing.T) {
	root := sourceRoot(t)
	for _, value := range declaredStringConstants(t, filepath.Join(root, "internal/queue"), "Kind") {
		kind := queueGuideKind(value)
		if _, ok := guide.Lookup(kind); !ok || kind == "queue.other" {
			t.Errorf("queue kind %q has no dedicated guide entry (got %q)", value, kind)
		}
	}
	for _, value := range declaredStringConstants(t, filepath.Join(root, "internal/ioroute"), "Why") {
		kind := ioGuideKind(ioroute.Why(value))
		if _, ok := guide.Lookup(kind); !ok || kind == "io.other" {
			t.Errorf("io why %q has no dedicated guide entry (got %q)", value, kind)
		}
	}
	for _, prefix := range nightKeyPrefixes(t, root) {
		kind := nightGuideKind(prefix + "2026-01-01")
		if _, ok := guide.Lookup(kind); !ok || kind == "night.other" {
			t.Errorf("night key prefix %q has no dedicated guide entry (got %q)", prefix, kind)
		}
	}
	if kind := codexGuideKind(codexwatch.KindRejected); kind != "codex.rejected" {
		t.Errorf("rejected guide kind = %q", kind)
	}
	for _, notice := range remoteNoticeSamples(t, root) {
		kind := remoteGuideKind(notice)
		if _, ok := guide.Lookup(kind); !ok || kind == "remote.other" {
			t.Errorf("remote %+v has no guide entry", notice)
		}
	}
	if queueGuideKind("unknown") != "queue.other" || ioGuideKind("unknown") != "io.other" || nightGuideKind("unknown") != "night.other" || codexGuideKind("unknown") != "codex.rejected" || remoteGuideKind(remotebroken.Notice{}) != "remote.other" {
		t.Fatal("неизвестные значения должны оставаться страховкой *.other")
	}
}

func TestGuideMapReturnsOnlyLiteralKinds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(sourceRoot(t), "cmd/aguard/guide_map.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasSuffix(fn.Name.Name, "GuideKind") {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			r, ok := n.(*ast.ReturnStmt)
			if !ok || len(r.Results) != 1 {
				return true
			}
			lit, ok := r.Results[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: mapping %s must return a literal guide kind", fset.Position(r.Pos()), fn.Name.Name)
			}
			return true
		})
	}
}

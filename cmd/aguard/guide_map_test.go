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
	for _, path := range paths {
		f := parseSource(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			var expr ast.Expr
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && id.Name == "Key" {
					expr = x.Value
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "key" && len(x.Rhs) > 0 {
						expr = x.Rhs[0]
					}
				}
			}
			if expr != nil {
				for expr != nil {
					if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						value, err := strconv.Unquote(lit.Value)
						if err == nil && strings.Contains(value, ":") {
							set[strings.SplitN(value, ":", 2)[0]+":"] = true
						}
						break
					}
					binary, ok := expr.(*ast.BinaryExpr)
					if !ok {
						break
					}
					expr = binary.X
				}
			}
			return true
		})
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
	for _, raw := range declaredStringConstants(t, filepath.Join(root, "internal/codexwatch"), "Kind") {
		kind := codexGuideKind(codexwatch.Kind(raw))
		if raw == string(codexwatch.KindNoCredit) { // только строка журнала, письма нет
			continue
		}
		if _, ok := guide.Lookup(kind); !ok || kind == "codex.other" {
			t.Errorf("codex %q has no dedicated guide entry (got %q)", raw, kind)
		}
	}
	for _, notice := range remoteNoticeSamples(t, root) {
		kind := remoteGuideKind(notice)
		if _, ok := guide.Lookup(kind); !ok || kind == "remote.other" {
			t.Errorf("remote %+v has no guide entry", notice)
		}
	}
	if queueGuideKind("unknown") != "queue.other" || ioGuideKind("unknown") != "io.other" || nightGuideKind("unknown") != "night.other" || codexGuideKind("unknown") != "codex.other" || remoteGuideKind(remotebroken.Notice{}) != "remote.other" {
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

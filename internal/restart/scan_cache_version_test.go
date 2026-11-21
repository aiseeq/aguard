package restart

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// scanStateRoots — объявления, которые строят сохраняемое в кеш состояние разбора: новый разбор, восстановление
// из снимка и сам transcriptScan со всеми его методами.
var scanStateRoots = []string{"newScan", "scanFromState", "transcriptScan"}

// scanRecomputed — функции, чей результат снимок не хранит: scanFromState зовёт их заново при загрузке,
// поэтому их внешние зависимости (akit) сторожу не нужны.
var scanRecomputed = map[string]bool{"commandWaits": true}

// TestScanCacheLogicFingerprint — сторож версии кеша разбора: хеш файлов пакета, от которых зависит состояние
// transcriptScan, сверяется со scanCacheLogicSHA256. Файлы выводятся из кода: объявления пакета, достижимые
// из scanStateRoots. Ключ хеша — «import path/имя файла», поэтому место checkout на хеш не влияет.
func TestScanCacheLogicFingerprint(t *testing.T) {
	pkgPath := reflect.TypeFor[transcriptScan]().PkgPath()
	files := scanCacheLogicFiles(t)
	t.Logf("файлы разбора: %v", files)
	hash := sha256.New()
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", path.Join(pkgPath, name), len(b))
		hash.Write(b)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != scanCacheLogicSHA256 {
		t.Fatalf("логика разбора изменилась: подними scanCacheVersion и обнови хеш (sha256=%s)", got)
	}
}

// scanDecl — объявление верхнего уровня: функция, метод, тип или одно имя var/const.
type scanDecl struct {
	file  string
	name  string
	ident *ast.Ident // имя в объявлении: его позиция — ключ объекта go/types
	node  ast.Node   // что обходить
}

// scanCacheLogicFiles — файлы пакета (тест идёт в его папке) с объявлениями, достижимыми из scanStateRoots.
// Ссылки разрешает go/types по исходникам одного пакета: импорты — пустые пакеты, ошибки типов от этого
// пропускаются, имена и методы своего пакета разрешаются точно. Вызов пакета вне стандартной библиотеки из
// достижимого кода — ошибка: его результат попал бы в снимок мимо сторожа.
func scanCacheLogicFiles(t *testing.T) []string {
	t.Helper()
	fset, files, names := parseScanPackage(t)
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: emptyImporter{}, Error: func(error) {}}
	pkg, _ := conf.Check("restart", fset, files, info)
	declAt := map[token.Pos]scanDecl{}
	var roots []scanDecl
	for i, file := range files {
		for _, decl := range file.Decls {
			for _, d := range declsOf(names[i], decl) {
				declAt[d.ident.Pos()] = d
				if slices.Contains(scanStateRoots, d.name) || isScanMethod(d) {
					roots = append(roots, d)
				}
			}
		}
	}
	reached := map[token.Pos]bool{}
	found := map[string]bool{}
	queue := slices.Clone(roots)
	for _, d := range roots {
		reached[d.ident.Pos()] = true
		found[d.name] = true
	}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if scanRecomputed[d.name] {
			continue
		}
		ast.Inspect(d.node, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := info.Uses[id]
			if name, ok := obj.(*types.PkgName); ok {
				if imp := name.Imported().Path(); strings.Contains(strings.Split(imp, "/")[0], ".") {
					t.Errorf("%s: %s зовёт %s: результат попадёт в снимок мимо сторожа — пересчитывай при загрузке (scanRecomputed)",
						d.file, d.name, imp)
				}
				return true
			}
			if obj == nil || obj.Pkg() != pkg || reached[obj.Pos()] {
				return true
			}
			if next, ok := declAt[obj.Pos()]; ok {
				reached[obj.Pos()] = true
				found[next.name] = true
				queue = append(queue, next)
			}
			return true
		})
	}
	for _, root := range scanStateRoots {
		if !found[root] {
			t.Fatalf("корень %s не найден в пакете", root)
		}
	}
	for name := range scanRecomputed {
		if !callsFunction(roots, "scanFromState", name) {
			t.Errorf("scanFromState не зовёт %s: её результат должен пересчитываться при загрузке", name)
		}
	}
	var list []string
	for pos := range reached {
		list = append(list, declAt[pos].file)
	}
	slices.Sort(list)
	return slices.Compact(list)
}

type emptyImporter struct{}

func (emptyImporter) Import(importPath string) (*types.Package, error) {
	pkg := types.NewPackage(importPath, path.Base(importPath))
	pkg.MarkComplete()
	return pkg, nil
}

func parseScanPackage(t *testing.T) (*token.FileSet, []*ast.File, []string) {
	t.Helper()
	bp, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("пакет в папке теста: %v", err)
	}
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(bp.GoFiles))
	for _, name := range bp.GoFiles {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	return fset, files, bp.GoFiles
}

func declsOf(file string, decl ast.Decl) []scanDecl {
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		return []scanDecl{{file: file, name: decl.Name.Name, ident: decl.Name, node: decl}}
	case *ast.GenDecl:
		var out []scanDecl
		for _, spec := range decl.Specs {
			switch spec := spec.(type) {
			case *ast.TypeSpec:
				out = append(out, scanDecl{file: file, name: spec.Name.Name, ident: spec.Name, node: spec})
			case *ast.ValueSpec:
				for _, id := range spec.Names {
					out = append(out, scanDecl{file: file, name: id.Name, ident: id, node: spec})
				}
			}
		}
		return out
	}
	return nil
}

func isScanMethod(d scanDecl) bool {
	fn, ok := d.node.(*ast.FuncDecl)
	if !ok || fn.Recv == nil {
		return false
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	id, ok := typ.(*ast.Ident)
	return ok && id.Name == "transcriptScan"
}

func callsFunction(decls []scanDecl, caller, callee string) bool {
	var found bool
	for _, d := range decls {
		if d.name != caller {
			continue
		}
		ast.Inspect(d.node, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == callee {
					found = true
				}
			}
			return !found
		})
	}
	return found
}

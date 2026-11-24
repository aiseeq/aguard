package guide

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func moduleRoot(t *testing.T) string {
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
			t.Fatal("не найден корень Go-модуля")
		}
		dir = parent
	}
}

func goFiles(t *testing.T, dir string, includeTests bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != dir && (entry.Name() == "vendor" || entry.Name() == ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && (includeTests || !strings.HasSuffix(path, "_test.go")) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parseFile(t *testing.T, fset *token.FileSet, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func receiverTypeName(expr ast.Expr) string {
	for {
		switch x := expr.(type) {
		case *ast.StarExpr:
			expr = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

func namedBase(t types.Type) *types.Named {
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Named:
			return x
		case *types.Alias:
			t = types.Unalias(x)
		default:
			return nil
		}
	}
}

func implementsReceiver(receiver string, iface types.Type, pkg *types.Package) bool {
	if pkg == nil {
		return false
	}
	typeName, ok := pkg.Scope().Lookup(receiver).(*types.TypeName)
	if !ok {
		return false
	}
	t := types.Unalias(typeName.Type())
	ifaceType, ok := interfaceType(iface)
	if !ok {
		return false
	}
	return types.Implements(t, ifaceType) || types.Implements(types.NewPointer(t), ifaceType)
}

func interfaceType(t types.Type) (*types.Interface, bool) {
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Alias:
			t = types.Unalias(x)
		default:
			iface, ok := t.Underlying().(*types.Interface)
			return iface, ok
		}
	}
}

type listedPackage struct {
	ImportPath      string
	Dir             string
	Export          string
	CompiledGoFiles []string
	GoFiles         []string
	CgoFiles        []string
}

type sourcePackage struct {
	importPath string
	dir        string
	files      []string
}

func goTypePackages(t *testing.T, root string) ([]sourcePackage, map[string]string, map[string]string) {
	t.Helper()
	const modulePath = "github.com/aiseeq/aguard"
	cmd := exec.Command("go", "list", "-export", "-deps", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -export -deps -json ./...: %v: %s", err, exitErr.Stderr)
		}
		t.Fatalf("go list -export -deps -json ./...: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	exports := map[string]string{}
	packageDirs := map[string]string{}
	var packages []sourcePackage
	for decoder.More() {
		var listed listedPackage
		if err := decoder.Decode(&listed); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		if listed.Export != "" {
			exports[listed.ImportPath] = listed.Export
		}
		if listed.ImportPath != modulePath && !strings.HasPrefix(listed.ImportPath, modulePath+"/") {
			continue
		}
		rel, err := filepath.Rel(root, listed.Dir)
		if err != nil {
			t.Fatalf("package %s is outside module root: %v", listed.ImportPath, err)
		}
		dir := filepath.ToSlash(rel)
		if dir == "." {
			dir = ""
		}
		packageDirs[listed.ImportPath] = dir
		files := listed.CompiledGoFiles
		if len(files) == 0 {
			files = append(append([]string(nil), listed.GoFiles...), listed.CgoFiles...)
			for i := range files {
				if !filepath.IsAbs(files[i]) {
					files[i] = filepath.Join(listed.Dir, files[i])
				}
			}
		}
		packages = append(packages, sourcePackage{listed.ImportPath, dir, files})
	}
	return packages, exports, packageDirs
}

func TestCodexLetterNextText(t *testing.T) {
	want := map[string]string{
		"codex.warn": "действий не нужно: при строке «да» на окно aguard сам сбросит при 100%; сброс пользователю не предлагать",
		"codex.full": "действий не нужно; сброс пользователю не предлагать",
	}
	for _, letter := range Letters() {
		if next, ok := want[string(letter.Kind)]; ok {
			if letter.Next != next {
				t.Errorf("%s Next = %q, ожидалось %q", letter.Kind, letter.Next, next)
			}
			delete(want, string(letter.Kind))
		}
	}
	if len(want) != 0 {
		t.Fatalf("не найдены письма: %v", want)
	}
}

func TestLetterRegistryMatchesSendCalls(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	known := map[string]bool{}
	for _, x := range Letters() {
		known[string(x.Kind)] = true
	}
	used := map[string]bool{}
	calledMaps := map[string]bool{}
	mapFns := map[string]bool{"queueGuideKind": true, "ioGuideKind": true, "nightGuideKind": true, "codexGuideKind": true, "remoteGuideKind": true, "bgtaskGuideKind": true, "sigguardGuideKind": true, "claudeTmpGuideKind": true, "btrfsGuideKind": true, "livenessGuideKind": true}
	for _, path := range goFiles(t, filepath.Join(root, "cmd", "aguard"), false) {
		file := parseFile(t, fset, path)
		enclosing := ""
		var enclosingBody *ast.BlockStmt
		nightMapped := false
		nightKindSent := false
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				enclosing = x.Name.Name
				enclosingBody = x.Body
			case *ast.CallExpr:
				name := ""
				if s, ok := x.Fun.(*ast.SelectorExpr); ok {
					name = s.Sel.Name
				} else if id, ok := x.Fun.(*ast.Ident); ok {
					name = id.Name
				}
				idx := -1
				switch name {
				case "sendSocket":
					idx = 4
				case "deliverSocket":
					idx = 4
				case "sendAlive", "sendUrgent":
					idx = 3
				case "diskSend":
					idx = 4
				case "ioSend", "tmpSend":
					idx = 2
				case "nightEmail":
					idx = 3
				case "codexSend":
					idx = 1
				case "cpuSend":
					idx = 3
				}
				if name == "nightEmail" && len(x.Args) > 3 {
					if id, ok := x.Args[3].(*ast.Ident); ok && id.Name == "kind" {
						nightKindSent = true
					}
				}
				if idx < 0 || len(x.Args) <= idx {
					if name == "nightGuideKind" {
						nightMapped = true
					}
					return true
				}
				arg := x.Args[idx]
				switch v := arg.(type) {
				case *ast.BasicLit:
					k, e := strconv.Unquote(v.Value)
					if e != nil {
						t.Errorf("%s: вид не строка", fset.Position(v.Pos()))
					} else if !known[k] {
						t.Errorf("%s: нет вида письма %q в реестре", fset.Position(v.Pos()), k)
					} else {
						used[k] = true
					}
				case *ast.CallExpr:
					id, ok := v.Fun.(*ast.Ident)
					if !ok || !mapFns[id.Name] {
						t.Errorf("%s: вид вычислен не функцией-отображением", fset.Position(v.Pos()))
					} else {
						calledMaps[id.Name] = true
					}
				case *ast.Ident:
					forwarded := v.Name == "kind" && (enclosing == "sendAlive" || enclosing == "sendUrgent" || enclosing == "sendSocket" || enclosing == "deliverSocket" || enclosing == "tmpSend" || enclosing == "nightEmail" || enclosing == "codexSend" || enclosing == "cpuSend" || enclosing == "nightRound")
					forwarded = forwarded || v.Name == "guideKind" && (enclosing == "diskSend" || enclosing == "ioSend" || enclosing == "tmpSend" || enclosing == "nightEmail")
					mappedAt := token.NoPos
					if enclosingBody != nil {
						ast.Inspect(enclosingBody, func(inner ast.Node) bool {
							assign, ok := inner.(*ast.AssignStmt)
							if !ok {
								return true
							}
							if assign.Pos() >= x.Pos() {
								return true
							}
							for i, lhs := range assign.Lhs {
								id, ok := lhs.(*ast.Ident)
								if !ok || id.Name != v.Name || len(assign.Rhs) == 0 {
									continue
								}
								rhs := assign.Rhs[min(i, len(assign.Rhs)-1)]
								call, ok := rhs.(*ast.CallExpr)
								if !ok {
									continue
								}
								mapID, ok := call.Fun.(*ast.Ident)
								if ok && mapFns[mapID.Name] {
									forwarded = true
									mappedAt = assign.Pos()
									ast.Inspect(enclosingBody, func(candidate ast.Node) bool {
										later, ok := candidate.(*ast.AssignStmt)
										if !ok || later.Pos() <= mappedAt || later.Pos() >= x.Pos() {
											return true
										}
										for _, left := range later.Lhs {
											if ident, ok := left.(*ast.Ident); ok && ident.Name == v.Name {
												forwarded = false
											}
										}
										return true
									})
									calledMaps[mapID.Name] = true
								}
							}
							return true
						})
						if mappedAt == token.NoPos {
							ast.Inspect(enclosingBody, func(inner ast.Node) bool {
								assign, ok := inner.(*ast.AssignStmt)
								if !ok || assign.Pos() >= x.Pos() {
									return true
								}
								for _, lhs := range assign.Lhs {
									if id, ok := lhs.(*ast.Ident); ok && id.Name == v.Name {
										forwarded = false
									}
								}
								return true
							})
						}
					}
					if !forwarded {
						t.Errorf("%s: передача переменного вида вне функции доставки или после переприсваивания", fset.Position(v.Pos()))
					}
				default:
					t.Errorf("%s: неподдерживаемое выражение вида письма", fset.Position(arg.Pos()))
				}
			}
			return true
		})
		if nightMapped && nightKindSent {
			calledMaps["nightGuideKind"] = true
		}
	}
	for _, path := range goFiles(t, filepath.Join(root, "cmd", "aguard"), false) {
		file := parseFile(t, fset, path)
		function := ""
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				function = fn.Name.Name
			}
			r, ok := n.(*ast.ReturnStmt)
			if !ok || len(r.Results) != 1 {
				return true
			}
			lit, ok := r.Results[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			k, e := strconv.Unquote(lit.Value)
			if e == nil && mapFns[function] && calledMaps[function] {
				if !known[k] {
					t.Errorf("%s: отображение %s возвращает вид %q без записи guide", fset.Position(lit.Pos()), function, k)
				} else {
					used[k] = true
				}
			} else if e == nil && known[k] && isSendFunction(function) {
				used[k] = true
			}
			return true
		})
	}
	for k := range known {
		if !used[k] {
			t.Errorf("мёртвый вид guide %q: нет вызова или отображения", k)
		}
	}
}

func isSendFunction(name string) bool {
	switch name {
	case "sendSocket", "sendAlive", "sendUrgent", "deliverSocket", "diskSend", "ioSend", "tmpSend", "nightEmail", "codexSend", "cpuSend":
		return true
	default:
		return false
	}
}

func TestGuideHasAllObserverSections(t *testing.T) {
	want := []string{"bgtask", "remote", "orphans", "night", "disk", "disk, track", "names_table", "io_route", "codex", "tmp", "cpu", "ceiling", "sigguard", "said", "queue", "claude-tmp", "btrfs", "liveness"}
	seen := map[string]bool{}
	for _, x := range Letters() {
		seen[x.Observer] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("наблюдателей %d, ожидалось %d: %v", len(seen), len(want), seen)
	}
	for _, name := range want {
		if _, ok := ByName(name); !ok {
			t.Errorf("нет раздела наблюдателя %s", name)
		}
	}
}

func TestQueueWakeGuideNextText(t *testing.T) {
	letter, ok := Lookup("queue.wake")
	if !ok {
		t.Fatal("нет записи queue.wake")
	}
	want := "выполните `tk next`; первый тикет не взять: ждёт внешнего (срок, человек, чужой тикет) — `tk wait <X> --until <когда и чего>`; ждёт своего тикета ID — `tk set <ID> --blocks <X>`, X — первый; просто не сейчас — оставьте открытым, но админу уйдёт «встал»"
	if letter.Next != want {
		t.Fatalf("строка guide: %q, ожидалось %q", letter.Next, want)
	}
}

func TestLettersUseOnlyThreeQuietOutcomes(t *testing.T) {
	for _, letter := range Letters() {
		switch letter.Quiet {
		case QuietWait, QuietPass, QuietLose:
		default:
			t.Errorf("%s: неизвестное значение тишины %q", letter.Kind, letter.Quiet)
		}
	}
	for _, kind := range []Kind{"ceiling.oom", "sigguard.denied", "sigguard.unparsed"} {
		letter, ok := Lookup(kind)
		if !ok || letter.Quiet != QuietPass {
			t.Errorf("%s: аварийное письмо должно проходить сквозь тишину", kind)
		}
	}
}

func TestOnlyApprovedDeliveryChannelsAndCommands(t *testing.T) {
	root := moduleRoot(t)
	allowedNet := map[string]bool{"internal/restart/serve.go": true, "internal/linesock/linesock.go": true, "internal/trackfd/trackfd.go": true}
	commands := map[string]bool{"sh": true, "systemctl": true, "journalctl": true, "ssh": true, "git": true, "tmux": true, "systemd-run": true, "podman": true, "graft": true, "tl": true}
	variableCommands := map[string]string{
		"cmd/cl/supervise.go:compactSession":             "configured systemd-run command",
		"cmd/cl/supervise.go:start":                      "configured systemd-run command",
		"cmd/aguard/ticket.go:findClassTicket":           "configured tk executable",
		"cmd/aguard/ticket.go:tkRun":                     "configured tk executable",
		"cmd/aguard/queue.go:queueTicketNote":            "configured tk executable",
		"cmd/aguard/queue.go:queueTicketsWithTable":      "configured tk executable",
		"cmd/aguard/queue.go:queueTicketsProject":        "configured tk executable",
		"cmd/aguard/queue_wake.go:nextForQueue":          "configured tk executable",
		"cmd/aguard/queue_external.go:readQueueExternal": "configured tk executable",
		"cmd/aguard/codex.go:appServer":                  "configured codex executable",
		"cmd/aguard/codex.go:noteTicket":                 "configured tk executable",
		"cmd/aguard/night.go:nightPauseHolds":            "configured night command",
		"cmd/aguard/io_bench.go:startBenchChild":         "fixed sh -c command from io bench variant",
	}
	deliverCalls := 0
	knownVariableCalls := map[string]bool{}
	for _, path := range goFiles(t, root, false) {
		rel, _ := filepath.Rel(root, path)
		file := parseFile(t, token.NewFileSet(), path)
		imports := map[string]string{}
		for _, imp := range file.Imports {
			name, _ := strconv.Unquote(imp.Path.Value)
			if name == "net/http" || name == "net/smtp" || name == "net/mail" {
				t.Errorf("%s: импорт запрещённого канала %s", rel, name)
			}
			if name == "net" && !allowedNet[filepath.ToSlash(rel)] {
				t.Errorf("%s: импорт net не разрешён; новый канал или команда — добавить в список осознанно", rel)
			}
			if name == "github.com/aiseeq/akit/deliver" {
				alias := filepath.Base(name)
				if imp.Name != nil {
					alias = imp.Name.Name
				}
				imports[alias] = name
			}
		}
		fnName := "<file>"
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				fnName = fn.Name.Name
			}
			if selector, ok := n.(*ast.SelectorExpr); ok && selector.Sel.Name == "Send" {
				if id, ok := selector.X.(*ast.Ident); ok && imports[id.Name] == "github.com/aiseeq/akit/deliver" {
					if filepath.ToSlash(rel) != "cmd/aguard/send.go" {
						t.Errorf("%s: deliver.Send вне cmd/aguard/send.go", rel)
					}
					deliverCalls++
				}
			}
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && (s.Sel.Name == "Command" || s.Sel.Name == "CommandContext") {
				if id, ok := s.X.(*ast.Ident); ok && id.Name == "exec" {
					idx := 0
					if s.Sel.Name == "CommandContext" {
						idx = 1
					}
					if len(c.Args) <= idx {
						return true
					}
					loc := filepath.ToSlash(rel) + ":" + fnName
					if lit, ok := c.Args[idx].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						cmd, e := strconv.Unquote(lit.Value)
						if e == nil && !commands[cmd] {
							t.Errorf("%s: exec.Command(%q): новый канал или команда — добавить в список осознанно", rel, cmd)
						}
					} else if variableCommands[loc] == "" {
						t.Errorf("%s: переменное имя команды: новый канал или команда — добавить в список осознанно", loc)
					} else {
						knownVariableCalls[loc] = true
					}
				}
			}
			return true
		})
	}
	if deliverCalls != 1 {
		t.Errorf("deliver.Send: найдено %d вызовов, ожидался один в cmd/aguard/send.go", deliverCalls)
	}
	for loc, reason := range variableCommands {
		if !knownVariableCalls[loc] {
			t.Errorf("разрешённый вызов команды отсутствует: %s (%s)", loc, reason)
		}
	}
}

func TestInterruptionRegistryMatchesCode(t *testing.T) {
	root := moduleRoot(t)
	locations := map[string][]string{
		"orphans.stop": {"internal/orphans/stop.go:Stop"}, "orphans.term": {"internal/orphans/stop.go:term"},
		"orphans.kill": {"internal/orphans/stop.go:Stop"}, "orphans.container": {"internal/orphans/stop.go:podmanStop"},
		"sigguard.deny": {"cmd/aguard/sigguard.go:sigguardSend"}, "ceiling.oom": {"cmd/aguard/ceiling.go:ceilingGoneNotice"},
		"io.bench.scope": {"cmd/aguard/io_bench.go:Stop", "cmd/aguard/io_bench.go:Signal", "cmd/aguard/io_bench.go:benchHold", "cmd/aguard/io_bench.go:runBenchHold", "cmd/aguard/io.go:ioCmd"},
		"night.fallback": {"cmd/aguard/night_fallback.go:clean"}, "io.service": {"cmd/aguard/io_report.go:ioImport"},
		"io.dump_signal": {"cmd/aguard/io.go:ioSignal", "cmd/aguard/io.go:ioDump"}, "cl.supervise": {"cmd/cl/supervise.go:watch", "cmd/cl/supervise.go:compactSession", "cmd/cl/supervise.go:start"},
		"cl.signal":  {"cmd/cl/supervise.go:watch", "cmd/cl/supervise.go:signalChild"},
		"cl.console": {"cmd/cl/console.go:start", "cmd/cl/console.go:stopChild"}, "track.check.cleanup": {"internal/track/check.go:stopChild", "internal/track/check.go:Check"},
		"child.timeout": {
			"internal/guiwatch/watch.go:journalWindow", "internal/guiwatch/watch.go:kernelJournalWindow", "cmd/aguard/disk.go:diskFacts",
			"cmd/aguard/ticket.go:findClassTicket", "cmd/aguard/ticket.go:tkRun", "cmd/aguard/codex.go:appServer",
			"cmd/aguard/codex.go:noteTicket", "cmd/aguard/codex_userref.go:tlUserRef", "cmd/aguard/sigguard.go:startJournal", "cmd/aguard/ceiling.go:ceilingJournalCursor",
			"cmd/aguard/cpu.go:cpuRemote", "cmd/aguard/queue.go:queueTicketNote", "cmd/aguard/queue.go:queueTicketsWithTable", "cmd/aguard/queue.go:queueTicketsProject",
			"cmd/aguard/queue_wake.go:nextForQueue", "cmd/aguard/queue_wake.go:workerOwners", "cmd/aguard/queue_external.go:readQueueExternal",
			"cmd/aguard/cpu_stray.go:readStrays", "internal/orphans/stop.go:podmanStop",
			"cmd/aguard/night.go:nightPauseHolds", "cmd/aguard/io_sudo.go:readJournal",
		},
	}
	entries := map[string]bool{}
	for _, x := range Interruptions() {
		entries[x.ID] = true
	}
	allowed := map[string]bool{}
	allowedPairs := map[string]map[string]bool{}
	for id, locs := range locations {
		if !entries[id] {
			t.Errorf("нет таблицы прерываний %s", id)
			continue
		}
		for _, loc := range locs {
			allowed[loc] = true
			path, fnName, _ := strings.Cut(loc, ":")
			f := parseFile(t, token.NewFileSet(), filepath.Join(root, filepath.FromSlash(path)))
			found := false
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if ok && fn.Name.Name == fnName {
					found = true
					return false
				}
				return true
			})
			if !found {
				t.Errorf("%s: место %s отсутствует", id, loc)
			}
		}
	}
	for caller, targets := range map[string][]string{
		"cmd/cl/supervise.go:compactSession":   {"cmd/cl/supervise.go:signalChild"},
		"cmd/aguard/orphans.go:orphansAct":     {"internal/orphans/stop.go:Stop"},
		"cmd/aguard/track.go:trackCmd":         {"internal/track/check.go:Check"},
		"internal/orphans/stop.go:Stop":        {"internal/orphans/stop.go:term"},
		"cmd/aguard/io_report.go:ioImport":     {"cmd/aguard/io.go:ioSignal"},
		"cmd/cl/supervise.go:start":            {"cmd/cl/supervise.go:launch"},
		"cmd/aguard/main.go:main":              {"cmd/aguard/main.go:dispatch"},
		"cmd/aguard/orphans.go:orphansRound":   {"cmd/aguard/orphans.go:orphansAct"},
		"cmd/aguard/io_bench.go:ioBench":       {"cmd/aguard/io_bench.go:runIOBench"},
		"cmd/aguard/main.go:watchers":          {"cmd/aguard/orphans.go:orphansRound"},
		"cmd/aguard/io.go:ioCmd":               {"cmd/aguard/io.go:ioDump", "cmd/aguard/io_report.go:ioImport", "cmd/aguard/io_bench.go:ioBench", "cmd/aguard/io_bench.go:benchHold"},
		"cmd/aguard/orphans.go:orphansCmd":     {"cmd/aguard/orphans.go:orphansRound"},
		"cmd/aguard/main.go:run":               {"cmd/aguard/main.go:watchers"},
		"cmd/cl/supervise.go:supervise":        {"cmd/cl/supervise.go:start", "cmd/cl/supervise.go:watch", "cmd/cl/supervise.go:compactSession"},
		"cmd/cl/main.go:main":                  {"cmd/cl/supervise.go:supervise"},
		"cmd/cl/supervise.go:launch":           {"cmd/cl/console.go:start"},
		"cmd/cl/console.go:start":              {"cmd/cl/console.go:stopChild"},
		"internal/track/check.go:Check":        {"internal/track/check.go:stopChild"},
		"cmd/aguard/io_bench.go:benchHold":     {"cmd/aguard/io_bench.go:runBenchHold"},
		"cmd/aguard/io.go:ioDump":              {"cmd/aguard/io.go:ioSignal"},
		"cmd/cl/supervise.go:watch":            {"cmd/cl/supervise.go:signalChild"},
		"cmd/aguard/io_bench.go:runBenchHold":  {"cmd/aguard/io_bench.go:Signal"},
		"cmd/aguard/io_bench.go:runBenchTrial": {"cmd/aguard/io_bench.go:Stop"},
		"cmd/aguard/io_bench.go:runIOBench":    {"cmd/aguard/io_bench.go:runBenchTrial"},
		"cmd/aguard/main.go:dispatch":          {"cmd/aguard/main.go:run", "cmd/aguard/orphans.go:orphansCmd", "cmd/aguard/track.go:trackCmd", "cmd/aguard/io.go:ioCmd"},
	} {
		if allowedPairs[caller] == nil {
			allowedPairs[caller] = map[string]bool{}
		}
		for _, target := range targets {
			allowedPairs[caller][target] = true
		}
	}
	wants := map[string]string{
		"internal/orphans/stop.go:Stop": "PidfdSendSignal", "internal/orphans/stop.go:term": "PidfdSendSignal",
		"internal/orphans/stop.go:podmanStop": "CommandContext", "cmd/aguard/sigguard.go:sigguardSend": "sendUrgent",
		"cmd/aguard/ceiling.go:ceilingGoneNotice": "sendCeilingOOMNotice", "cmd/aguard/night_fallback.go:clean": "Command",
		"cmd/aguard/io.go:ioSignal":       "Command",
		"cmd/aguard/io_bench.go:Stop":     "CommandContext",
		"cmd/cl/supervise.go:signalChild": "SignalChild", "cmd/cl/supervise.go:watch": "signalChild",
		"cmd/cl/supervise.go:compactSession": "StopChild", "cmd/cl/console.go:start": "stopChild",
		"cmd/cl/console.go:stopChild": "StopChild", "internal/track/check.go:Check": "stopChild",
		"internal/track/check.go:stopChild": "StopChild",
	}
	for loc, want := range wants {
		path, fnName, _ := strings.Cut(loc, ":")
		f := parseFile(t, token.NewFileSet(), filepath.Join(root, filepath.FromSlash(path)))
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != fnName {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					switch x := call.Fun.(type) {
					case *ast.Ident:
						if x.Name == want {
							found = true
						}
					case *ast.SelectorExpr:
						if x.Sel.Name == want {
							found = true
						}
					}
				}
				return true
			})
			return false
		})
		if !found {
			t.Errorf("%s: ожидаемое действие %s не найдено", loc, want)
		}
	}
	// Вычисляем сигналящие функции из AST и распространяем свойство по вызовам
	// функций и методов как внутри пакета, так и через импорт модулей.
	type fnKey struct{ dir, receiver, name string }
	functions := map[fnKey]*ast.FuncDecl{}
	functionLocations := map[fnKey]string{}
	direct := map[fnKey]bool{}
	callers := map[fnKey][]fnKey{}
	type interfaceCall struct {
		caller fnKey
		method string
		iface  types.Type
	}
	var interfaceCalls []interfaceCall
	packages, exportFiles, packageDirs := goTypePackages(t, root)
	infoByDir := map[string]*types.Info{}
	pkgByDir := map[string]*types.Package{}
	astByPath := map[string]*ast.File{}
	lookup := func(importPath string) (io.ReadCloser, error) {
		path := exportFiles[importPath]
		if path == "" {
			return nil, fmt.Errorf("go list did not provide export data for %s", importPath)
		}
		return os.Open(path)
	}
	exportImp := importer.ForCompiler(token.NewFileSet(), "gc", lookup)
	checkedByPath := map[string]*types.Package{}
	imp := importerFunc(func(path string) (*types.Package, error) {
		if pkg := checkedByPath[path]; pkg != nil {
			return pkg, nil
		}
		return exportImp.Import(path)
	})
	for _, source := range packages {
		files := make([]*ast.File, 0, len(source.files))
		fset := token.NewFileSet()
		for _, path := range source.files {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
			astByPath[path] = f
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		var firstTypeError error
		conf := types.Config{
			Importer: imp,
			Error: func(err error) {
				if firstTypeError == nil {
					firstTypeError = err
				}
			},
		}
		checkedPkg, err := conf.Check(source.importPath, fset, files, info)
		if err != nil {
			t.Fatalf("type check %s: %v (first error: %v)", source.importPath, err, firstTypeError)
		}
		infoByDir[source.dir] = info
		checkedByPath[source.importPath] = checkedPkg
		pkgByDir[source.dir] = checkedPkg
	}
	for _, source := range packages {
		dir := source.dir
		info := infoByDir[dir]
		for _, path := range source.files {
			file := astByPath[path]
			rel, _ := filepath.Rel(root, path)
			imports := map[string]string{}
			for _, imp := range file.Imports {
				pkgPath, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("invalid import path %q in %s: %v", imp.Path.Value, path, err)
				}
				if _, ok := packageDirs[pkgPath]; !ok {
					continue
				}
				name := filepath.Base(pkgPath)
				if imp.Name != nil {
					name = imp.Name.Name
				}
				imports[name] = packageDirs[pkgPath]
			}
			var current fnKey
			ast.Inspect(file, func(n ast.Node) bool {
				if fn, ok := n.(*ast.FuncDecl); ok {
					receiver := ""
					if fn.Recv != nil && len(fn.Recv.List) != 0 {
						field := fn.Recv.List[0]
						receiver = receiverTypeName(field.Type)
						if tv, ok := info.Types[field.Type]; ok && tv.Type != nil {
							if named := namedBase(tv.Type); named != nil {
								receiver = named.Obj().Name()
							}
						}
					}
					current = fnKey{dir, receiver, fn.Name.Name}
					functions[current] = fn
					functionLocations[current] = filepath.ToSlash(rel) + ":" + fn.Name.Name
					ast.Inspect(fn.Body, func(inner ast.Node) bool {
						call, ok := inner.(*ast.CallExpr)
						if !ok {
							return true
						}
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
							name := sel.Sel.Name
							isSignalType := name == "Signal" && selectorRoot(sel.X) == "syscall"
							if (name == "Signal" && len(call.Args) > 0 && !isSignalType && !literalSignal(call.Args[0], "SIGWINCH")) || name == "Kill" || name == "Tgkill" || name == "PidfdSendSignal" || akitSignal[name] {
								if name != "PidfdSendSignal" || len(call.Args) <= 1 || !isZero(call.Args[1]) {
									direct[current] = true
								}
							}
							if (name == "Command" || name == "CommandContext") && isStopExec(call) {
								direct[current] = true
							}
						}
						switch fun := call.Fun.(type) {
						case *ast.Ident:
							callers[current] = append(callers[current], fnKey{dir, "", fun.Name})
						case *ast.SelectorExpr:
							if id, ok := fun.X.(*ast.Ident); ok {
								if targetDir, ok := imports[id.Name]; ok {
									callers[current] = append(callers[current], fnKey{targetDir, "", fun.Sel.Name})
								}
							}
							if selection := info.Selections[fun]; selection != nil {
								method, ok := selection.Obj().(*types.Func)
								if !ok {
									return true
								}
								signature, ok := method.Type().(*types.Signature)
								if !ok {
									return true
								}
								methodRecv := selection.Recv()
								if declaredRecv := signature.Recv(); declaredRecv != nil {
									methodRecv = declaredRecv.Type()
								}
								if _, ok := interfaceType(methodRecv); ok {
									interfaceCalls = append(interfaceCalls, interfaceCall{current, fun.Sel.Name, methodRecv})
								} else if declaredRecv := signature.Recv(); declaredRecv != nil {
									if named := namedBase(declaredRecv.Type()); named != nil && named.Obj().Pkg() != nil {
										if targetDir, ok := packageDirs[named.Obj().Pkg().Path()]; ok {
											callers[current] = append(callers[current], fnKey{targetDir, named.Obj().Name(), fun.Sel.Name})
										}
									}
								}
							}
						}
						return true
					})
					return false
				}
				return true
			})
		}
	}
	for _, dispatch := range interfaceCalls {
		// Interface dispatch conservatively reaches every same-named method in
		// this module whose receiver type implements the interface, using the
		// same checked package objects used while resolving the interface.
		for candidate := range functions {
			if candidate.receiver != "" && candidate.name == dispatch.method && implementsReceiver(candidate.receiver, dispatch.iface, pkgByDir[candidate.dir]) {
				callers[dispatch.caller] = append(callers[dispatch.caller], candidate)
			}
		}
	}
	actualPairs := map[string]bool{}
	for caller, targets := range callers {
		for _, target := range targets {
			actualPairs[functionLocations[caller]+"\x00"+functionLocations[target]] = true
		}
	}
	for caller, targets := range allowedPairs {
		for target := range targets {
			if !actualPairs[caller+"\x00"+target] {
				t.Errorf("разрешённая пара %s -> %s отсутствует в графе вызовов", caller, target)
			}
		}
	}
	signalFns := maps.Clone(direct)
	changed := true
	for changed {
		changed = false
		for caller, targets := range callers {
			for _, target := range targets {
				if signalFns[target] && !signalFns[caller] {
					signalFns[caller] = true
					changed = true
				}
			}
		}
	}
	for caller, targets := range callers {
		if !signalFns[caller] {
			continue
		}
		for _, target := range targets {
			if signalFns[target] && !allowedPairs[functionLocations[caller]][functionLocations[target]] {
				t.Errorf("%s: вызов сигналящей функции %s (%q) — новое место прерывания, добавьте в guide", functionLocations[caller], target.name, functionLocations[target])
			}
		}
	}
	allowedStop := map[string]map[string]bool{
		"cmd/aguard/night_fallback.go:clean":  {"dynamic": true},
		"cmd/aguard/io.go:ioSignal":           {"kill": true},
		"cmd/aguard/io_bench.go:Stop":         {"stop": true},
		"internal/orphans/stop.go:podmanStop": {"dynamic": true},
	}
	for _, path := range goFiles(t, root, false) {
		rel, _ := filepath.Rel(root, path)
		file := parseFile(t, token.NewFileSet(), path)
		fnName := ""
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				fnName = fn.Name.Name
			}
			loc := filepath.ToSlash(rel) + ":" + fnName
			if s, ok := n.(*ast.SelectorExpr); ok {
				name := s.Sel.Name
				if name == "Kill" || name == "Tgkill" || name == "PidfdSendSignal" || name == "StopChild" || name == "Signal" || akitSignal[name] {
					if name == "Signal" {
						call, ok := parentCall(file, s)
						if !ok || len(call.Args) == 0 || selectorRoot(s.X) == "syscall" || literalSignal(call.Args[0], "SIGWINCH") {
							return true
						}
					}
					if name == "PidfdSendSignal" {
						if call, ok := parentCall(file, s); ok && len(call.Args) > 1 {
							if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Value == "0" {
								return true
							}
						}
					}
					if !allowed[loc] {
						t.Errorf("%s: новое место прерывания %s — добавьте в guide", loc, name)
					}
				}
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if s, ok := call.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "CommandContext" {
					if id, ok := s.X.(*ast.Ident); ok && id.Name == "exec" && !allowed[loc] {
						t.Errorf("%s: CommandContext прерывает дочернюю команду по контексту, добавьте в guide", loc)
					}
				}
				if s, ok := call.Fun.(*ast.SelectorExpr); ok && (s.Sel.Name == "Command" || s.Sel.Name == "CommandContext") {
					if id, ok := s.X.(*ast.Ident); ok && id.Name == "exec" {
						cmdIndex := 0
						if s.Sel.Name == "CommandContext" {
							cmdIndex = 1
						}
						cmdName := ""
						if len(call.Args) > cmdIndex {
							if lit, ok := call.Args[cmdIndex].(*ast.BasicLit); ok {
								cmdName, _ = strconv.Unquote(lit.Value)
							}
						}
						argsStart := cmdIndex + 1
						for _, arg := range call.Args[argsStart:] {
							if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
								text, _ := strconv.Unquote(lit.Value)
								if cmdName == "sh" && strings.Contains(text, "kill") {
									t.Errorf("%s: shell-команда kill без строки в guide", loc)
								}
							}
							if lit, ok := arg.(*ast.BasicLit); ok {
								text, _ := strconv.Unquote(lit.Value)
								if text == "stop" || text == "kill" {
									if !allowedStop[loc][text] {
										t.Errorf("%s: стоп-команда без строки в guide", loc)
									}
								}
							}
						}
						if fnName == "clean" && !allowedStop[loc]["dynamic"] {
							t.Errorf("%s: команда с verb из конфигурации может быть stop/kill; добавьте в guide", loc)
						}
					}
				}
			}
			return true
		})
	}
}

func literalSignal(expr ast.Expr, want string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == want
}

func selectorRoot(expr ast.Expr) string {
	for {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok {
			break
		}
		expr = sel.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func isZero(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Value == "0"
}

func isStopExec(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext") {
		return false
	}
	idx := 0
	if sel.Sel.Name == "CommandContext" {
		idx = 1
	}
	for _, arg := range call.Args[idx+1:] {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, _ := strconv.Unquote(lit.Value)
			if value == "stop" || value == "kill" || strings.Contains(value, "kill ") {
				return true
			}
		}
	}
	return false
}

func parentCall(file *ast.File, node ast.Node) (*ast.CallExpr, bool) {
	var found *ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if call.Fun == node {
			found = call
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel == node {
			found = call
		}
		return true
	})
	return found, found != nil
}

// akitSignal — функции akit/proc, которые шлют сигнал (go doc github.com/aiseeq/akit/proc): место прерывания, как Kill.
var akitSignal = map[string]bool{"SignalChild": true, "SignalDescendant": true, "CancelByStop": true}

type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

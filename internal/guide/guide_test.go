package guide

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
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

func TestLetterRegistryMatchesSendCalls(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	known := map[string]bool{}
	for _, x := range Letters() {
		known[string(x.Kind)] = true
	}
	used := map[string]bool{}
	calledMaps := map[string]bool{}
	mapFns := map[string]bool{"queueGuideKind": true, "ioGuideKind": true, "nightGuideKind": true, "codexGuideKind": true, "remoteGuideKind": true, "bgtaskGuideKind": true, "sigguardGuideKind": true}
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
					if enclosingBody != nil {
						ast.Inspect(enclosingBody, func(inner ast.Node) bool {
							assign, ok := inner.(*ast.AssignStmt)
							if !ok {
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
									calledMaps[mapID.Name] = true
								}
							}
							return true
						})
					}
					if !forwarded {
						t.Errorf("%s: передача переменного вида вне функции доставки", fset.Position(v.Pos()))
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

func TestGuideHasAllFourteenObserverSections(t *testing.T) {
	want := []string{"bgtask", "remote", "orphans", "night", "disk", "names_table", "io_route", "codex", "tmp", "cpu", "ceiling", "sigguard", "said", "queue"}
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
	commands := map[string]bool{"sh": true, "systemctl": true, "journalctl": true, "ssh": true, "git": true, "tmux": true, "systemd-run": true, "podman": true, "graft": true}
	variableCommands := map[string]string{
		"cmd/cl/supervise.go:compactSession":             "configured systemd-run command",
		"cmd/cl/supervise.go:start":                      "configured systemd-run command",
		"cmd/aguard/ticket.go:findClassTicket":           "configured tk executable",
		"cmd/aguard/ticket.go:tkRun":                     "configured tk executable",
		"cmd/aguard/queue.go:queueTicketNote":            "configured tk executable",
		"cmd/aguard/queue.go:queueTickets":               "configured tk executable",
		"cmd/aguard/queue_wake.go:nextForQueue":          "configured tk executable",
		"cmd/aguard/queue_external.go:readQueueExternal": "configured tk executable",
		"cmd/aguard/codex.go:appServer":                  "configured codex executable",
		"cmd/aguard/codex.go:noteTicket":                 "configured tk executable",
		"cmd/aguard/night.go:nightPauseHolds":            "configured night command",
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
		"night.fallback": {"cmd/aguard/night_fallback.go:clean"}, "io.service": {"cmd/aguard/io_report.go:ioImport"},
		"io.dump_signal": {"cmd/aguard/io.go:ioSignal", "cmd/aguard/io.go:ioDump"}, "cl.supervise": {"cmd/cl/supervise.go:watch", "cmd/cl/supervise.go:compactSession", "cmd/cl/supervise.go:start"},
		"cl.signal":  {"cmd/cl/supervise.go:watch", "cmd/cl/supervise.go:signalChild"},
		"cl.console": {"cmd/cl/console.go:start", "cmd/cl/console.go:stopChild"}, "track.check.cleanup": {"internal/track/check.go:stopChild", "internal/track/check.go:Check"},
		"child.timeout": {
			"internal/guiwatch/watch.go:journalWindow", "internal/guiwatch/watch.go:kernelJournalWindow", "cmd/aguard/disk.go:diskFacts",
			"cmd/aguard/ticket.go:findClassTicket", "cmd/aguard/ticket.go:tkRun", "cmd/aguard/codex.go:appServer",
			"cmd/aguard/codex.go:noteTicket", "cmd/aguard/sigguard.go:startJournal", "cmd/aguard/ceiling.go:ceilingJournalCursor",
			"cmd/aguard/cpu.go:cpuRemote", "cmd/aguard/queue.go:queueTicketNote", "cmd/aguard/queue.go:queueTickets",
			"cmd/aguard/queue_wake.go:nextForQueue", "cmd/aguard/queue_wake.go:workerOwners", "cmd/aguard/queue_external.go:readQueueExternal",
			"cmd/aguard/cpu_stray.go:readStrays", "cmd/aguard/cpu_stray.go:readStrays", "internal/orphans/stop.go:podmanStop",
			"cmd/aguard/night.go:nightPauseHolds",
		},
	}
	entries := map[string]bool{}
	for _, x := range Interruptions() {
		entries[x.ID] = true
	}
	allowed := map[string]bool{}
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
	wants := map[string]string{
		"internal/orphans/stop.go:Stop": "PidfdSendSignal", "internal/orphans/stop.go:term": "PidfdSendSignal",
		"internal/orphans/stop.go:podmanStop": "CommandContext", "cmd/aguard/sigguard.go:sigguardSend": "sendUrgent",
		"cmd/aguard/ceiling.go:ceilingGoneNotice": "sendCeilingOOMNotice", "cmd/aguard/night_fallback.go:clean": "Command",
		"cmd/aguard/io.go:ioSignal":       "Command",
		"cmd/cl/supervise.go:signalChild": "Signal", "cmd/cl/supervise.go:watch": "signalChild",
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
	// Вычисляем сигналящие функции из AST, затем распространяем свойство по
	// вызовам внутри одного пакета. Имена обёрток здесь не перечисляются.
	type fnKey struct{ dir, name string }
	functions := map[fnKey]*ast.FuncDecl{}
	functionLocations := map[fnKey]string{}
	direct := map[fnKey]bool{}
	callers := map[fnKey][]fnKey{}
	for _, path := range goFiles(t, root, false) {
		rel, _ := filepath.Rel(root, path)
		file := parseFile(t, token.NewFileSet(), path)
		var current fnKey
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				current = fnKey{filepath.ToSlash(filepath.Dir(rel)), fn.Name.Name}
				functions[current] = fn
				functionLocations[current] = filepath.ToSlash(rel) + ":" + fn.Name.Name
				ast.Inspect(fn.Body, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					name := sel.Sel.Name
					isSignalType := name == "Signal" && selectorRoot(sel.X) == "syscall"
					if (name == "Signal" && len(call.Args) > 0 && !isSignalType && !literalSignal(call.Args[0], "SIGWINCH")) || name == "Kill" || name == "Tgkill" || name == "PidfdSendSignal" {
						if name == "PidfdSendSignal" && len(call.Args) > 1 && isZero(call.Args[1]) {
							return true
						}
						direct[current] = true
					}
					if name == "Command" || name == "CommandContext" {
						if isStopExec(call) {
							direct[current] = true
						}
					}
					return true
				})
				return false
			}
			return true
		})
		for key, fn := range functions {
			if key.dir != filepath.ToSlash(filepath.Dir(rel)) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					callers[key] = append(callers[key], fnKey{key.dir, id.Name})
				}
				return true
			})
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
			if signalFns[target] && !allowed[functionLocations[caller]] {
				t.Errorf("%s: вызов сигналящей функции %s — новое место прерывания, добавьте в guide", functionLocations[caller], target.name)
			}
		}
	}
	allowedStop := map[string]map[string]bool{
		"cmd/aguard/night_fallback.go:clean":  {"dynamic": true},
		"cmd/aguard/io.go:ioSignal":           {"kill": true},
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
				if name == "Kill" || name == "Tgkill" || name == "PidfdSendSignal" || name == "StopChild" || name == "Signal" {
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

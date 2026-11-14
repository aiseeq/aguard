package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/ceiling"
)

// Слайс агентов: вес процессора и диска ниже, чем у приложений пользователя (100), — машина отдаёт ресурсы
// пользователю, а не агентам. Файл слайса — agents.slice рядом, ставится в ~/.config/systemd/user (graft install).
const agentsSlice = "agents.slice"
const executablePermission os.FileMode = 0o111

// scopeArgv — запуск claude в своём scope слайса агентов: systemd-run --scope регистрирует scope и делает exec
// команды в том же процессе, так что терминал, сигналы и код выхода — как у голого claude. Имя scope —
// agent-<имя сессии>-<id> (id — pid cl, у перезапусков с -r<n>), оно видно в /proc/<pid>/cgroup. IOAccounting
// включает io.stat у scope — без него в agents.slice нет контроллера io и aguard io не видит запись сессий.
func scopeArgv(systemdRun, claude, name, id string, args []string) []string {
	return scopeArgvBytes(systemdRun, claude, name, id, args, 0, 0, false)
}

func scopeUnit(name, id string) string {
	if name == "" {
		name = "claude"
	}
	return "agent-" + unitSafe(name) + "-" + unitSafe(id) + ".scope"
}

func scopeArgvBytes(systemdRun, claude, name, id string, args []string, base, swap int64, enabled bool) []string {
	unit := strings.TrimSuffix(scopeUnit(name, id), ".scope")
	argv := []string{systemdRun, "--user", "--scope", "--quiet", "--collect", "--slice=" + agentsSlice, "--property=IOAccounting=yes", "--unit=" + unit, "--", claude}
	if enabled {
		prefix := append([]string(nil), argv[:len(argv)-2]...)
		prefix = append(prefix, "--property=OOMPolicy=continue", fmt.Sprintf("--property=MemoryMax=%d", base), fmt.Sprintf("--property=MemorySwapMax=%d", swap))
		argv = append(prefix, argv[len(argv)-2:]...)
	}
	return append(argv, args...)
}

var writeCeilingState = ceiling.Write

func scopeEnv(env []string, enabled bool, prefixPath string) []string {
	if !enabled || prefixPath == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, "CLAUDE_CODE_SHELL_PREFIX=") {
			out = append(out, item)
		}
	}
	return append(out, "CLAUDE_CODE_SHELL_PREFIX="+prefixPath)
}

func prepareScope(home, systemdRun, claude, name, id string, args []string, mem ceiling.ClMemory, env []string, logErr func(error)) ([]string, []string) {
	if mem.Max == "" {
		return scopeArgv(systemdRun, claude, name, id, args), env
	}
	if name == "" {
		name = "claude"
	}
	base, swap, err := ceiling.MemoryLimits(mem)
	unit := scopeUnit(name, id)
	prefix := filepath.Join(home, "bin", "cl-oom-prefix")
	if err == nil {
		var info os.FileInfo
		info, err = os.Stat(prefix)
		if err == nil && info.Mode()&executablePermission == 0 {
			err = fmt.Errorf("обёртка %s не исполняема", prefix)
		}
	}
	if err == nil {
		pid, pidErr := scopeProcessID(id)
		if pidErr != nil {
			err = fmt.Errorf("pid scope %q: %w", id, pidErr)
		} else {
			err = writeCeilingState(ceiling.StatePath(home, unit), ceiling.State{Unit: unit, Base: base, Swap: swap, By: "cl", At: time.Now(), ClaudePID: pid})
		}
	}
	if err != nil {
		if logErr != nil {
			logErr(err)
		}
		return scopeArgv(systemdRun, claude, name, id, args), env
	}
	return scopeArgvBytes(systemdRun, claude, name, id, args, base, swap, true), scopeEnv(env, true, prefix)
}

func scopeProcessID(id string) (int, error) {
	pidText := id
	for _, marker := range []string{"-r", "-c"} {
		if prefix, suffix, ok := strings.Cut(id, marker); ok {
			if _, err := strconv.Atoi(suffix); err != nil {
				return 0, fmt.Errorf("scope suffix %q: %w", marker, err)
			}
			pidText = prefix
			break
		}
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid %q", id)
	}
	return pid, nil
}

func updateScopeClaudePID(home, unit string, pid int) error {
	path := ceiling.StatePath(home, unit)
	_, err := ceiling.Update(path, func(state *ceiling.State) error {
		state.ClaudePID = pid
		return nil
	})
	return err
}

// unitSafe оставляет в имени юнита только [A-Za-z0-9_.-], прочее — «_».
func unitSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// sliceInstalled: без файла слайса systemd создаст пустой слайс с весом по умолчанию, и агенты молча получат
// равный с пользователем приоритет — поэтому отказ с подсказкой, а не запуск.
func sliceInstalled(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("нет %s: агенты получили бы приоритет наравне с пользователем; поставить: "+
			"cp ~/git/aguard/cmd/cl/agents.slice ~/.config/systemd/user/ && systemctl --user daemon-reload", path)
	}
	return nil
}

// nameArg — значение -n/--name из аргументов, если есть.
func nameArg(args []string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--name="); ok {
			return v
		}
		if (a == "-n" || a == "--name") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

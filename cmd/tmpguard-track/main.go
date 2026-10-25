// tmpguard-track — root-часть сторожа временных файлов (system-сервис).
// Записывает, кто создал каждую запись верхнего уровня в /tmp и
// /tmp/claude-tmp, и снимает их размеры. Сообщений никому не шлёт.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/journal"
	"github.com/aiseeq/aguard/internal/record"
	"github.com/aiseeq/aguard/internal/track"
)

func main() {
	lg := journal.New()
	if err := run(lg); err != nil {
		lg.Error("tmpguard-track: " + err.Error())
		os.Exit(1)
	}
}

func run(lg *slog.Logger) error {
	roots := flag.String("roots", "/tmp", "помечаемые папки через запятую (первая — корень файловой системы для open_by_handle_at)")
	containers := flag.String("containers", "/tmp/claude-tmp", "папки-контейнеры через запятую: учитывается их содержимое")
	stateDir := flag.String("state-dir", record.DefaultDir, "журнал owners.jsonl и долговременный снимок")
	runDir := flag.String("run-dir", "/run/tmpguard", "частый снимок state.json (tmpfs)")
	full := flag.Duration("full", 5*time.Minute, "период полного замера и сверки")
	young := flag.Duration("young", 15*time.Minute, "записи моложе этого замеряются чаще")
	youngEvery := flag.Duration("young-every", time.Minute, "период замера молодых записей")
	persist := flag.Duration("persist", 30*time.Minute, "период копирования снимка на диск")
	rotateMB := flag.Int64("rotate-mb", 64, "размер owners.jsonl до ротации, МБ")
	flag.Parse()
	if os.Geteuid() != 0 {
		return errors.New("нужен root (fanotify и proc connector)")
	}
	cfg := track.Config{
		Roots: split(*roots), Containers: split(*containers), StateDir: *stateDir, RunDir: *runDir,
		Full: *full, Young: *young, YoungEvery: *youngEvery, Persist: *persist, RotateBytes: *rotateMB << 20,
	}
	if len(cfg.Roots) == 0 {
		return errors.New("пустой -roots")
	}
	return track.Run(cfg, lg)
}

func split(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.TrimRight(p, "/"))
		}
	}
	return out
}

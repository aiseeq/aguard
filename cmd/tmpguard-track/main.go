// tmpguard-track — root-часть сторожа временных файлов (system-сервис).
// Записывает, кто создал каждую запись верхнего уровня в /tmp и
// /tmp/claude-tmp, и снимает их размеры. Сообщений никому не шлёт.
package main

import (
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"tmpguard/internal/record"
	"tmpguard/internal/track"
)

func main() {
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
	log.SetFlags(0)
	if os.Geteuid() != 0 {
		log.Fatal("tmpguard-track: нужен root (fanotify и proc connector)")
	}
	cfg := track.Config{
		Roots: split(*roots), Containers: split(*containers), StateDir: *stateDir, RunDir: *runDir,
		Full: *full, Young: *young, YoungEvery: *youngEvery, Persist: *persist, RotateBytes: *rotateMB << 20,
	}
	if len(cfg.Roots) == 0 {
		log.Fatal("tmpguard-track: пустой -roots")
	}
	if err := track.Run(cfg); err != nil {
		log.Fatalf("tmpguard-track: %v", err)
	}
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.TrimRight(p, "/"))
		}
	}
	return out
}

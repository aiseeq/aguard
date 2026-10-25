package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOlder(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{{"2.1.309", "2.1.310", true}, {"2.1.310", "2.1.310", false}, {"2.1.99", "2.1.100", true}, {"2.2", "2.1.999", false}, {"2.1", "2.1.1", true}}
	for _, c := range cases {
		if got, err := older(c.a, c.b); err != nil || got != c.want {
			t.Errorf("older(%s, %s) = %v, %v", c.a, c.b, got, err)
		}
	}
	if _, err := older("2.1.x", "2.1.1"); err == nil {
		t.Error("не версия — ошибка")
	}
}

func TestRestartArgs(t *testing.T) {
	keep, problem := restartArgs([]string{"--dangerously-skip-permissions", "-n", "lead", "-c", "--model", "opus", "--effort=high", "привет"})
	if problem != "" || !slices.Equal(keep, []string{"--model", "opus", "--effort=high"}) {
		t.Errorf("keep %q problem %q", keep, problem)
	}
	if _, problem := restartArgs([]string{"-r", "--bare"}); !strings.Contains(problem, "--bare") {
		t.Errorf("неизвестный флаг держит перезапуск: %q", problem)
	}
	if keep, problem := restartArgs([]string{"--resume", "abc", "--chrome"}); problem != "" || !slices.Equal(keep, []string{"--chrome"}) {
		t.Errorf("--resume со значением: %q %q", keep, problem)
	}
	// Командная строка сессии, запущенной cl: свои флаги cl не держат перезапуск и не повторяются дважды.
	home := t.TempDir()
	worktrees := filepath.Join(home, ".cache", "worktrees")
	if err := os.MkdirAll(worktrees, 0o755); err != nil {
		t.Fatal(err)
	}
	cmdline := append(sessionFlags(home), "-n", "lead", "--model", "opus", "Ты лид")
	if keep, problem := restartArgs(withoutOwnFlags(cmdline, home)); problem != "" || !slices.Equal(keep, []string{"--model", "opus"}) {
		t.Errorf("свои флаги cl: keep %q problem %q", keep, problem)
	}
	if !bare(nil) || !bare([]string{"-n", "x"}) || !bare([]string{"--name=x"}) || bare([]string{"-c"}) || bare([]string{"привет"}) {
		t.Error("bare")
	}
}

func testConfig() config {
	return config{
		Roles: []role{
			{Names: []string{"lead"}, Text: "Ты лид ({why}).", OnStart: startAlways},
			{Names: []string{"*-master"}, Text: "Мастер ({why}).", OnStart: startResume},
		},
		ResumeText:  "Файл: {path}.",
		RestartNote: "Служебная строка.",
	}
}

func TestRoleLine(t *testing.T) {
	c := testConfig()
	if got := c.roleLine("lead", "старт", "", false); got != "Ты лид (старт)." {
		t.Errorf("лид при старте: %q", got)
	}
	if got := c.roleLine("lead", "перезапуск", "/r/lead.md", true); got != "Ты лид (перезапуск). Файл: /r/lead.md. Служебная строка." {
		t.Errorf("лид после перезапуска: %q", got)
	}
	if got := c.roleLine("sc2go-master", "старт", "", false); got != "" {
		t.Errorf("мастер без файла при старте молчит: %q", got)
	}
	if got := c.roleLine("sc2go-master", "старт", "/r/m.md", false); got != "Мастер (старт). Файл: /r/m.md." {
		t.Errorf("мастер с файлом при старте: %q", got)
	}
	if got := c.roleLine("sc2go-master", "перезапуск", "", true); got != "Мастер (перезапуск). Служебная строка." {
		t.Errorf("мастер после перезапуска: %q", got)
	}
	if got := c.roleLine("lead-2", "старт", "", false); got != "" {
		t.Errorf("второй сессии в ~ роли лида нет: %q", got)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	cfg, err := loadConfig(file, "/home/u")
	if err != nil || cfg.Restart.Mode != modeLog || cfg.Restart.idle() != 10*time.Minute {
		t.Errorf("без файла — журнал без действий: %+v %v", cfg.Restart, err)
	}
	if err := os.WriteFile(file, []byte(`{"restart":{"mode":"act","journal":"~/j.log"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(file, "/home/u"); err != nil || cfg.Restart.Mode != modeAct || cfg.Restart.Journal != "/home/u/j.log" || cfg.Restart.IdleMinutes != 10 {
		t.Errorf("act поверх умолчаний: %+v %v", cfg.Restart, err)
	}
	if err := os.WriteFile(file, []byte(`{"restart":{"mode":"yes"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(file, "/home/u"); err == nil || cfg.Restart.Mode != modeLog {
		t.Errorf("неизвестный режим — ошибка и журнал: %+v %v", cfg.Restart, err)
	}
	if err := os.WriteFile(file, []byte(`{"restart":{"mode":"log","compact_mode":"yes"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(file, "/home/u"); err == nil || cfg.Restart.CompactMode != modeLog {
		t.Errorf("неизвестный режим сжатия — ошибка и журнал: %+v %v", cfg.Restart, err)
	}
}

// Конфиг репо (его ставят в ~/.config/cl) читается и даёт лиду строку роли с восстановимыми Сном и Дайджестом.
func TestRepoConfig(t *testing.T) {
	cfg, err := loadConfig("config.json", "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := cfg.roleFor("lead")
	if !ok || !r.restorable("Сон (скилл sleep)") || !r.restorable("Дайджест (скилл digest)") || r.restorable("замер") {
		t.Errorf("роль лида: %+v", r)
	}
	if line := cfg.roleLine("demu-master", "перезапуск cl на Claude Code 2.1.310", "", true); !strings.Contains(line, "продолжай по своей роли") {
		t.Errorf("мастер: %q", line)
	}
}

func TestFreshResume(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for _, d := range []string{"2026-10-22", "2026-10-23"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "2026-10-23", "lead.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := freshResume(dir, "lead", 24*time.Hour, now); err != nil || got != file {
		t.Errorf("свежий файл: %q", got)
	}
	if got, err := freshResume(dir, "lead", 24*time.Hour, now.Add(25*time.Hour)); err != nil || got != "" {
		t.Errorf("старше суток: %q", got)
	}
	if got, err := freshResume(dir, "git-admin", 24*time.Hour, now); err != nil || got != "" {
		t.Errorf("без файла: %q", got)
	}
}

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

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
		ResumeText: "Файл: {path}.",
		ResetNote:  "Беседа новая.",
	}
}

func TestRoleLine(t *testing.T) {
	c := testConfig()
	if got := c.roleLine("lead", "старт", "", eventStart); got != "Ты лид (старт)." {
		t.Errorf("лид при старте: %q", got)
	}
	if got := c.roleLine("lead", "сброс", "/r/lead.md", eventReset); got != "Ты лид (сброс). Файл: /r/lead.md. Беседа новая." {
		t.Errorf("лид после сброса: %q", got)
	}
	if got := c.roleLine("sc2go-master", "старт", "", eventStart); got != "" {
		t.Errorf("мастер без файла при старте молчит: %q", got)
	}
	if got := c.roleLine("sc2go-master", "старт", "/r/m.md", eventStart); got != "Мастер (старт). Файл: /r/m.md." {
		t.Errorf("мастер с файлом при старте: %q", got)
	}

	if got := c.roleLine("sc2go-master", "сброс", "", eventReset); got != "Мастер (сброс). Беседа новая." {
		t.Errorf("мастер после сброса (без файла — всё равно строка роли): %q", got)
	}
	if got := c.roleLine("lead-2", "старт", "", eventStart); got != "" {
		t.Errorf("второй сессии в ~ роли лида нет: %q", got)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	cfg, err := loadConfig(file, "/home/u")
	if err != nil || cfg.Restart.Mode != modeLog || cfg.Restart.interval() != time.Minute {
		t.Errorf("без файла — журнал без действий: %+v %v", cfg.Restart, err)
	}
	if err := os.WriteFile(file, []byte(`{"restart":{"mode":"act","journal":"~/j.log"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(file, "/home/u"); err != nil || cfg.Restart.Mode != modeAct || cfg.Restart.Journal != "/home/u/j.log" || cfg.Restart.CheckSeconds != 60 {
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

// Конфиг репо (его ставят в ~/.config/cl) читается и даёт строки ролей лиду и мастерам.
func TestRepoConfig(t *testing.T) {
	cfg, err := loadConfig("config.json", "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if line := cfg.roleLine("lead", "старт cl на Claude Code 2.1.310", "", eventStart); !strings.Contains(line, "Ты лид") {
		t.Errorf("лид: %q", line)
	}
	if line := cfg.roleLine("demu-master", "сброс", "", eventReset); !strings.Contains(line, "продолжай по своей роли") {
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

func TestWhyText(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 48, 11, 0, time.Local)
	if got := whyText(eventStart, "2.1.311", at); got != "старт cl на Claude Code 2.1.311" {
		t.Errorf("старт: %q", got)
	}
	if got := whyText(eventReset, "2.1.311", at.UTC()); got != "cl начал новую беседу в 02:48 вместо простаивавшей; Claude Code 2.1.311" {
		t.Errorf("сброс (время местное): %q", got)
	}
}

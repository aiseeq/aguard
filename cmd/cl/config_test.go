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
			{Names: []string{"lead"}, Text: "Ты лид ({why}).", OnStart: startAlways, Prompt: "Ты {name}."},
			{Names: []string{"*-master"}, Text: "Мастер ({why}).", OnStart: startResume},
		},
		ResumeText: "Файл: {path}.",
	}
}

func TestRoleLine(t *testing.T) {
	c := testConfig()
	if got := c.roleLine("lead", "старт", "", eventStart); got != "Ты лид (старт)." {
		t.Errorf("лид при старте: %q", got)
	}
	if got := c.roleLine("lead", "сброс", "/r/lead.md", eventReset); got != "Ты лид (сброс). Файл: /r/lead.md." {
		t.Errorf("лид после сброса: %q", got)
	}
	if got := c.roleLine("sc2go-master", "старт", "", eventStart); got != "" {
		t.Errorf("мастер без файла при старте молчит: %q", got)
	}
	if got := c.roleLine("sc2go-master", "старт", "/r/m.md", eventStart); got != "Мастер (старт). Файл: /r/m.md." {
		t.Errorf("мастер с файлом при старте: %q", got)
	}

	if got := c.roleLine("lead", "сброс", "", eventReset); got != "" {
		t.Errorf("после сброса без файла напоминать не о чем, даже лиду: %q", got)
	}
	if got := c.systemPrompt("lead"); got != "Ты lead." {
		t.Errorf("промпт роли: %q", got)
	}
	if got := c.systemPrompt("sc2go-master"); got != "" {
		t.Errorf("роль без промпта: %q", got)
	}
	if got := c.roleLine("lead-2", "старт", "", eventStart); got != "" {
		t.Errorf("второй сессии в ~ роли лида нет: %q", got)
	}

	c.Restart.ResumeDir = "/r"
	c.ResumePrompt = "Возобновление — {dir}/<дата>/{name}.md."
	if got := c.systemPrompt("lead"); got != "Ты lead. Возобновление — /r/<дата>/lead.md." {
		t.Errorf("путь возобновления после промпта роли: %q", got)
	}
	if got := c.systemPrompt("sc2go-master"); got != "Возобновление — /r/<дата>/sc2go-master.md." {
		t.Errorf("путь возобновления и у роли без промпта: %q", got)
	}
	if got := c.systemPrompt("lead-2"); got != "" {
		t.Errorf("сессии без роли — ничего: %q", got)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	cfg, err := loadConfig(file, "/home/u")
	if err != nil || cfg.Restart.Mode != modeLog || cfg.Restart.interval() != time.Minute {
		t.Errorf("без файла — журнал без действий: %+v %v", cfg.Restart, err)
	}
	if got := (restartConfig{CheckSeconds: 1}).interval(); got != 10*time.Second {
		t.Errorf("check_seconds 1 — не чаще 10 с: %v", got)
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
	if line := cfg.roleLine("demu-master", "сброс", "/r/demu-master.md", eventReset); !strings.Contains(line, "продолжай по своей роли") {
		t.Errorf("мастер: %q", line)
	}
	if !matchAny(cfg.Restart.LiveUpgradeNames, "demu-master") { // всем с 30.10 (1dae51b)
		t.Errorf("живое обновление cl: %q", cfg.Restart.LiveUpgradeNames)
	}
	// Файлы настроек ролей (тикет cl-role-settings): Artifact только у админов, скиллы ~/work скрыты вне ~/work.
	for _, c := range []struct{ name, dir, want string }{
		{"lead", "/home/u", "/home/u/.config/cl/settings/no-artifact.json"},
		{"shop-master", "/home/u/work/shop", "/home/u/.config/cl/settings/no-artifact.json"},
		{"demu-master", "/home/u/git/demu", "/home/u/.config/cl/settings/git-master.json"},
		{"git-admin", "/home/u/git", "/home/u/.config/cl/settings/git-admin.json"},
		{"work-admin", "/home/u/work", ""},
	} {
		if got := cfg.settingsFile(c.name, c.dir); got != c.want {
			t.Errorf("настройки %s в %s: %q, want %q", c.name, c.dir, got, c.want)
		}
	}
	for _, name := range []string{"lead", "work-admin", "git-admin", "demu-master"} {
		if p := cfg.systemPrompt(name); !strings.Contains(p, "/home/u/.claude/restart/<") || !strings.Contains(p, "/"+name+".md") {
			t.Errorf("промпт %s без пути возобновления: %q", name, p)
		}
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

	// Файл не в последней папке-дате тоже находится; из двух — свежий.
	older := filepath.Join(dir, "2026-10-22", "lead.md")
	if err := os.WriteFile(older, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := freshResume(dir, "lead", 24*time.Hour, now); err != nil || got != older {
		t.Errorf("свежий из двух папок: %q", got)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if got, err := freshResume(dir, "lead", 24*time.Hour, now); err != nil || got != older {
		t.Errorf("файл в непоследней папке: %q", got)
	}
}

func TestWhyText(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 48, 11, 0, time.Local)
	if got := whyText(eventStart, "2.1.311", time.Time{}); got != "старт cl на Claude Code 2.1.311" {
		t.Errorf("старт: %q", got)
	}
	if got := whyText(eventStart, "2.1.311", at.UTC()); got != "cl поднял сессию заново в 02:48 на Claude Code 2.1.311" {
		t.Errorf("подъём --resume при потерянных таймерах — не старт cl (время местное): %q", got)
	}
	if got := whyText(eventReset, "2.1.311", at.UTC()); got != "cl начал новую беседу в 02:48 вместо простаивавшей; Claude Code 2.1.311" {
		t.Errorf("сброс (время местное): %q", got)
	}
}

// Файл настроек роли — по папке сессии: первый вариант, к папкам которого она подходит (сама папка или вложенная),
// вариант без dirs — любая папка; ~ раскрывается; вариант без file — ошибка конфига.
func TestRoleSettings(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.json")
	write(t, file, `{"roles":[{"names":["*-master"],"settings":[{"dirs":["~/work"],"file":"~/s/work.json"},{"file":"/s/git.json"}]},`+
		`{"names":["lead"],"settings":[{"file":"~/s/lead.json"}]},{"names":["work-admin"]}]}`)
	cfg, err := loadConfig(file, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, dir, want string }{
		{"shop-master", "/home/u/work/shop", "/home/u/s/work.json"},
		{"shop-master", "/home/u/work", "/home/u/s/work.json"},
		{"shop-master", "/home/u/work/shop/", "/home/u/s/work.json"},
		{"shop-master", "/home/u/work/", "/home/u/s/work.json"},
		{"shop-master", "/home/u/work/../git/x", "/s/git.json"},
		{"aguard-master", "/home/u/git/aguard", "/s/git.json"},
		{"x-master", "/home/u/workshop", "/s/git.json"}, // не ~/work: только целое имя папки
		{"lead", "/home/u", "/home/u/s/lead.json"},
		{"work-admin", "/home/u/work", ""},
		{"nobody", "/home/u/work", ""},
	} {
		if got := cfg.settingsFile(c.name, c.dir); got != c.want {
			t.Errorf("%s в %s: %q, want %q", c.name, c.dir, got, c.want)
		}
	}
	write(t, file, `{"roles":[{"names":["lead"],"settings":[{"dirs":["~"]}]}]}`)
	if _, err := loadConfig(file, "/home/u"); err == nil || !strings.Contains(err.Error(), "без file") {
		t.Errorf("вариант без file — ошибка конфига: %v", err)
	}
}

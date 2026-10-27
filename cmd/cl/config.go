package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/cllog"
	"github.com/aiseeq/aguard/internal/names"
)

// Режимы перезапуска: modeLog — только журнал «перезапустил бы / не стал бы», modeAct — перезапуск всерьёз.
const (
	modeLog = "log"
	modeAct = "act"
	// modeOff — только для compact_mode: не сжимать и не писать «сжал бы».
	modeOff = "off"
)

// Когда роль говорит строку при обычном старте (при перезапуске cl она говорит всегда).
const (
	startAlways = "always"      // всегда
	startResume = "resume_file" // только когда есть свежий файл возобновления
)

// config — ~/.config/cl/config.json: тексты ролей и настройки перезапуска. Тексты ролей живут только здесь, в коде
// их нет. Файл перечитывается на каждой проверке: режим и тексты меняются без перезапуска консолей.
type config struct {
	Restart restartConfig `json:"restart"`
	// Roles — роли по имени сессии, первая подходящая; сессия без роли строки не получает.
	Roles []role `json:"roles"`
	// ResumeText — добавка к строке роли, когда есть свежий файл возобновления; {path} — путь к нему.
	ResumeText string `json:"resume_text"`
	// ResumePrompt — добавка к системному промпту каждой роли: куда класть файл возобновления, чтобы cl отдал его
	// следующей беседе; {name} — имя сессии, {dir} — restart.resume_dir. Путь знал только скилл work-supervisor, и
	// мастер, которого пользователь просил напрямую, клал файл в scratchpad (shop-master 27.10 14:21 CET, задание лида).
	ResumePrompt string `json:"resume_prompt"`
}

// restartConfig — как cl перезапускает сессию на новую версию Claude Code; когда можно — решает aguard (его
// [restart]: порог простоя, таймеры, которые роль ставит заново).
type restartConfig struct {
	Mode         string `json:"mode"`
	CheckSeconds int    `json:"check_seconds"`
	Journal      string `json:"journal"`
	ResumeDir    string `json:"resume_dir"`
	ResumeHours  int    `json:"resume_max_age_hours"`
	// CompactMode — сжатие беседы по вердикту aguard: log — «сжал бы» в журнал, act — сжать, off — молчать.
	CompactMode string `json:"compact_mode"`
	// CompactNames — в act сжимать только сессии с подходящим именем (path.Match), для пилота; пусто — все.
	CompactNames []string `json:"compact_names"`
	// ResetMode — сброс беседы по вердикту aguard (работа закрыта, простой от часа): log — «сбросил бы» в журнал,
	// act — поднять новую беседу со строкой роли и файлом возобновления, off — молчать.
	ResetMode string `json:"reset_mode"`
	// ResetNames — в act сбрасывать только сессии с подходящим именем, для пилота; пусто — все.
	ResetNames []string `json:"reset_names"`
	// PtyNames — сессии с подходящим именем cl запускает под своим pty (console.go); пусто — ни одной; с 27.10 у нас ["*"] (пилот 27.10 17:00 CET, выкатка на work-admin 18:08 CET).
	PtyNames []string `json:"pty_names"`
	// TypeMode — сжатие и сброс сессии под pty набором /compact и /clear в живой сессии: log — «набрал бы» в журнал
	// (действует прежний путь через перезапуск), act — набрать вместо перезапуска, off — молчать.
	TypeMode string `json:"type_mode"`
}

// role — строка роли для сессий с подходящим именем.
type role struct {
	// Names — шаблоны имени сессии (path.Match: lead, *-master).
	Names []string `json:"names"`
	// Text — строка роли; {why} — «старт cl на Claude Code 2.1.310» или что cl сделал, когда и зачем (whyText).
	Text string `json:"text"`
	// OnStart — always или resume_file; пусто — при обычном старте молчать.
	OnStart string `json:"on_start"`
	// Prompt — кто эта сессия, системным промптом при каждом подъёме (--append-system-prompt); {name} — имя сессии.
	Prompt string `json:"prompt"`
}

// defaultConfig — без файла: только журнал, без строк ролей.
func defaultConfig(home string) config {
	return config{Restart: restartConfig{
		Mode:         modeLog,
		CheckSeconds: 60,
		Journal:      cllog.Path(home),
		ResumeDir:    filepath.Join(home, ".claude", "restart"),
		ResumeHours:  24,
		CompactMode:  modeLog,
		ResetMode:    modeLog,
		TypeMode:     modeLog,
	}}
}

// loadConfig читает конфиг поверх умолчаний. Нет файла — умолчания; битый файл или неизвестный режим — ошибка:
// cl тогда не перезапускает (сомнение — не перезапускать).
func loadConfig(file, home string) (config, error) {
	cfg := defaultConfig(home)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("конфиг cl: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(home), fmt.Errorf("конфиг cl %s: %w", file, err)
	}
	if cfg.Restart.Mode != modeLog && cfg.Restart.Mode != modeAct {
		return defaultConfig(home), fmt.Errorf("конфиг cl %s: restart.mode %q, нужно %q или %q", file, cfg.Restart.Mode, modeLog, modeAct)
	}
	for key, m := range map[string]string{"compact_mode": cfg.Restart.CompactMode, "reset_mode": cfg.Restart.ResetMode,
		"type_mode": cfg.Restart.TypeMode} {
		if m != modeOff && m != modeLog && m != modeAct {
			return defaultConfig(home), fmt.Errorf("конфиг cl %s: restart.%s %q, нужно %q, %q или %q", file, key, m, modeOff, modeLog, modeAct)
		}
	}
	cfg.Restart.Journal = names.Expand(cfg.Restart.Journal, home)
	cfg.Restart.ResumeDir = names.Expand(cfg.Restart.ResumeDir, home)
	return cfg, nil
}

func (c restartConfig) interval() time.Duration {
	return time.Duration(max(c.CheckSeconds, 10)) * time.Second
}

// act — делать ли действие над сессией с этим именем всерьёз: режим act и имя в пилоте (или пилот не задан).
func act(mode string, pilot []string, name string) bool {
	if mode != modeAct {
		return false
	}
	if len(pilot) == 0 {
		return true
	}
	return matchAny(pilot, name)
}

// matchAny — имя подходит к одному из шаблонов path.Match.
func matchAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// roleFor — первая роль, шаблон которой подходит к имени сессии.
func (c config) roleFor(name string) (role, bool) {
	for _, r := range c.Roles {
		if matchAny(r.Names, name) {
			return r, true
		}
	}
	return role{}, false
}

// roleLine — первая реплика сессии при старте и после сброса (event — eventStart или eventReset): строка роли и файл
// возобновления. Пусто — молчать. После сброса — только с файлом возобновления: сбрасывается закрытая работа без
// таймеров, напоминать не о чем, а реплика будит новую беседу впустую (пользователь 27.10 09:50 CET в транскрипте
// лида: «если консоль работает, не надо её рестартить. А если не работает, то не о чем напоминать»; скрин git-admin
// после сброса 13:52 CET — «видишь тут абсурдность ситуации?»). Кто она, беседа знает из Prompt.
func (c config) roleLine(name, why, resume, event string) string {
	r, ok := c.roleFor(name)
	if !ok || r.Text == "" {
		return ""
	}
	switch {
	case event == eventReset && resume == "":
		return ""
	case event == eventStart && r.OnStart != startAlways && (r.OnStart != startResume || resume == ""):
		return ""
	}
	parts := []string{strings.ReplaceAll(r.Text, "{why}", why)}
	if resume != "" && c.ResumeText != "" {
		parts = append(parts, strings.ReplaceAll(c.ResumeText, "{path}", resume))
	}
	return strings.Join(parts, " ")
}

// systemPrompt — Prompt роли сессии name и ResumePrompt; роли нет — пусто.
func (c config) systemPrompt(name string) string {
	r, ok := c.roleFor(name)
	if !ok {
		return ""
	}
	var parts []string
	for _, text := range []string{r.Prompt, c.ResumePrompt} {
		if text != "" {
			parts = append(parts, strings.NewReplacer("{name}", name, "{dir}", c.Restart.ResumeDir).Replace(text))
		}
	}
	return strings.Join(parts, " ")
}

// usedDir — куда cl убирает отданный файл возобновления, в той же папке-дате: файл разовый, и следующий подъём,
// ручной или сбросом, его не получит (пользователь 26.10 11:14 CET в транскрипте лида: «такой файл, по видимому не
// удаляется после поднятия новой консоли, а должен»).
const usedDir = "used"

// takeResume убирает отданный файл возобновления в usedDir рядом и возвращает его новый путь.
func takeResume(file string) (string, error) {
	used := filepath.Join(filepath.Dir(file), usedDir, filepath.Base(file))
	if err := os.MkdirAll(filepath.Dir(used), 0o755); err != nil {
		return "", fmt.Errorf("файл возобновления: %w", err)
	}
	if err := os.Rename(file, used); err != nil {
		return "", fmt.Errorf("файл возобновления: %w", err)
	}
	return used, nil
}

// freshResume — самый свежий файл возобновления <имя>.md в папках-датах ~/.claude/restart, не старше maxAge: не только
// в последней папке — сессия могла взять дату со своих часов, а папку следующего дня уже завела другая. Нет папки,
// файла или он старый — пусто без ошибки.
func freshResume(dir, name string, maxAge time.Duration, now time.Time) (string, error) {
	if name == "" {
		return "", nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("файлы возобновления: %w", err)
	}
	found, newest := "", time.Time{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		file := filepath.Join(dir, e.Name(), name+".md")
		info, err := os.Stat(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("файл возобновления: %w", err)
		}
		if now.Sub(info.ModTime()) <= maxAge && info.ModTime().After(newest) {
			found, newest = file, info.ModTime()
		}
	}
	return found, nil
}

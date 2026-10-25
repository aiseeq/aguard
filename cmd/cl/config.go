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
	// RestartNote — добавка к строке роли после перезапуска cl: беседа сохранена, строка не ответ пользователя.
	RestartNote string `json:"restart_note"`
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
}

// role — строка роли для сессий с подходящим именем.
type role struct {
	// Names — шаблоны имени сессии (path.Match: lead, *-master).
	Names []string `json:"names"`
	// Text — строка роли; {why} — «перезапуск cl на Claude Code 2.1.310» или «старт cl на Claude Code 2.1.310».
	Text string `json:"text"`
	// OnStart — always или resume_file; пусто — при обычном старте молчать.
	OnStart string `json:"on_start"`
}

// defaultConfig — без файла: только журнал, без строк ролей.
func defaultConfig(home string) config {
	return config{Restart: restartConfig{
		Mode:         modeLog,
		CheckSeconds: 60,
		Journal:      filepath.Join(home, ".local", "state", "cl", "restart.log"),
		ResumeDir:    filepath.Join(home, ".claude", "restart"),
		ResumeHours:  24,
		CompactMode:  modeLog,
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
	if m := cfg.Restart.CompactMode; m != modeOff && m != modeLog && m != modeAct {
		return defaultConfig(home), fmt.Errorf("конфиг cl %s: restart.compact_mode %q, нужно %q, %q или %q", file, m, modeOff, modeLog, modeAct)
	}
	cfg.Restart.Journal = expand(cfg.Restart.Journal, home)
	cfg.Restart.ResumeDir = expand(cfg.Restart.ResumeDir, home)
	return cfg, nil
}

func (c restartConfig) interval() time.Duration {
	return time.Duration(max(c.CheckSeconds, 10)) * time.Second
}

// compactAct — сжимать ли сессию с этим именем всерьёз: режим act и имя в пилоте (или пилот не задан).
func (c restartConfig) compactAct(name string) bool {
	if c.CompactMode != modeAct {
		return false
	}
	if len(c.CompactNames) == 0 {
		return true
	}
	return matchAny(c.CompactNames, name)
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

// roleLine — первая реплика сессии: строка роли, файл возобновления, после перезапуска — пометка cl. Пусто — молчать.
func (c config) roleLine(name, why, resume string, restart bool) string {
	r, ok := c.roleFor(name)
	if !ok || r.Text == "" {
		return ""
	}
	if !restart && r.OnStart != startAlways && (r.OnStart != startResume || resume == "") {
		return ""
	}
	parts := []string{strings.ReplaceAll(r.Text, "{why}", why)}
	if resume != "" && c.ResumeText != "" {
		parts = append(parts, strings.ReplaceAll(c.ResumeText, "{path}", resume))
	}
	if restart && c.RestartNote != "" {
		parts = append(parts, c.RestartNote)
	}
	return strings.Join(parts, " ")
}

// freshResume — файл возобновления <имя>.md в последней папке-дате ~/.claude/restart, не старше maxAge. Нет папки,
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
	latest := ""
	for _, e := range entries {
		if e.IsDir() && e.Name() > latest {
			latest = e.Name()
		}
	}
	if latest == "" {
		return "", nil
	}
	file := filepath.Join(dir, latest, name+".md")
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("файл возобновления: %w", err)
	}
	if now.Sub(info.ModTime()) > maxAge {
		return "", nil
	}
	return file, nil
}

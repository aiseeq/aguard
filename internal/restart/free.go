package restart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func free(f facts, after time.Duration) (bool, string) {
	if len(f.Errors) > 0 {
		return false, "ошибка чтения фактов"
	}
	if f.Kind != "interactive" {
		return false, "сессия не интерактивная"
	}
	if f.HookState == "waiting" || f.HookState == "error" {
		return false, "ход ждёт человека или завершился ошибкой"
	}
	if f.HookState == "working" || f.Status == "busy" {
		if len(f.Transcript.Pending) > 0 && f.Transcript.PendingWaits == len(f.Transcript.Pending) && f.TurnQuiet >= after && f.TTYQuiet >= after {
			return true, "ожидание команды"
		}
		return false, "идёт ход"
	}
	if f.HookState == "" && !f.Transcript.Context.Request.IsZero() {
		return false, "нет состояния хука"
	}
	if f.HookState != "" && f.HookState != "done" && f.HookState != "idle" && f.HookState != "paused" {
		return false, "неизвестное состояние хука"
	}
	if len(f.Transcript.Pending) > 0 {
		return false, "инструмент без результата"
	}
	if f.TurnQuiet < after || f.TTYQuiet < after {
		return false, "сессия недавно работала"
	}
	return true, "свободна"
}

// Free собирает только признаки простоя, без экрана и списка потомков.
func (j *Judge) Free(cfg *Config, pid int, after time.Duration, now time.Time) (bool, string) {
	if pid <= 0 {
		return false, "неверный pid"
	}
	c := j.cache(pid, now)
	c.mu.Lock()
	defer c.mu.Unlock()
	p := j.pathsOf(cfg)
	var f facts
	s, err := readSession(p.sessions, pid)
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
		return free(f, after)
	}
	f.Info = s
	h, err := readHookState(p, s.SessionID)
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
	}
	f.HookState, f.HookTasks, f.HookWaits, f.HookAgents = h.state, h.tasks, h.waits, h.agents
	file, info, err := transcriptFile(p, s.SessionID)
	if errors.Is(err, os.ErrNotExist) {
		f.TurnQuiet = now.Sub(time.UnixMilli(s.StartedAt))
	} else if err != nil {
		f.Errors = append(f.Errors, err.Error())
	} else {
		f.TurnQuiet = now.Sub(info.ModTime())
		f.Transcript, err = c.scan.read(file, info, time.UnixMilli(s.StartedAt), now)
		if err != nil {
			f.Errors = append(f.Errors, err.Error())
		}
	}
	f.TTYQuiet, err = ttyQuiet(p, pid, now)
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
	}
	return free(f, after)
}

// WakeFree проверяет простой и задачи, оставшиеся на последнем Stop.
func (j *Judge) WakeFree(cfg *Config, pid int, after time.Duration, now time.Time) (bool, error) {
	ok, reason := j.Free(cfg, pid, after, now)
	if !ok || reason == "ожидание команды" {
		return false, nil
	}
	p := j.pathsOf(cfg)
	s, err := readSession(p.sessions, pid)
	if err != nil {
		return false, err
	}
	hook, err := readHookState(p, s.SessionID)
	if err != nil {
		return false, err
	}
	return !hook.tasks, nil // суб-агент — тоже запись в задачах
}

// transcriptFile ищет файл беседы в любой папке проектов: имя папки Claude Code выводит из cwd сам.
// Нет файла — os.ErrNotExist, отличимый от ошибки обхода папок. Сведения о файле — того же stat, что нашёл его:
// второй stat открывал бы окно, где файл уже другой.
func transcriptFile(p paths, sessionID string) (string, os.FileInfo, error) {
	entries, err := os.ReadDir(p.projects)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, os.ErrNotExist
	}
	if err != nil {
		return "", nil, fmt.Errorf("папки транскриптов: %w", err)
	}
	var found string
	var foundInfo os.FileInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(p.projects, entry.Name(), filepath.Base(sessionID)+".jsonl")
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("транскрипт: %w", err)
		}
		if found != "" {
			return "", nil, fmt.Errorf("несколько транскриптов сессии %s", sessionID)
		}
		found, foundInfo = path, info
	}
	if found == "" {
		return "", nil, os.ErrNotExist
	}
	return found, foundInfo, nil
}

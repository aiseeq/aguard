package restart

import (
	"fmt"
	"os"
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
	file, err := transcriptFile(p, s.SessionID)
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
		return free(f, after)
	}
	info, err := os.Stat(file)
	if err != nil {
		f.Errors = append(f.Errors, fmt.Sprintf("транскрипт: %v", err))
		return free(f, after)
	}
	f.TurnQuiet = now.Sub(info.ModTime())
	f.Transcript, err = c.scan.read(file, info, time.UnixMilli(s.StartedAt), now)
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
	}
	f.TTYQuiet, err = ttyQuiet(p, pid, now)
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
	}
	return free(f, after)
}

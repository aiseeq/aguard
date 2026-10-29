package restart

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/aiseeq/aguard/internal/linesock"
)

// Version — версия протокола вопроса cl и ответа aguard. Другая версия у cl — то же, что нет ответа: не перезапускать.
const Version = 1

// Request — вопрос cl о своей сессии: одна строка JSON.
type Request struct {
	V   int `json:"v"`
	Pid int `json:"pid"` // процесс claude
	// Name — имя сессии у cl: по нему ищется роль, пока реестр Claude Code имени не знает.
	Name string `json:"name"`
	// ArgsProblem — почему перезапуск потерял бы аргументы запуска; пусто — восстановимы. Аргументы знает только cl.
	ArgsProblem string `json:"args_problem"`
	// Typed — сжатие и сброс cl наберёт в живой сессии под pty (type_mode act): /clear не теряет таймеры CronCreate.
	Typed bool `json:"typed,omitempty"`
}

// NewRequest — вопрос текущей версии протокола о процессе pid.
func NewRequest(pid int, name, argsProblem string) Request {
	return Request{V: Version, Pid: pid, Name: name, ArgsProblem: argsProblem}
}

// Answer — вердикт и то, что нужно cl для журнала и перезапуска. Error — вопрос не разобран: вердикта нет.
type Answer struct {
	V int `json:"v"`
	Verdict
	Key       string `json:"key"`
	Mode      string `json:"mode"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Running   string `json:"running"`
	Installed string `json:"installed"`
	// Crons — у сессии живые таймеры CronCreate.
	Crons bool `json:"crons"`
	// Compact — сжать беседу сейчас: cl останавливает claude, `claude -p --resume <сессия> /compact`, поднимает
	// снова. CompactAgainst — почему нет, CompactKey — решение без чисел, Tokens — контекст последнего ответа.
	Compact        bool     `json:"compact"`
	CompactAgainst []Reason `json:"compact_against"`
	CompactKey     string   `json:"compact_key"`
	Tokens         int64    `json:"tokens"`
	// Reset — сбросить беседу: cl останавливает claude и поднимает новую беседу со строкой роли и файлом
	// возобновления. ResetAgainst — почему нет, ResetKey — решение без чисел.
	Reset        bool     `json:"reset"`
	ResetAgainst []Reason `json:"reset_against"`
	ResetKey     string   `json:"reset_key"`
	Error        string   `json:"error,omitempty"`
}

// cacheTTL — сколько держать разбор транскрипта сессии, о которой не спрашивают: cl спрашивает раз в минуту.
const cacheTTL = 10 * time.Minute

// Judge отвечает на вопросы: факты о сессии и решение. Разбор транскрипта каждой сессии кэшируется между вопросами.
type Judge struct {
	Home   string
	mu     sync.Mutex
	caches map[int]*cached
}

type cached struct {
	mu   sync.Mutex // один вопрос по сессии за раз: кэш разбора не потокобезопасен
	scan scanCache
	used time.Time
}

// NewJudge — судья для пользователя с домашней папкой home.
func NewJudge(home string) *Judge { return &Judge{Home: home, caches: map[int]*cached{}} }

// cache — кэш сессии pid; заодно выбрасывает давно не спрошенные.
func (j *Judge) cache(pid int, now time.Time) *cached {
	j.mu.Lock()
	defer j.mu.Unlock()
	for p, c := range j.caches {
		if now.Sub(c.used) > cacheTTL {
			delete(j.caches, p)
		}
	}
	c := j.caches[pid]
	if c == nil {
		c = &cached{}
		j.caches[pid] = c
	}
	c.used = now
	return c
}

// Answer — вердикты перезапуска и сжатия по вопросу cl при разделах конфига cfg и cc.
func (j *Judge) Answer(cfg *Config, cc *CompactConfig, req Request, now time.Time) Answer {
	if req.V != Version {
		return Answer{V: Version, Error: fmt.Sprintf("версия протокола %d, aguard знает %d", req.V, Version)}
	}
	if req.Pid <= 0 {
		return Answer{V: Version, Error: fmt.Sprintf("pid %d", req.Pid)}
	}
	c := j.cache(req.Pid, now)
	c.mu.Lock()
	f := probe(pathsFor(j.Home, cfg), &c.scan, req.Pid, now)
	c.mu.Unlock()
	name := firstNonEmpty(f.Name, req.Name)
	v := decide(f, allowance{idle: cfg.Idle.D(), argsProblem: req.ArgsProblem})
	cv := j.Compact(cfg, cc, Request{Pid: req.Pid, Name: name, ArgsProblem: req.ArgsProblem, Typed: req.Typed}, now)
	return Answer{
		V: Version, Verdict: v, Key: v.Key(), Mode: cfg.Mode,
		SessionID: f.SessionID, Name: name, Running: f.Running, Installed: f.Installed, Crons: len(f.Transcript.Crons) > 0,
		Compact: cv.Compact, CompactAgainst: cv.Against, CompactKey: cv.Key(), Tokens: cv.Tokens,
		Reset: cv.Reset.Reset, ResetAgainst: cv.Reset.Against, ResetKey: cv.Reset.Key(),
	}
}

// Serve отвечает на вопросы cl до отмены ctx (сокет — linesock.Listen). answer получает разобранный вопрос;
// ошибки соединений — в журнал.
func Serve(ctx context.Context, ln net.Listener, answer func(Request) Answer, lg *slog.Logger) error {
	return linesock.Serve(ctx, ln, func(line []byte) any {
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return Answer{V: Version, Error: "вопрос не JSON: " + err.Error()}
		}
		return answer(req)
	}, lg, "aguard restart")
}

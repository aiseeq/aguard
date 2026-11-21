package restart

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/linesock"
	"github.com/aiseeq/aguard/internal/screen"
	"github.com/aiseeq/aguard/internal/statefile"
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
	// Full — cl --cl-check просит проверить git-дерево, даже если другой довод уже запрещает сброс.
	Full bool `json:"full,omitempty"`
	// Clean — cl под pty видел весь ввод человека: после запуска claude или последней отправки (Enter, Ctrl+C) текста
	// он не набирал. Текст в поле тогда — подсказка Claude Code, не черновик (случай chooks-master 30.10: у сессий,
	// которые ведут письма, реплик человека в транскрипте нет, и признак по atime pts не срабатывал).
	Clean bool `json:"clean,omitempty"`
	// Stale — чем живой процесс устарел, кроме версии: аргументы запуска расходятся с положенными по роли («файл
	// настроек роли: нет → /x.json»; знает только cl). Не пусто — перезапуск в простое и при свежей версии.
	Stale string `json:"stale,omitempty"`
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
	// FieldEmpty — поле ввода по экрану пусто: cl считает его чистым, если человек ничего не вводил с вопроса (так
	// чистеет и поле, о котором прежний cl при подхвате на ходу не знал).
	FieldEmpty bool `json:"field_empty,omitempty"`
	// Stale — Request.Stale, по которому решалось: для журнала cl.
	Stale string `json:"stale,omitempty"`
	Error string `json:"error,omitempty"`
}

// cacheTTL — сколько держать разбор транскрипта сессии, о которой не спрашивают: cl спрашивает раз в минуту.
const cacheTTL = 10 * time.Minute

// Judge отвечает на вопросы: факты о сессии и решение. Разбор транскрипта каждой сессии кэшируется между вопросами.
type Judge struct {
	Home   string
	mu     sync.Mutex
	caches map[int]*cached
	// paths — пути вместо живой машины (подменённая сессия в тесте); nil — pathsFor.
	paths     *paths
	statePath string
}

// scanCacheVersion — версия сохранённого разбора транскриптов: другая версия в файле — разбор с нуля.
// scanCacheLogicSHA256 — хеш файлов, от которых зависит состояние разбора (сейчас transcript.go): сторож
// TestScanCacheLogicFingerprint краснеет при их правке, чтобы версию подняли вместе с логикой разбора.
const scanCacheVersion = 3
const scanCacheLogicSHA256 = "94882aa2eed6f01f2713f4e65590ab0336cb5e990947afa2eed35ef45a5c9ed7"

type savedScanCache struct {
	Version int                 `json:"version"`
	Caches  map[int]*savedCache `json:"caches"`
}

type savedCache struct {
	Used        time.Time           `json:"used"`
	PathSession string              `json:"path_session"`
	Path        string              `json:"path,omitempty"`
	File        string              `json:"file"`
	Size        int64               `json:"size"`
	Mtime       time.Time           `json:"mtime"`
	Since       time.Time           `json:"since"`
	Offset      int64               `json:"offset"`
	Inode       uint64              `json:"inode"`
	PrefixHash  [sha256.Size]byte   `json:"prefix_hash"`
	Scan        transcriptScanState `json:"scan"`
}

// LoadScanCache restores only entries whose transcript still matches the saved prefix.
func (j *Judge) LoadScanCache(path string, now time.Time) error {
	j.statePath = path
	b, err := statefile.Load(path)
	if err != nil {
		if statefile.IsCorrupt(err) {
			if resetErr := statefile.Reset(path); resetErr != nil {
				return errors.Join(fmt.Errorf("discard corrupt transcript cache: %w", err), resetErr)
			}
			return nil
		}
		return err
	}
	saved, ok, decodeErr := decodeSavedScanCache(b)
	if decodeErr != nil {
		if resetErr := statefile.Reset(path); resetErr != nil {
			return errors.Join(fmt.Errorf("discard invalid transcript cache: %w", decodeErr), resetErr)
		}
		return nil
	}
	if !ok {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	var restoreErr error
	for pid, entry := range saved.Caches {
		if pid <= 0 {
			continue
		}
		restored, ok, err := restoreSavedCache(entry, now)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore transcript cache for pid %d: %w", pid, err))
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if ok {
			j.caches[pid] = restored
		}
	}
	return restoreErr
}

func decodeSavedScanCache(b []byte) (savedScanCache, bool, error) {
	var saved savedScanCache
	if err := json.Unmarshal(b, &saved); err != nil {
		return savedScanCache{}, false, fmt.Errorf("decode transcript scan cache: %w", err)
	}
	if saved.Version != scanCacheVersion {
		return savedScanCache{}, false, nil
	}
	return saved, true, nil
}

func restoreSavedCache(entry *savedCache, now time.Time) (*cached, bool, error) {
	if !validSavedCache(entry, now) {
		return nil, false, nil
	}
	stamp, err := statefile.FileStampOf(entry.File)
	if err != nil {
		return nil, false, fmt.Errorf("stat transcript %s: %w", entry.File, err)
	}
	if !stamp.Exists {
		return nil, false, os.ErrNotExist
	}
	if stamp.Size < entry.Size || stamp.Mtime < entry.Mtime.UnixNano() {
		return nil, false, nil
	}
	inode, prefixHash, err := transcriptPrefix(entry.File, entry.Offset)
	if err != nil {
		return nil, false, fmt.Errorf("read transcript prefix %s: %w", entry.File, err)
	}
	if inode != entry.Inode || prefixHash != entry.PrefixHash {
		return nil, false, nil
	}
	scan := scanFromState(entry.Scan)
	if scan.uses == nil || scan.pending == nil || scan.tasks == nil || scan.agents == nil || scan.closed == nil || scan.expires == nil || scan.crons == nil || scan.oldCronDeletes == nil || scan.fired == nil {
		return nil, false, nil
	}
	return &cached{used: entry.Used, scan: scanCache{pathSession: entry.PathSession, path: entry.Path, file: entry.File,
		size: entry.Size, mtime: entry.Mtime, since: entry.Since, offset: entry.Offset, inode: entry.Inode,
		prefixHash: entry.PrefixHash, scan: scan}}, true, nil
}

func validSavedCache(entry *savedCache, now time.Time) bool {
	return entry != nil && now.Sub(entry.Used) <= cacheTTL && entry.File != "" && entry.Offset >= 0 &&
		entry.Offset <= entry.Size && !entry.Scan.Since.IsZero()
}

// SaveScanCache writes a compact snapshot of live in-memory entries through statefile.
func (j *Judge) SaveScanCache(now time.Time) error {
	if j.statePath == "" {
		return nil
	}
	saved := savedScanCache{Version: scanCacheVersion, Caches: map[int]*savedCache{}}
	type cacheRef struct {
		pid  int
		c    *cached
		used time.Time
	}
	refs := make([]cacheRef, 0)
	j.mu.Lock()
	for pid, c := range j.caches {
		refs = append(refs, cacheRef{pid: pid, c: c, used: c.used})
	}
	j.mu.Unlock()
	for _, ref := range refs {
		if !ref.c.mu.TryLock() {
			continue
		}
		entry, err := snapshotCached(ref.c, ref.used, now)
		if err != nil {
			ref.c.mu.Unlock()
			return fmt.Errorf("кеш разбора сессии %d: %w", ref.pid, err)
		}
		if entry != nil {
			saved.Caches[ref.pid] = entry
		}
		ref.c.mu.Unlock()
	}
	b, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("encode transcript scan cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(j.statePath), 0o700); err != nil {
		return fmt.Errorf("create transcript scan cache directory: %w", err)
	}
	return statefile.Save(j.statePath, b)
}

func snapshotCached(c *cached, used, now time.Time) (*savedCache, error) {
	if now.Sub(used) > cacheTTL || c.scan.scan == nil {
		return nil, nil
	}
	s := c.scan
	stateBytes, err := json.Marshal(s.scan.state())
	if err != nil {
		return nil, fmt.Errorf("encode scan state: %w", err)
	}
	var scanState transcriptScanState
	if err := json.Unmarshal(stateBytes, &scanState); err != nil {
		return nil, fmt.Errorf("copy scan state: %w", err)
	}
	return &savedCache{Used: used, PathSession: s.pathSession, Path: s.path, File: s.file,
		Size: s.size, Mtime: s.mtime, Since: s.since, Offset: s.offset, Inode: s.inode,
		PrefixHash: s.prefixHash, Scan: scanState}, nil
}

// transcriptPrefix — inode открытого файла и sha256 его последних ≤ 4 КБ перед offset: оба про один и тот же
// открытый файл, так что подмена между stat и чтением не смешает inode одного файла с хвостом другого.
func transcriptPrefix(path string, offset int64) (uint64, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return 0, digest, fmt.Errorf("open transcript for fingerprint: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return 0, digest, fmt.Errorf("stat opened transcript: %w", err)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, digest, fmt.Errorf("stat opened transcript: нет inode")
	}
	if opened.Size() < offset {
		return 0, digest, fmt.Errorf("transcript shorter than offset %d: %d", offset, opened.Size())
	}
	length := min(offset, int64(4<<10))
	if length == 0 {
		return stat.Ino, sha256.Sum256(nil), nil
	}
	b := make([]byte, length)
	if _, err := f.ReadAt(b, offset-length); err != nil {
		return 0, digest, fmt.Errorf("read transcript prefix: %w", err)
	}
	return stat.Ino, sha256.Sum256(b), nil
}

// pathsOf — где смотреть сессию: живая машина или подмена теста.
func (j *Judge) pathsOf(cfg *Config) paths {
	if j.paths != nil {
		return *j.paths
	}
	return pathsFor(j.Home, cfg)
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
	children := &descendantsCache{}
	c.mu.Lock()
	f := probeWithChildren(j.pathsOf(cfg), &c.scan, req.Pid, now, req.Stale != "", children)
	c.mu.Unlock()
	f.cleanField(req.Clean)
	name := firstNonEmpty(f.Name, req.Name)
	v := decide(f, allowance{idle: cfg.Idle.D(), argsProblem: req.ArgsProblem, typed: req.Typed, stale: req.Stale})
	cv := j.compactWithChildren(cfg, cc, Request{Pid: req.Pid, Name: name, ArgsProblem: req.ArgsProblem, Typed: req.Typed, Clean: req.Clean, Full: req.Full}, now, children)
	if cv.Compact {
		v.Lost = cv.Lost
	}
	return Answer{
		V: Version, Verdict: v, Key: v.Key(), Mode: cfg.Mode,
		SessionID: f.SessionID, Name: name, Running: f.Running, Installed: f.Installed, Crons: len(f.Transcript.Crons) > 0,
		Compact: cv.Compact, CompactAgainst: cv.Against, CompactKey: cv.Key(), Tokens: cv.Tokens,
		Reset: cv.Reset.Reset, ResetAgainst: cv.Reset.Against, ResetKey: cv.Reset.Key(),
		FieldEmpty: f.PromptErr == "" && f.Prompt == screen.Empty, Stale: req.Stale,
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

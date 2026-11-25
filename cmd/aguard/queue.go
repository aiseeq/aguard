package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/daily"
	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/aguard/internal/orphans"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/akit/queuegate"
	"github.com/aiseeq/akit/session"
)

type queueTicketJSON struct {
	Repo     string `json:"repo"`
	Prio     string `json:"prio"`
	Back     string `json:"back"`
	Status   string `json:"status"`
	Path     string `json:"path"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Executor string `json:"executor"`
	Journal  bool   `json:"journal"`
	// Actors — время последней записи журнала по авторам, «2006-01-02 15:04» местного (akit tk v1.47.0).
	Actors map[string]string `json:"actors"`
	// BlockedBy — тикеты, поставленные перед этим (tk set --blocks): slug той же папки или git|work/<проект>/<slug>.
	BlockedBy []string `json:"blocked_by"`
	Overdue   bool     `json:"overdue"`
	Due       string   `json:"due"` // срок «ждёт», если tk его разобрал
}

func (a *app) queueCmd(ctx context.Context, args []string) error {
	cmd, rest := sub(args)
	cfg, err := a.config()
	if err != nil {
		return err
	}
	switch cmd {
	case "tick":
		fl := flag.NewFlagSet("queue tick", flag.ContinueOnError)
		dry := fl.Bool("dry-run", false, "показать ворота и уведомления без записи")
		parse(fl, rest)
		return a.queueRound(ctx, &cfg, restart.NewJudge(a.home), *dry)
	case "report":
		fl := flag.NewFlagSet("queue report", flag.ContinueOnError)
		since := fl.Duration("since", 168*time.Hour, "глубина отчёта")
		asJSON := fl.Bool("json", false, "вывод JSON")
		parse(fl, rest)
		return a.queueReport(*since, *asJSON, time.Now())
	default:
		return fmt.Errorf("неизвестная команда queue %q", cmd)
	}
}

type queueTicketCache struct {
	Tickets     []queue.Ticket
	Fingerprint string
	ScannedAt   time.Time
	OwnersKey   string
	Loaded      bool
	Projects    map[string]queueProjectCache
}

type queueProjectCache struct {
	Fingerprint string
	ScannedAt   time.Time
	Tickets     []queue.Ticket
}

// queueListError — tk ls не дал список: упал или ответил не JSON. Остальные ошибки прохода (таблица имён) — не про список.
type queueListError struct{ err error }

func (e queueListError) Error() string { return e.err.Error() }
func (e queueListError) Unwrap() error { return e.err }

func (a *app) queueRound(ctx context.Context, c *config.Config, judge *restart.Judge, dry bool) error {
	if c.Queue.Mode == "off" {
		return nil
	}
	now := time.Now()
	live, err := queueLive(&c.Queue)
	if err != nil {
		return err
	}
	rc := *c
	rc.Queue.Live = live
	c = &rc
	if _, err := a.queueGatePass(c, now, dry); err != nil {
		return err
	}
	return a.queueNotices(ctx, c, judge, dry, now)
}

// queueLive — список live прохода. «*» в нём — все проекты с папкой задач под git_roots и dirs, кроме skip: новый проект
// идёт в боевой режим и ворота tk next сам (пользователь 04.11 «все агенты», queue-live-allowlist). Остальные имена списка
// добавляются как есть: имена, под которыми tk видит проект вне корней (папка лида), находом не выводятся.
func queueLive(cfg *queue.Config) ([]string, error) {
	if !slices.Contains(cfg.Live, queue.LiveAll) {
		return slices.Clone(cfg.Live), nil
	}
	dirs, err := queueFingerprintDirs(cfg)
	if err != nil {
		return nil, fmt.Errorf("queue.live «*»: %w", err)
	}
	var out []string
	for _, name := range cfg.Live {
		if name != queue.LiveAll {
			out = append(out, name)
		}
	}
	for _, tasks := range dirs {
		info, err := os.Stat(tasks)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) { // нет папки или docs — файл: не проект
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("queue.live «*»: %w", err)
		}
		if info.IsDir() {
			out = append(out, filepath.Base(queueTasksProject(tasks, cfg.Dirs)))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// queueGatePass пишет gate.json по слову пользователя (queue.level и queue.spaces) и возвращает то, что увидит tk next:
// файл не записан — прошлый. Запас, таблица и поправки ворот не читаются. Сухой прогон файл не пишет и показывает расчёт.
func (a *app) queueGatePass(c *config.Config, now time.Time, dry bool) (queue.Gate, error) {
	gate := queue.UserGate(c.Queue.Level, c.Queue.Spaces, c.Queue.Rest(), now)
	if err := a.writeQueueGate(&c.Queue, now, gate, dry); err != nil {
		return queue.Gate{}, err
	}
	if dry {
		return gate, nil
	}
	return a.previousQueueGate(c.Queue.Gate, now), nil
}

func (a *app) writeQueueGate(cfg *queue.Config, now time.Time, gate queue.Gate, dry bool) error {
	live := slices.Clone(cfg.Live)
	if cfg.Mode == queue.ModeLog {
		live = []string{}
	}
	gate.At, gate.Live = now, live
	if dry {
		if err := json.NewEncoder(a.out).Encode(gate); err != nil {
			return fmt.Errorf("печать ворот: %w", err)
		}
		return nil
	}
	old, oldErr := queue.ReadGate(cfg.Gate)
	stat, statErr := os.Stat(cfg.Gate)
	changed := oldErr != nil || !slices.Equal(old.Live, gate.Live) || !slices.Equal(old.Open, gate.Open) || !slices.Equal(old.Lane, gate.Lane) ||
		old.OpenByRoot == nil || !maps.EqualFunc(old.OpenByRoot, gate.OpenByRoot, slices.Equal[[]string]) ||
		old.CritOnly == nil || !maps.Equal(old.CritOnly, gate.CritOnly)
	stale := statErr != nil || old.At.After(now) || now.Sub(stat.ModTime()) > time.Hour
	if !changed && !stale {
		return nil
	}
	if err := gate.Write(cfg.Gate); err != nil {
		a.warn("queue", "gate.json", "err", err)
		return nil
	}
	if changed {
		line := fmt.Sprintf("ворота изменились: open=%s%s%s", strings.Join(gate.Open, ","), formatOpenByRoot(gate.OpenByRoot), formatCritOnly(gate.CritOnly))
		a.queueJournal(queue.Notice{Kind: "gate", Line: line}, now)
	}
	return nil
}

func formatOpenByRoot(byRoot map[string][]string) string {
	if len(byRoot) == 0 {
		return ""
	}
	parts := make([]string, 0, len(byRoot))
	for _, root := range slices.Sorted(maps.Keys(byRoot)) {
		pairs := "-"
		if len(byRoot[root]) != 0 {
			pairs = strings.Join(byRoot[root], ",")
		}
		parts = append(parts, root+"="+pairs)
	}
	return "; по корням: " + strings.Join(parts, ", ")
}

// formatCritOnly — корни дня отдыха с причиной для строки о смене ворот; в рабочий день пусто.
func formatCritOnly(critOnly map[string]string) string {
	if len(critOnly) == 0 {
		return ""
	}
	parts := make([]string, 0, len(critOnly))
	for _, root := range slices.Sorted(maps.Keys(critOnly)) {
		parts = append(parts, root+" ("+critOnly[root]+")")
	}
	return "; только CRIT: " + strings.Join(parts, ", ")
}

func (a *app) previousQueueGate(path string, now time.Time) queue.Gate {
	old, err := queue.ReadGate(path)
	// Файл старого формата без карты: tk видит только CRIT и HIGH, письма считают так же.
	if err == nil && queuegate.Read(path, now).Valid {
		return old
	}
	return queue.Gate{}
}

func (a *app) queueReport(since time.Duration, asJSON bool, now time.Time) error {
	counts, byTo := map[string]int{}, map[string]int{}
	gates := 0
	store := queueDaily(a)
	bad, err := store.Read(now.Add(-since), func(r *queueRecord) {
		if now.Sub(r.At) > since {
			return
		}
		if r.Kind == "gate" {
			gates++
		} else {
			counts[r.Kind]++
			byTo[r.To]++
		}
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bad > 0 {
		a.warn("queue", "битые строки журнала", "count", bad)
	}
	result := struct {
		Since       time.Duration  `json:"since"`
		Kinds       map[string]int `json:"kinds"`
		Recipients  map[string]int `json:"recipients"`
		GateChanges int            `json:"gate_changes"`
	}{since, counts, byTo, gates}
	if asJSON {
		return json.NewEncoder(a.out).Encode(result)
	}
	fmt.Fprintf(a.out, "за %s: смен ворот %d\n", since, gates)
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		fmt.Fprintf(a.out, "%s: %d\n", k, counts[k])
	}
	for _, k := range slices.Sorted(maps.Keys(byTo)) {
		if k != "" {
			fmt.Fprintf(a.out, "%s: %d\n", k, byTo[k])
		}
	}
	return nil
}

func queueDaily(a *app) daily.Store[queueRecord] {
	return daily.Store[queueRecord]{Dir: filepath.Join(a.stateDir, "queue"), What: "журнал очереди", At: func(r *queueRecord) time.Time { return r.At }}
}

func (a *app) queueNotices(ctx context.Context, c *config.Config, judge *restart.Judge, dry bool, now time.Time) error {
	defer a.namesAlert(c, dry, now) // и на выходе до Step: при холодном старте с битой таблицей лид узнаёт, почему письма стоят; письмо одно на все места демона
	frozen, problems, frozenErr := queue.ReadFrozen(c.Queue.Frozen, a.home)
	if frozenErr != nil {
		a.warn("queue", "список замороженных", "err", frozenErr)
	}
	for _, problem := range problems {
		a.warn("queue", "строка списка замороженных", "problem", problem)
	}
	statePath := filepath.Join(a.dir("queue"), "state.json")
	before, oldBytes, err := a.queueNoticeState(statePath, dry, now)
	if err != nil {
		a.warn("queue", "состояние", "err", err)
		return nil
	}
	tickets, err := a.cachedQueueTickets(ctx, c, now)
	if err != nil {
		a.warn("queue", "tk ls", "err", err)
		return nil
	}
	external, err := a.queueExternal(ctx, c, &before, now)
	if err != nil { // без внешних Step счёл бы их пропавшими и стёр отметки о письмах
		a.warn("queue", "внешний трекер", "err", err)
		return nil
	}
	tickets = queueNoticeTickets(tickets, external)
	a.queueCancelJournals(&before, tickets)
	alive, err := a.reg.Alive()
	if err != nil {
		a.warn("queue", "реестр сессий", "err", err)
		return nil
	}
	view := queueView(c, alive, judge, now, a, frozen, frozenErr)
	a.queueBootGrace(&view, c.Queue.BootGrace.D())
	notices := before.Step(tickets, view)
	if dry {
		for _, n := range notices {
			fmt.Fprintf(a.out, "%s %s %s: %s\n", n.Kind, n.To, n.Key, n.Line)
		}
		return nil
	}
	a.deliverQueueNotices(c, alive, &before, view, notices, now)
	a.writePendingQueueNotes(ctx, c.Queue.Tk, tickets, &before)
	newBytes, err := json.Marshal(before)
	if err != nil {
		return fmt.Errorf("кодирование состояния очереди: %w", err)
	}
	if !slices.Equal(oldBytes, newBytes) {
		if err := before.Save(statePath); err != nil {
			a.queueUnsaved = &before
			return err
		}
		a.queueUnsaved = nil
	}
	return nil
}

func queueNoticeTickets(cached, external []queue.Ticket) []queue.Ticket {
	return append(slices.Clone(cached), external...)
}

func (a *app) queueCancelJournals(state *queue.State, tickets []queue.Ticket) {
	for i := range tickets {
		if tickets[i].Status != "отклонён" {
			continue
		}
		author, reason, journalLoaded := state.CancelJournal(tickets[i].Key)
		if !journalLoaded && state.NeedsCancelJournal(tickets[i]) {
			var err error
			author, reason, err = readCancelJournal(tickets[i])
			journalLoaded = true
			if err != nil {
				a.warn("queue", "журнал отмены тикета", "path", tickets[i].Path, "err", err)
				author, reason = "", ""
			}
			state.RememberCancelJournal(tickets[i].Key, author, reason)
		}
		if journalLoaded {
			tickets[i].CancelAuthor, tickets[i].CancelReason, tickets[i].CancelJournalRead = author, reason, true
		}
	}
}

func (a *app) queueBootGrace(view *queue.View, grace time.Duration) {
	bootAt, err := a.systemBootTime()
	if err != nil {
		a.warn("queue", "время загрузки", "err", err)
		return
	}
	view.BootAt = bootAt
	view.BootGrace = grace
}

// queueNoticeState — состояние для прохода писем. Прошлая запись не удалась — берётся память демона: иначе каждые 30 с
// те же письма. Битый файл откладывается для разбора (сухой прогон файлы не трогает), проход идёт с пустого.
func (a *app) queueNoticeState(statePath string, dry bool, now time.Time) (queue.State, []byte, error) {
	if a.queueUnsaved != nil {
		return *a.queueUnsaved, nil, nil
	}
	state, oldBytes, err := loadQueueState(statePath)
	if err == nil || !queueStateJSONError(err) {
		return state, oldBytes, err
	}
	a.warn("queue", "состояние", "err", err)
	if !dry {
		aside := statePath + "." + now.Format("20060102T150405.000000000")
		if renameErr := os.Rename(statePath, aside); renameErr != nil {
			return queue.State{}, nil, fmt.Errorf("битое состояние не отложено: %w", renameErr)
		}
	}
	return queue.State{}, nil, nil
}

func queueStateJSONError(err error) bool {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntax) || errors.As(err, &typeErr)
}

func loadQueueState(path string) (queue.State, []byte, error) {
	state, err := queue.LoadState(path)
	if err != nil {
		return queue.State{}, nil, err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return queue.State{}, nil, fmt.Errorf("кодирование исходного состояния очереди: %w", err)
	}
	return state, b, nil
}

func (a *app) cachedQueueTickets(ctx context.Context, c *config.Config, now time.Time) ([]queue.Ticket, error) {
	dirs, err := queueFingerprintDirs(&c.Queue)
	if err != nil {
		return nil, fmt.Errorf("папки задач: %w", err)
	}
	cache := &a.queueCache
	table, err := a.namesTable("queue")
	if err != nil {
		return nil, err
	}
	ownersKey, err := queueOwnersKey(c, table)
	if err != nil {
		return nil, err
	}
	ownersChanged := cache.Loaded && cache.OwnersKey != ownersKey
	projects, changed, topologyChanged, err := queueProjectChanges(dirs, &c.Queue, cache)
	if err != nil {
		return nil, err
	}
	fullScanDue := !cache.Loaded || ownersChanged || queueProjectsExpired(projects, cache.Projects, now)
	if fullScanDue {
		tickets, err := a.queueTicketsWithTable(ctx, c, table)
		if err != nil {
			var listErr queueListError
			if cache.Loaded && errors.As(err, &listErr) {
				a.warn("queue", "tk ls", "err", err)
				return cache.Tickets, nil
			}
			return nil, err
		}
		projects = queueCacheProjects(projects, tickets, now)
		cache.Tickets, cache.ScannedAt, cache.OwnersKey, cache.Loaded, cache.Projects = tickets, now, ownersKey, true, projects
		return tickets, nil
	}
	if len(changed) == 0 && cache.Loaded && !topologyChanged {
		return cache.Tickets, nil
	}
	for _, project := range changed {
		tickets, err := a.queueTicketsProject(ctx, c, project, table, dirs)
		if err != nil {
			var listErr queueListError
			if cache.Loaded && errors.As(err, &listErr) {
				a.warn("queue", "tk ls", "err", err)
				return cache.Tickets, nil
			}
			return nil, err
		}
		entry := projects[project]
		entry.Tickets, entry.ScannedAt = tickets, now
		projects[project] = entry
	}
	tickets := queueProjectTickets(projects)
	cache.Tickets, cache.ScannedAt, cache.OwnersKey, cache.Loaded, cache.Projects = tickets, now, ownersKey, true, projects
	return tickets, nil
}

// queueProjectTickets supplies the user-visible order after project slices are combined.
func queueProjectTickets(projects map[string]queueProjectCache) []queue.Ticket {
	var tickets []queue.Ticket
	for _, entry := range projects {
		tickets = append(tickets, entry.Tickets...)
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].Key < tickets[j].Key })
	return tickets
}

func queueOwnersKey(c *config.Config, table map[string]string) (string, error) {
	data, err := json.Marshal(struct {
		Lead   string            `json:"lead"`
		Admins map[string]string `json:"admins"`
		Names  map[string]string `json:"names"`
	}{c.Lead, c.Admins, table})
	return string(data), err
}

func queueProjectsExpired(current, previous map[string]queueProjectCache, now time.Time) bool {
	for project := range current {
		if old, ok := previous[project]; ok && now.Sub(old.ScannedAt) >= 10*time.Minute {
			return true
		}
	}
	return false
}

func queueCacheProjects(projects map[string]queueProjectCache, tickets []queue.Ticket, now time.Time) map[string]queueProjectCache {
	for project, entry := range projects {
		entry.Tickets = nil
		projects[project] = entry
	}
	for i := range tickets {
		entry, ok := projects[tickets[i].Dir]
		if !ok {
			continue
		}
		entry.Tickets = append(entry.Tickets, tickets[i])
		entry.ScannedAt = now
		projects[tickets[i].Dir] = entry
	}
	for project, entry := range projects {
		entry.ScannedAt = now
		projects[project] = entry
	}
	return projects
}

func queueProjectChanges(dirs []string, cfg *queue.Config, cache *queueTicketCache) (map[string]queueProjectCache, []string, bool, error) {
	projects := make(map[string]queueProjectCache, len(dirs))
	changed := make([]string, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for _, tasks := range dirs {
		project := queueTasksProject(tasks, cfg.Dirs)
		if seen[project] {
			continue
		}
		seen[project] = true
		fingerprint, err := queue.Fingerprint([]string{tasks})
		if err != nil {
			return nil, nil, false, fmt.Errorf("отпечаток папок: %w", err)
		}
		previous, ok := cache.Projects[project]
		if !ok || previous.Fingerprint != fingerprint {
			changed = append(changed, project)
			projects[project] = queueProjectCache{Fingerprint: fingerprint}
		} else {
			projects[project] = previous
		}
	}
	slices.Sort(changed)
	topologyChanged := len(cache.Projects) != len(projects)
	return projects, changed, topologyChanged, nil
}

// queueJudge — судья свободы сессии: restart.Judge, в тестах подставной.
type queueJudge interface {
	Free(cfg *restart.Config, pid int, after time.Duration, now time.Time) (bool, string)
	WakeFree(cfg *restart.Config, pid int, after time.Duration, now time.Time) (bool, error)
}

func queueView(c *config.Config, alive map[string]session.Info, judge queueJudge, now time.Time, a *app, frozen map[string]bool, frozenErr error) queue.View {
	cfg := &c.Queue
	live := slices.Clone(cfg.Live)
	if cfg.Mode == queue.ModeLog {
		live = nil
	}
	freeCache := map[string]bool{}
	free := func(name string) bool {
		if state, ok := freeCache[name]; ok {
			return state
		}
		in, ok := alive[name]
		if !ok {
			freeCache[name] = false
			return false
		}
		state, _ := judge.Free(&c.Restart, in.Pid, cfg.FreeAfter.D(), now)
		freeCache[name] = state
		return state
	}
	var owners map[string]bool
	var workerErr error
	var workersRead bool
	wakeFree := func(name string) bool {
		if !workersRead {
			owners, workerErr = a.workerOwners()
			workersRead = true
			if workerErr != nil {
				a.warn("queue", "список бойцов", "err", workerErr)
			}
		}
		in, ok := alive[name]
		if !ok || workerErr != nil || owners[name] {
			return false
		}
		free, err := judge.WakeFree(&c.Restart, in.Pid, cfg.FreeAfter.D(), now)
		if err != nil {
			a.warn("queue", "задачи сессии", "err", err)
			return false
		}
		return free
	}
	turnAfter := func(name string, at time.Time) bool {
		id := alive[name].SessionID
		paths, err := filepath.Glob(filepath.Join(a.projects, "*", filepath.Base(id)+".jsonl"))
		if err != nil || len(paths) > 1 {
			a.warn("queue", "транскрипт сессии", "session", name, "files", len(paths), "err", err)
			return false
		}
		if len(paths) == 0 {
			return false
		}
		info, err := os.Stat(paths[0])
		if err != nil {
			a.warn("queue", "транскрипт сессии", "session", name, "err", err)
			return false
		}
		return info.ModTime().After(at)
	}
	return queue.View{Now: now, Home: a.home, Lead: c.Lead, Rest: cfg.Rest(), Log: a.log, CritCopyAfter: cfg.CritCopyAfter.D(),
		Frozen:   func(dir string) (bool, error) { return frozen[filepath.Clean(dir)], frozenErr },
		Projects: func() []queue.Ticket { return a.queueWakeProjects(c) },
		Serious:  func(project string) bool { return slices.Contains(live, project) },
		Alive:    func(name string) bool { _, ok := alive[name]; return ok }, Free: free, WakeFree: wakeFree,
		Session:   func(name string) string { return alive[name].SessionID },
		Cwd:       func(name string) string { return alive[name].Cwd },
		TurnAfter: turnAfter,
		AdminFor:  func(dir string) string { _, admin := orphans.LongestPrefix(c.Admins, dir); return admin },
		AdminNext: func(name, dir string) (queue.Ticket, bool, error) {
			return a.nextForAdmin(context.Background(), cfg.Tk, name, dir, now)
		},
		Next: func(name, dir string) (queue.Ticket, bool, error) {
			return a.nextForWake(context.Background(), cfg.Tk, name, dir, now, func(at time.Time) bool { return turnAfter(name, at) })
		},
		WakeCandidates: func(name, dir string) ([]queue.Ticket, error) {
			candidates, err := a.wakeCandidates(context.Background(), cfg.Tk, name, dir)
			if err != nil {
				a.warn("queue", "tk ls (wake)", "err", err)
			}
			return candidates, err
		},
	}
}

func (a *app) deliverQueueNotices(c *config.Config, alive map[string]session.Info, state *queue.State, view queue.View, notices []queue.Notice, now time.Time) {
	cfg := &c.Queue
	groups := map[string][]queue.Notice{}
	for _, n := range notices {
		if !n.Serious || cfg.Mode == queue.ModeLog {
			state.ConfirmUnsent(n, view)
			a.queueJournal(n, now)
			a.log.Info("aguard queue: написал бы", "kind", n.Kind, "to", n.To, "key", n.Key, "line", n.Line)
			a.note("queue", "would_send", n.Key, n.To, 1, "notice")
			continue
		}
		if n.To == "" {
			a.queueJournal(n, now)
			continue
		}
		groups[n.To] = append(groups[n.To], n)
	}
	for to, ns := range groups {
		var lines []string
		for _, n := range ns {
			lines = append(lines, n.Line)
		}
		kind := queueGuideKind(ns[0].Kind)
		a.warnOtherGuideInput(kind, ns[0].Kind)
		if err := a.sendAlive(alive, to, c.SendAuth, kind, strings.Join(lines, "\n")); err != nil {
			if a.quietFailure("queue", err) {
				continue
			}
			a.warn("queue", "письмо не отправлено", "to", to, "err", err)
			a.note("queue", "send_failed", "", to, int64(len(ns)), "notices")
			continue
		}
		for _, n := range ns {
			state.Confirm(n, view)
			a.queueJournal(n, now)
			kind := queueGuideKind(n.Kind)
			a.warnOtherGuideInput(kind, n.Kind)
			a.log.Info("aguard queue: письмо доставлено", "kind", kind, "source", n.Kind, "to", to, "key", n.Key)
			a.note("queue", "sent", n.Key, to, 1, "notice")
		}
	}
}

func (a *app) writePendingQueueNotes(ctx context.Context, tk string, tickets []queue.Ticket, state *queue.State) {
	for _, note := range state.PendingNotes() {
		if err := a.queueTicketNote(ctx, tk, tickets, note); err != nil {
			a.warn("queue", "tk note", "key", note.Key, "err", err)
			continue
		}
		state.ConfirmNote(note.Key, note.Line)
	}
}

func (a *app) queueTicketNote(ctx context.Context, tk string, tickets []queue.Ticket, n queue.PendingNote) error {
	for i := range tickets {
		t := &tickets[i]
		if t.Key != n.Key {
			continue
		}
		if t.Transfer != "" || t.External { // файла тикета нет, записывать некуда
			return nil
		}
		cmd := exec.CommandContext(ctx, tk, "note", t.Slug, n.Line, "--by", "aguard")
		cmd.Dir = t.Dir
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	return fmt.Errorf("тикет %s не найден", n.Key)
}

func queueFingerprintDirs(cfg *queue.Config) ([]string, error) {
	skip := map[string]bool{}
	for _, p := range cfg.Skip {
		skip[filepath.Clean(p)] = true
	}
	var dirs []string
	add := func(project, tasks string) {
		project, tasks = filepath.Clean(project), filepath.Clean(tasks)
		if !skip[project] && !skip[tasks] {
			dirs = append(dirs, tasks)
		}
	}
	for _, root := range cfg.GitRoots {
		repos, err := queueTaskRepos(root)
		if err != nil {
			return nil, err
		}
		for _, project := range repos {
			add(project, filepath.Join(project, "docs", "tasks"))
		}
	}
	for _, dir := range cfg.Dirs {
		tasks := filepath.Join(dir, "docs", "tasks")
		if st, err := os.Stat(tasks); err == nil && st.IsDir() {
			add(dir, tasks)
		} else {
			add(dir, filepath.Join(dir, "tasks"))
		}
	}
	return dirs, nil
}

// queueTaskRepos mirrors tk taskRepos(root, root): root and direct child directories with docs/tasks,
// excluding a .git marker that is not itself a directory.
func queueTaskRepos(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []string{root}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("чтение корня задач %s: %w", root, err)
	}
	candidates := []string{root}
	for _, entry := range entries {
		if entry.IsDir() {
			candidates = append(candidates, filepath.Join(root, entry.Name()))
		}
	}
	var repos []string
	for _, project := range candidates {
		gitInfo, err := os.Lstat(filepath.Join(project, ".git"))
		if err == nil && !gitInfo.IsDir() {
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("проверка .git в %s: %w", project, err)
		}
		if _, err := os.Stat(filepath.Join(project, "docs", "tasks")); err == nil {
			repos = append(repos, project)
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("проверка docs/tasks в %s: %w", project, err)
		}
	}
	return repos, nil
}

func queueKey(project, slug, path string) string {
	name := filepath.Base(path)
	if len(name) < 14 || name[13] != '-' {
		return ""
	}
	if !validQueueStamp(name[:13]) {
		return ""
	}
	return project + "/" + name[:13] + "-" + slug
}

func validQueueStamp(stamp string) bool {
	if len(stamp) != 13 {
		return false
	}
	for i, char := range stamp {
		if i == 8 {
			if char != '-' {
				return false
			}
		} else if char < '0' || char > '9' {
			return false
		}
	}
	date, err := time.Parse("20060102", stamp[:8])
	if err != nil {
		return false
	}
	hour, minute := (int(stamp[9]-'0')*10)+int(stamp[10]-'0'), (int(stamp[11]-'0')*10)+int(stamp[12]-'0')
	return hour < 24 && minute < 60 && date.Format("20060102") == stamp[:8]
}

func queueOwners(dir, home, lead string, table, admins map[string]string) (master, admin string) {
	master = names.For(dir, home, table)
	if master == "" {
		master = lead
	}
	_, admin = orphans.LongestPrefix(admins, dir)
	if admin == "" {
		admin = lead
	}
	return master, admin
}

func (a *app) queueTickets(ctx context.Context, c *config.Config) ([]queue.Ticket, error) {
	table, err := a.namesTable("queue")
	if err != nil {
		return nil, err
	}
	return a.queueTicketsWithTable(ctx, c, table)
}

func (a *app) queueTicketsWithTable(ctx context.Context, c *config.Config, table map[string]string) ([]queue.Ticket, error) {
	cfg := &c.Queue
	var out []queue.Ticket
	read := func(root string, rootMode bool) error {
		args := []string{"ls", "--json", "--all"}
		if rootMode {
			args = append(args, "--root", root)
		}
		cmd := exec.CommandContext(ctx, cfg.Tk, args...)
		if !rootMode {
			cmd.Dir = root
		}
		b, e := cmd.Output()
		if e != nil {
			return queueListError{err: fmt.Errorf("%s: %w", root, e)}
		}
		var raw []queueTicketJSON
		if e = json.Unmarshal(b, &raw); e != nil {
			return queueListError{err: fmt.Errorf("%s: разбор: %w", root, e)}
		}
		for i := range raw {
			if ticket, ok := a.queueTicketFromJSON(c, root, rootMode, &raw[i], table); ok {
				out = append(out, ticket)
			}
		}
		return nil
	}
	for _, root := range cfg.GitRoots {
		if err := read(root, true); err != nil {
			return nil, err
		}
	}
	for _, root := range cfg.Dirs {
		if err := read(root, false); err != nil {
			return nil, err
		}
	}
	transfers, err := a.queueTransfers(c, table)
	if err != nil {
		return nil, queueListError{err: err}
	}
	out = append(out, transfers...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// queueTicketsProject reads one project in place; its filesystem fingerprint is maintained independently.
func (a *app) queueTicketsProject(ctx context.Context, c *config.Config, project string, table map[string]string, taskDirs []string) ([]queue.Ticket, error) {
	args := []string{"ls", "--json", "--all"}
	cmd := exec.CommandContext(ctx, c.Queue.Tk, args...)
	cmd.Dir = project
	b, err := cmd.Output()
	if err != nil {
		return nil, queueListError{err: fmt.Errorf("%s: %w", project, err)}
	}
	var raw []queueTicketJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, queueListError{err: fmt.Errorf("%s: разбор: %w", project, err)}
	}
	var tickets []queue.Ticket
	for i := range raw {
		if ticket, ok := a.queueTicketFromJSON(c, project, false, &raw[i], table); ok {
			tickets = append(tickets, ticket)
		}
	}
	transfers, err := a.queueTransfersProject(c, table, project, taskDirs)
	if err != nil {
		return nil, queueListError{err: err}
	}
	tickets = append(tickets, transfers...)
	return tickets, nil
}

func (a *app) queueTicketFromJSON(c *config.Config, root string, rootMode bool, r *queueTicketJSON, table map[string]string) (queue.Ticket, bool) {
	project := root
	if r.Repo != "" && r.Repo != filepath.Base(root) {
		project = filepath.Join(root, r.Repo)
	}
	project = filepath.Clean(project)
	if slices.Contains(c.Queue.Skip, project) {
		return queue.Ticket{}, false
	}
	key := queueKey(filepath.Base(project), r.Slug, r.Path)
	if key == "" {
		a.warn("queue", "имя файла тикета", "path", r.Path)
		return queue.Ticket{}, false
	}
	created, err := time.ParseInLocation("20060102-1504", filepath.Base(r.Path)[:13], time.Local)
	if err != nil {
		a.warn("queue", "имя файла тикета", "path", r.Path, "err", err)
		return queue.Ticket{}, false
	}
	master, admin := queueOwners(project, a.home, c.Lead, table, c.Admins)
	ticketPath, err := queueTicketPath(project, rootMode, r.Path)
	if err != nil {
		a.warn("queue", "папка тикета", "project", project, "err", err)
		ticketPath = filepath.Join("docs", "tasks", filepath.Base(filepath.FromSlash(r.Path)))
	}
	actorNotes := a.queueActorNotes(r)
	return queue.Ticket{Key: key, Project: filepath.Base(project), Dir: project, Path: ticketPath, Slug: r.Slug, Title: r.Title, Prio: r.Prio, Status: r.Status, Back: r.Back, Master: master, Admin: admin, Created: created, Executor: r.Executor, Journal: r.Journal,
		MasterNote: actorNotes[master], ActorNotes: actorNotes, BlockedBy: r.BlockedBy, Overdue: r.Overdue, WaitDue: r.Due != ""}, true
}

func queueTicketPath(project string, rootMode bool, tkPath string) (string, error) {
	tasks := filepath.Join(project, "docs", "tasks")
	if !rootMode {
		info, err := os.Stat(tasks)
		if errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
			tasks = filepath.Join(project, "tasks")
		} else if err != nil {
			return "", fmt.Errorf("каталог %s: %w", tasks, err)
		}
	}
	rel, err := filepath.Rel(project, tasks)
	if err != nil {
		return "", fmt.Errorf("относительный путь от %s к %s: %w", project, tasks, err)
	}
	return filepath.Join(rel, filepath.Base(filepath.FromSlash(tkPath))), nil
}

// queueTransferLine — строка .transferred.jsonl, которую tk дописывает до удаления перенесённого тикета.
type queueTransferLine struct {
	Slug string `json:"slug"`
	Stem string `json:"stem"`
	Back string `json:"back"`
	Key  string `json:"key"`
	URL  string `json:"url"`
	By   string `json:"by"`
}

// queueTransfers — перенесённые во внешний трекер тикеты с заказчиком как закрытые: доставка, однократность и потеря — те же, что у строки о закрытом.
// Файл читается с тикетами: tk удаляет файл тикета, папка меняется, отпечаток видит перенос. Ошибка чтения — как сбой tk ls: прошлый список цел, отметки доставки не стираются.
func (a *app) queueTransfers(c *config.Config, table map[string]string) ([]queue.Ticket, error) {
	cfg := &c.Queue
	dirs, err := queueFingerprintDirs(cfg)
	if err != nil {
		return nil, fmt.Errorf("папки переносов: %w", err)
	}
	var out []queue.Ticket
	for _, tasks := range dirs {
		project := queueTasksProject(tasks, cfg.Dirs)
		if slices.ContainsFunc(cfg.Skip, func(p string) bool { return filepath.Clean(p) == project }) {
			continue
		}
		lines, err := a.readQueueTransfers(filepath.Join(tasks, ".transferred.jsonl"))
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			if ticket, ok := a.queueTransferTicket(c, project, line, table); ok {
				out = append(out, ticket)
			}
		}
	}
	return out, nil
}

func (a *app) queueTransfersProject(c *config.Config, table map[string]string, project string, dirs []string) ([]queue.Ticket, error) {
	for _, skip := range c.Queue.Skip {
		if filepath.Clean(skip) == filepath.Clean(project) {
			return nil, nil
		}
	}
	var out []queue.Ticket
	for _, tasks := range dirs {
		if queueTasksProject(tasks, c.Queue.Dirs) != project {
			continue
		}
		lines, err := a.readQueueTransfers(filepath.Join(tasks, ".transferred.jsonl"))
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			if ticket, ok := a.queueTransferTicket(c, project, line, table); ok {
				out = append(out, ticket)
			}
		}
	}
	return out, nil
}

func (a *app) queueTransferTicket(c *config.Config, project string, line queueTransferLine, table map[string]string) (queue.Ticket, bool) {
	key := queueKey(filepath.Base(project), line.Slug, line.Stem)
	if key == "" {
		a.warn("queue", "строка переноса", "stem", line.Stem)
		return queue.Ticket{}, false
	}
	master, admin := queueOwners(project, a.home, c.Lead, table, c.Admins)
	transfer := strings.TrimSpace(line.Key + " " + line.URL)
	return queue.Ticket{Key: key, Project: filepath.Base(project), Dir: project, Slug: line.Slug, Status: queue.StatusTransferred,
		Back: line.Back, Master: master, Admin: admin, Transfer: transfer, CancelAuthor: line.By, CancelJournalRead: true,
		CancelReason: "перенос во внешний трекер " + transfer}, true
}

// queueTasksProject — папка проекта по папке тикетов: <проект>/docs/tasks, у папок из dirs — <папка>/tasks.
func queueTasksProject(tasks string, dirs []string) string {
	parent := filepath.Dir(tasks)
	if filepath.Base(parent) == "docs" && !slices.Contains(dirs, parent) {
		return filepath.Dir(parent)
	}
	return parent
}

func (a *app) readQueueTransfers(path string) ([]queueTransferLine, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("чтение переносов: %w", err)
	}
	var out []queueTransferLine
	for raw := range strings.SplitSeq(string(b), "\n") {
		if raw == "" {
			continue
		}
		var line queueTransferLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil || line.Stem == "" || line.Key == "" {
			a.warn("queue", "битая строка переноса", "path", path, "err", err)
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

type queueRecord struct {
	At      time.Time `json:"at"`
	Serious bool      `json:"serious"`
	Kind    string    `json:"kind"`
	To      string    `json:"to"`
	Key     string    `json:"key"`
	Line    string    `json:"line"`
}

func (a *app) queueJournal(n queue.Notice, at time.Time) {
	r := queueRecord{At: at, Serious: n.Serious, Kind: n.Kind, To: n.To, Key: n.Key, Line: n.Line}
	store := queueDaily(a)
	if err := store.Append([]queueRecord{r}); err != nil {
		a.warn("queue", "журнал", "err", err)
		return
	}
	if err := store.Rotate(at, 60*24*time.Hour); err != nil {
		a.warn("queue", "ротация журнала", "err", err)
	}
}

// queueWakeProjects сохраняет путь для опроса, даже когда последний тикет исчез из tk ls.
func (a *app) queueWakeProjects(c *config.Config) []queue.Ticket {
	dirs, err := queueFingerprintDirs(&c.Queue)
	if err != nil {
		a.warn("queue", "папки задач", "err", err)
		return nil
	}
	table, err := a.namesTable("queue")
	if err != nil {
		a.warn("queue", "имена", "err", err)
		return nil
	}
	var projects []queue.Ticket
	for _, tasks := range dirs {
		st, err := os.Stat(tasks)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			a.warn("queue", "папка задач", "path", tasks, "err", err)
			continue
		}
		if !st.IsDir() {
			continue
		}
		dir := queueTasksProject(tasks, c.Queue.Dirs)
		master, admin := queueOwners(dir, a.home, c.Lead, table, c.Admins)
		project := queue.WakeTicket(filepath.Base(dir), dir, "")
		project.Master, project.Admin = master, admin
		projects = append(projects, project)
	}
	return projects
}

// queueActorNotes — время последней записи каждого автора журнала тикета (tk ls --json, поле actors). Битое время —
// WARN, автор без записи: тикет остаётся в очереди, как без заметки.
func (a *app) queueActorNotes(r *queueTicketJSON) map[string]time.Time {
	if len(r.Actors) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(r.Actors))
	for actor, s := range r.Actors {
		at, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			a.warn("queue", "время записи автора в журнале тикета", "path", r.Path, "actor", actor, "err", err)
			continue
		}
		out[actor] = at
	}
	return out
}

func readCancelJournal(t queue.Ticket) (string, string, error) {
	if !filepath.IsLocal(t.Path) {
		return "", "", fmt.Errorf("путь тикета не локальный: %q", t.Path)
	}
	path := filepath.Join(t.Dir, filepath.FromSlash(t.Path))
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("чтение тикета %s: %w", path, err)
	}
	return parseCancelJournal(b, t.Status)
}

func parseCancelJournal(data []byte, status string) (string, string, error) {
	inJournal := false
	entry := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inJournal = trimmed == "## Журнал"
			continue
		}
		if !inJournal || !strings.HasPrefix(line, "- ") {
			continue
		}
		candidate := strings.TrimSpace(line)
		parts := strings.SplitN(strings.TrimPrefix(candidate, "- "), " — ", 3)
		stampStatus := strings.Fields(parts[0])
		if len(stampStatus) == 3 && stampStatus[2] == status {
			entry = candidate
		}
	}
	if entry == "" {
		return "", "", fmt.Errorf("в тикете нет записи со статусом %q", status)
	}
	parts := strings.SplitN(strings.TrimPrefix(entry, "- "), " — ", 3)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("неизвестный формат строки журнала: %s", entry)
	}
	stampStatus := strings.Fields(parts[0])
	if len(stampStatus) != 3 || stampStatus[2] != status {
		return "", "", fmt.Errorf("последняя запись журнала не соответствует статусу %q: %s", status, entry)
	}
	author := strings.TrimSpace(parts[1])
	if author == "" {
		return "", "", fmt.Errorf("автор последней записи журнала пуст")
	}
	reason := strings.TrimSpace(parts[2])
	if _, err := time.ParseInLocation("2006-01-02 15:04", stampStatus[0]+" "+stampStatus[1], time.Local); err != nil {
		return author, reason, fmt.Errorf("время последней записи журнала: %w", err)
	}
	return author, reason, nil
}

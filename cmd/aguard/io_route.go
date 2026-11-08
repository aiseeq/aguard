package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/ioroute"
	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/aguard/internal/names"
	"github.com/aiseeq/akit/proc"
)

// Маршрут случаев фриза (тикет alert-async): хозяин виновника по главной cgroup, класс — что фризило; письма и
// тикеты по ioroute. Тикеты заводит одна горутина: два случая одного класса не заводят два тикета.

// ioOwnerFinder — хозяин главной cgroup случая.
type ioOwnerFinder struct {
	jobs     *cpuwatch.Jobs
	units    map[string]string // служба → сессия ([io] owners)
	home     string
	table    map[string]string               // таблица имён cl: папка → сессия
	sessions func(id string) (string, error) // живая сессия по SessionID, "" — нет такой
}

func (a *app) ioOwners(cfg *iowatch.Config) *ioOwnerFinder {
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err != nil {
		a.warn("io", "таблица имён cl для хозяев случаев", "err", err)
		table = map[string]string{}
	}
	return &ioOwnerFinder{units: cfg.Owners, home: a.home, table: table,
		jobs: &cpuwatch.Jobs{Dir: filepath.Join(a.home, ".local", "state", "worker"),
			Units: fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice", a.uid, a.uid)},
		sessions: func(id string) (string, error) {
			alive, err := a.reg.Alive()
			if err != nil {
				return "", err
			}
			for _, name := range slices.Sorted(maps.Keys(alive)) {
				if alive[name].SessionID == id {
					return name, nil
				}
			}
			return "", nil
		}}
}

// routeCase — случай для маршрута. Хозяина нет, когда главную cgroup не к кому отнести; главная — возможная жертва
// (удаление снимка btrfs, чтение главной — подкачка) — хозяин есть, но с пометкой Victim; сбой поиска задания бойца или сессии
// по scratchpad — хозяина нет, ошибка наверх.
func (o *ioOwnerFinder) routeCase(c *iowatch.Case) (ioroute.Case, error) {
	rc := ioroute.Case{Start: c.Start, End: c.End, DurMS: c.DurMS, Text: ioCaseLine(c)}
	cul := c.Culprits
	if cul == nil || cul.Main == "" {
		rc.Class, rc.Generic = "unknown", true
		if cul.KernelOver() {
			rc.Class = "kernel"
		}
		return rc, nil
	}
	rc.Victim = cul.SnapshotDeletion() || c.PagingVictim()
	unit := proc.ServiceUnit(cul.Main)
	if name, _, ok := proc.SessionScope(cul.Main); ok {
		rc.Class, rc.Generic = byProc(cul.MainProc, "session")
		rc.Owner = name
		return rc, nil
	}
	if strings.HasPrefix(unit, "worker-") {
		rc.Class, rc.Generic = byProc(cul.MainProc, "worker")
		job, err := o.jobs.ByUnit(unit)
		if err != nil {
			return rc, fmt.Errorf("задание бойца %s: %w", unit, err)
		}
		if job != nil {
			rc.Owner = job.Master
		}
		return rc, nil
	}
	if owner, ok := o.units[unit]; ok {
		rc.Class, rc.Owner = ticketClass(iowatch.CgroupName(cul.Main)), owner
		return rc, nil
	}
	rc.Class = ticketClass(iowatch.CgroupName(cul.Main))
	owner, err := o.byProcess(cul.ProcessDetails[cul.MainProc])
	rc.Owner = owner
	if owner != "" {
		rc.Class, rc.Generic = byProc(cul.MainProc, rc.Class)
	}
	return rc, err
}

// sessionTmp — scratchpad сессии Claude Code: …/claude-tmp/claude-<uid>/<ключ проекта>/<SessionID>/.
var sessionTmp = regexp.MustCompile(`/claude-tmp/claude-\d+/(-[^/ ]+)/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:/|$| )`)

// byProcess — хозяин временного scope (systemd-run пробы, песочница) по главному процессу: scratchpad сессии в
// cmdline или cwd — живая сессия с этим SessionID, иначе мастер проекта по ключу; cwd в ~/git|~/work/<проект> — его
// мастер. Ничего — "".
func (o *ioOwnerFinder) byProcess(d iowatch.ProcessDetails) (string, error) {
	for _, s := range []string{d.CWD, d.Cmdline} {
		m := sessionTmp.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		if o.sessions != nil {
			name, err := o.sessions(m[2])
			if err != nil {
				return "", fmt.Errorf("сессия %s по scratchpad: %w", m[2], err)
			}
			if name != "" {
				return name, nil
			}
		}
		if name := o.byProjectKey(m[1]); name != "" {
			return name, nil
		}
	}
	// cwd ровно ~ хозяина не говорит: из него работают Chrome, Telegram и прочие программы рабочего стола, а по таблице
	// имён ~ — лид (находка проверки alert-async 08.11: Chrome — 178 случаев за неделю ушли бы лиду).
	if d.CWD != "" && filepath.Clean(d.CWD) != o.home {
		return names.For(d.CWD, o.home, o.table), nil
	}
	return "", nil
}

// byProjectKey — сессия проекта по ключу папки Claude Code (путь, где / заменён на -): ~/git/<проект>, ~/work/<проект>.
func (o *ioOwnerFinder) byProjectKey(key string) string {
	homeKey := strings.ReplaceAll(o.home, "/", "-")
	for _, space := range []string{"git", "work"} {
		if p, ok := strings.CutPrefix(key, homeKey+"-"+space+"-"); ok && p != "" {
			return names.For(filepath.Join(o.home, space, p), o.home, o.table)
		}
	}
	return ""
}

func byProc(main, generic string) (string, bool) {
	if c := ticketClass(iowatch.ProcBase(main)); c != "" {
		return c, false
	}
	return generic, true
}

var (
	classUnsafe = regexp.MustCompile(`[^a-z0-9]+`)
	// classTail — номер экземпляра в имени unit: -<pid>, (<pid>), -<hex id>; один класс у всех экземпляров.
	classTail = regexp.MustCompile(`(?:[-@][0-9]+|\([0-9]+\)|-[0-9a-f]{12,})$`)
)

// ticketClass — класс для slug тикета: имя unit без типа и номера экземпляра, строчные латиница и цифры через дефис,
// до 32 знаков.
func ticketClass(name string) string {
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".service"), ".scope")
	name = classTail.ReplaceAllString(name, "")
	s := strings.Trim(classUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return s
}

// ioTicketJob — тикет хозяину: заметка в открытый io-freeze-<класс>[-N] или новый. tk, lead, log — из конфига на
// момент случая (горутина тикетов конфиг не читает).
type ioTicketJob struct {
	owner, class string
	why          ioroute.Why
	cases        []ioroute.Case
	tk, lead     string
	log          bool // mode = log: только журнал «завёл бы»
}

// ioTicketer — исполнитель тикетов: одна горутина, очередь с запасом.
type ioTicketer struct {
	a     *app
	queue chan ioTicketJob
}

func (t *ioTicketer) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-t.queue:
			if err := t.apply(ctx, job); err != nil {
				t.a.warn("io", "тикет о фризе", "owner", job.owner, "class", job.class, "err", err)
				t.a.note("io", "ticket_failed", job.class, job.owner, int64(len(job.cases)), "cases")
			}
		}
	}
}

// add — в очередь; полна — потеря явно, в журнал.
func (t *ioTicketer) add(job ioTicketJob) {
	select {
	case t.queue <- job:
	default:
		t.a.warn("io", "очередь тикетов полна, случаи без тикета", "owner", job.owner, "class", job.class, "cases", len(job.cases))
		t.a.note("io", "ticket_failed", job.class, job.owner, int64(len(job.cases)), "cases")
	}
}

func (t *ioTicketer) apply(ctx context.Context, job ioTicketJob) error {
	who := job.owner
	if who == "" {
		who = job.lead
	}
	dir, err := t.a.sessionDir(who, job.lead)
	if err != nil { // хозяин есть, его папки нет (задание бойца от сессии без проекта): тикет лиду с именем хозяина
		t.a.warn("io", "папка хозяина не найдена, тикет — лиду", "owner", who, "err", err)
		dir, job.lead = t.a.home, job.lead+" (за "+who+")"
	}
	base := "io-freeze-" + job.class
	if job.log {
		t.a.log.Info("aguard io: завёл бы тикет", "dir", dir, "slug", base, "cases", len(job.cases))
		t.a.note("io", "ticket_would", job.class, who, int64(len(job.cases)), "cases")
		return nil
	}
	open, next, prev, err := t.find(ctx, job.tk, dir, base)
	if err != nil {
		return err
	}
	if open != "" {
		if err := tkRun(ctx, job.tk, dir, nil, "note", open, ioTicketNote(job), "--by", "aguard"); err != nil {
			return err
		}
		t.a.note("io", "ticket_note", open, who, int64(len(job.cases)), "cases")
		return nil
	}
	text := ioTicketText(job, prev)
	args := []string{"new", next, "--prio", "MED", "--diff", "HUNT", "--by", "aguard", "--title", ioTicketTitle(job), "--text", "-"}
	if err := tkRun(ctx, job.tk, dir, strings.NewReader(text), args...); err != nil {
		return err
	}
	t.a.note("io", "ticket_new", next, who, int64(len(job.cases)), "cases")
	return nil
}

type tkRow struct {
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

var tkClosed = map[string]bool{"готово": true, "отклонён": true, "перенесён": true}

// find — открытый тикет класса (base или base-N); иначе свободный slug и последний закрытый (ссылка в новом).
func (t *ioTicketer) find(ctx context.Context, tk, dir, base string) (open, next, prev string, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tk, "ls", "--all", "--slug", base, "--json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("tk ls в %s: %w", dir, err)
	}
	var rows []tkRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", "", "", fmt.Errorf("tk ls в %s: %w", dir, err)
	}
	maxN := 0
	for _, r := range rows {
		n, ok := classSlugN(r.Slug, base)
		if !ok {
			continue
		}
		if !tkClosed[r.Status] {
			return r.Slug, "", "", nil
		}
		if n >= maxN {
			maxN, prev = n, r.Slug
		}
	}
	if prev == "" {
		return "", base, "", nil
	}
	return "", base + "-" + strconv.Itoa(maxN+1), prev, nil
}

// classSlugN — slug класса base: сам base (номер 1) или base-N; иное (base-gopls) — не класс.
func classSlugN(slug, base string) (int, bool) {
	if slug == base {
		return 1, true
	}
	rest, ok := strings.CutPrefix(slug, base+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 1
}

func tkRun(ctx context.Context, tk, dir string, stdin *strings.Reader, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tk, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tk %s в %s: %w: %s", args[0], dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sessionDir — папка тикетов сессии: таблица имён cl, папка админа, <проект>-master — ~/git или ~/work, лид — домашняя.
func (a *app) sessionDir(who, lead string) (string, error) {
	if who == lead {
		return a.home, nil
	}
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err != nil {
		return "", err
	}
	for _, dir := range slices.Sorted(maps.Keys(table)) {
		if table[dir] == who {
			return dir, nil
		}
	}
	c, err := a.config()
	if err != nil {
		return "", err
	}
	for _, dir := range slices.Sorted(maps.Keys(c.Admins)) {
		if c.Admins[dir] == who {
			return dir, nil
		}
	}
	if p, ok := strings.CutSuffix(who, "-master"); ok && p != "" {
		for _, space := range []string{"git", "work"} {
			dir := filepath.Join(a.home, space, p)
			if st, err := os.Stat(dir); err == nil && st.IsDir() && names.For(dir, a.home, table) == who {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("папка сессии %s не найдена (таблица имён cl, admins, ~/git|~/work/<проект>)", who)
}

func ioTicketTitle(job ioTicketJob) string {
	return "Фризы диска от " + job.class + " (aguard io)"
}

func ioTicketText(job ioTicketJob, prev string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "aguard io: случаи фриза диска (PSI full io), главный виновник — %s", job.class)
	if job.owner == "" {
		b.WriteString(", хозяина не нашлось")
	} else {
		fmt.Fprintf(&b, " у %s", job.owner)
	}
	b.WriteString(". Несрочные: письма нет (тикет alert-async в aguard); новые случаи класса — заметками сюда.\n")
	if strings.Contains(job.lead, " (за ") {
		fmt.Fprintf(&b, "Папка хозяина не найдена — тикет лиду: %s.\n", job.lead)
	}
	if job.why == ioroute.WhyOffline {
		b.WriteString("Был и срочный случай, хозяин не был в сети: лиду ушло письмо.\n")
	}
	if prev != "" {
		fmt.Fprintf(&b, "Прежний тикет класса: %s (закрыт).\n", prev)
	}
	b.WriteString("\nСлучаи:\n")
	writeRouteCases(&b, job.cases)
	fmt.Fprintf(&b, "\nПодробнее: aguard io report -since %s (виновники, процессы, cwd).\n", ioReportSince(job.cases[0].Start, time.Now()))
	b.WriteString("\n## Приёмка\nПричина найдена и убрана (или принята с доводом в журнале); `aguard io report -since 168h` неделю после правки — без случаев от 2 с этого класса у хозяина.\n")
	return b.String()
}

func ioTicketNote(job ioTicketJob) string {
	var b strings.Builder
	b.WriteString("aguard io: ещё ")
	b.WriteString(strconv.Itoa(len(job.cases)))
	b.WriteString(":")
	for _, c := range job.cases {
		b.WriteString(" ")
		b.WriteString(c.Text)
		b.WriteString(";")
	}
	return b.String()
}

func writeRouteCases(b *strings.Builder, cases []ioroute.Case) {
	for _, c := range cases {
		fmt.Fprintf(b, "- %s\n", c.Text)
	}
}

// ioRouteLetter — текст письма по действию маршрута; полная сводка главного случая — из кешированных сводок.
func ioRouteLetter(act ioroute.Action, full []iowatch.Case, alertMin time.Duration, owners []string) string {
	var b strings.Builder
	switch act.Why {
	case ioroute.WhyMachine:
		fmt.Fprintf(&b, "aguard io: общий вред машины — %d случаев от 2 с за окно серии у разных хозяев, ни у кого нет серии.\n", len(act.Cases))
	case ioroute.WhyCross:
		fmt.Fprintf(&b, "aguard io: класс %s фризит у разных хозяев (%s) — общая причина? Хозяевам ушли тикеты io-freeze-%s.\n",
			act.Class, strings.Join(owners, ", "), act.Class)
	case ioroute.WhyOffline:
		fmt.Fprintf(&b, "aguard io: вред сейчас, хозяин %s не в сети — тебе; ему тикет.\n", act.Cases[0].Owner)
	case ioroute.WhyVictim:
		fmt.Fprintf(&b, "aguard io: вред сейчас; главная cgroup %s в основном читала при подкачке не меньше половины чтения — может быть жертвой вытеснения, ей тикет, письмо тебе.\n",
			act.Cases[0].Owner)
	case ioroute.WhyNoOwner:
		b.WriteString("aguard io: вред сейчас, хозяина виновника нет.\n")
	case ioroute.WhyAfter:
		fmt.Fprintf(&b, "aguard io: вред идёт и после паузы писем — %d случаев.\n", len(act.Cases))
	}
	if act.Why == ioroute.WhyMachine || act.Why == ioroute.WhyAfter {
		writeRouteCases(&b, act.Cases)
	}
	if len(full) > 0 {
		text, _ := ioAlertText(full, alertMin)
		b.WriteString(text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// alertable — случай идёт в маршрут: от alert_min, не тихий и не под потолком.
func alertable(c *iowatch.Case, cfg iowatch.Config) bool {
	return c.Dur() >= cfg.AlertMin.D() && !c.Quiet && !c.Capped
}

// setRoute — пороги маршрута и поиск хозяев по конфигу цикла; состояние маршрута сохраняется.
func (s *ioService) setRoute(cfg iowatch.Config) {
	if s.route == nil {
		s.route = ioroute.New(cfg.Route())
	} else {
		s.route.SetConfig(cfg.Route())
	}
	s.owners = s.a.ioOwners(&cfg)
}

// seedRoute — маршрут по сохранённым случаям окна cross_window без действий: перезапуск (выкатка) не сбрасывает
// серии, паузы писем и сверку классов.
func (s *ioService) seedRoute(cfg iowatch.Config, now time.Time) {
	s.route = nil
	s.setRoute(cfg)
	from := now.Add(-max(cfg.CrossWindow.D(), cfg.AlertEvery.D(), cfg.SeriesWindow.D()))
	var seed []ioroute.Case
	for i := range s.cases {
		c := &s.cases[i]
		if c.End.Before(from) || !alertable(c, cfg) {
			continue
		}
		rc, err := s.owners.routeCase(c)
		if err != nil {
			s.a.warn("io", "хозяин случая при засеве маршрута", "err", err)
		}
		rc.Offline = c.OwnerOffline
		seed = append(seed, rc)
	}
	slices.SortFunc(seed, func(x, y ioroute.Case) int { return x.End.Compare(y.End) })
	s.route.Seed(seed, now)
	if len(seed) > 0 {
		s.a.log.Info("aguard io: маршрут засеян", "cases", len(seed), "pending", s.route.Pending())
	}
}

// routeOne — закрытый случай в маршрут; c — запись в кеше службы: хозяин не в сети сохраняется с ней для засева.
func (s *ioService) routeOne(c *iowatch.Case) {
	rc, err := s.owners.routeCase(c)
	if err != nil {
		s.a.warn("io", "хозяин случая не найден, случай — как без хозяина", "err", err)
	}
	alive := s.aliveFunc()
	c.OwnerOffline = rc.Owner != "" && !alive(rc.Owner)
	s.act(s.route.Route(rc, alive))
}

// aliveFunc — жива ли сессия по реестру сейчас; реестр не читается — в сети (письмо покажет).
func (s *ioService) aliveFunc() func(string) bool {
	live, regErr := s.a.reg.Alive()
	if regErr != nil {
		s.a.warn("io", "реестр сессий: хозяин считается в сети, письмо покажет", "err", regErr)
	}
	return func(name string) bool {
		_, ok := live[name]
		return ok || regErr != nil
	}
}

// act — действия маршрута: тикеты — в очередь, письма — своей горутиной каждое.
func (s *ioService) act(acts []ioroute.Action) {
	cfg := s.cfg
	for _, act := range acts {
		if act.Kind == ioroute.Summary {
			continue // только в сводку
		}
		if act.Kind == ioroute.Ticket {
			s.tickets.add(ioTicketJob{owner: act.Owner, class: act.Class, why: act.Why, cases: act.Cases,
				tk: cfg.Tk, lead: cfg.Lead, log: cfg.Mode == iowatch.ModeLog})
			continue
		}
		var owners []string
		if act.Why == ioroute.WhyCross {
			owners = s.route.Owners(act.Class)
		}
		msg := ioRouteLetter(act, s.fullCases(act.Cases), cfg.AlertMin.D(), owners)
		var dur int64
		for _, c := range act.Cases {
			dur = max(dur, c.DurMS)
		}
		go func() {
			defer func() {
				if v := recover(); v != nil {
					s.a.warn("io", "паника письма", "err", fmt.Sprint(v), "stack", string(debug.Stack()))
				}
			}()
			if s.a.ioSend(cfg, act.To, string(act.Why), msg, dur) || act.To == cfg.Lead {
				return
			}
			// Письмо хозяину не ушло (сессия кончилась после проверки, тишина) — случаи в его тикет: иначе не дойдут никуда.
			for _, t := range ioroute.Tickets(act.Cases) {
				if t.Kind == ioroute.Ticket {
					s.tickets.add(ioTicketJob{owner: t.Owner, class: t.Class, why: act.Why, cases: t.Cases,
						tk: cfg.Tk, lead: cfg.Lead, log: cfg.Mode == iowatch.ModeLog})
				}
			}
		}()
	}
}

// fullCases — сводки случаев действия из кеша службы (по началу).
func (s *ioService) fullCases(cases []ioroute.Case) []iowatch.Case {
	out := make([]iowatch.Case, 0, len(cases))
	for _, rc := range cases {
		for i := len(s.cases) - 1; i >= 0; i-- {
			if s.cases[i].Start.Equal(rc.Start) {
				out = append(out, s.cases[i])
				break
			}
		}
	}
	return out
}

// ioSend — письмо о фризе сессии to; mode = log — строка журнала «написал бы». Сбой и тишина — в журнал и учёт,
// false: письмо не дошло.
func (a *app) ioSend(cfg iowatch.Config, to, why, msg string, dur int64) bool {
	if cfg.Mode == iowatch.ModeLog {
		a.log.Info("aguard io: написал бы", "to", to, "why", why, "message", msg)
		a.note("io", "написал бы", why, to, dur, "ms")
		return true
	}
	alive, err := a.reg.Alive()
	if err == nil {
		err = a.sendAlive(alive, to, cfg.SendAuth, msg)
	}
	if err != nil {
		if a.quietFailure("io", err) {
			return false
		}
		a.warn("io", "письмо о фризе не ушло", "to", to, "err", err)
		a.note("io", "alert_failed", why, to, dur, "ms")
		return false
	}
	a.note("io", "alert_sent", why, to, dur, "ms")
	return true
}

// ioRouteReplay — маршрут по сохранённым случаям с since: сколько писем кому и почему, тикетов и заметок; рядом —
// сколько писем дала прежняя рассылка (лиду и копия виновнику, пачкой не чаще 15 мин). Хозяева считаются в сети,
// тикет класса — открытым всю глубину (вторая и дальше пачки — заметки). verbose — каждое письмо строкой.
func (a *app) ioRouteReplay(path string, live bool, since time.Time, verbose bool) error {
	cfg, err := a.ioConfig()
	if err != nil {
		return err
	}
	all, err := a.ioLoadCases(&cfg, path, live, since)
	if err != nil {
		return err
	}
	owners := a.ioOwners(&cfg)
	var cases []ioroute.Case
	var ownerErr error
	for i := range all {
		c := &all[i]
		if c.End.Before(since) || !alertable(c, cfg) {
			continue
		}
		rc, err := owners.routeCase(c)
		if err != nil && ownerErr == nil {
			ownerErr = err
		}
		cases = append(cases, rc)
	}
	slices.SortFunc(cases, func(x, y ioroute.Case) int { return x.End.Compare(y.End) })
	r := ioroute.New(cfg.Route())
	letters := map[string]int{}
	whys := map[ioroute.Why]int{}
	tickets := map[[2]string]int{}
	notes, summary := 0, 0
	var lines []string
	apply := func(acts []ioroute.Action, at time.Time) {
		for _, act := range acts {
			if act.Kind == ioroute.Summary {
				summary += len(act.Cases)
				continue
			}
			if act.Kind == ioroute.Ticket {
				k := [2]string{act.Owner, act.Class}
				if tickets[k] > 0 {
					notes++
				}
				tickets[k]++
				continue
			}
			letters[act.To]++
			whys[act.Why]++
			lines = append(lines, fmt.Sprintf("  %s → %s (%s): %s", at.Format("02.01 15:04"), act.To, act.Why, act.Cases[len(act.Cases)-1].Text))
		}
	}
	all1 := func(string) bool { return true }
	for _, c := range cases {
		apply(r.Tick(c.End), c.End)
		apply(r.Route(c, all1), c.End)
	}
	end := time.Now()
	apply(r.Tick(end.Add(cfg.AlertEvery.D()+cfg.SeriesWindow.D())), end)
	oldLead := ioroute.BatchLetters(cases, 15*time.Minute)
	total := 0
	for _, n := range letters {
		total += n
	}
	fmt.Fprintf(a.out, "aguard io route: случаев от %s с %s — %d\n", ioSeconds(cfg.AlertMin.D().Milliseconds()), since.Format("02.01 15:04"), len(cases))
	fmt.Fprintf(a.out, "было (лиду и копия виновнику, пачкой не чаще 15 мин): писем лиду %d\n", oldLead)
	fmt.Fprintf(a.out, "стало: писем %d, из них лиду %d; тикетов %d, заметок в них %d; без хозяина только в сводку — случаев %d\n",
		total, letters[cfg.Lead], len(tickets), notes, summary)
	fmt.Fprintf(a.out, "пороги: срочно от %s или %d-й случай хозяина за %s; письмо адресату не чаще %s; общий вред — %d случаев от %d хозяев\n",
		ioSeconds(cfg.UrgentMin.D().Milliseconds()), cfg.SeriesN, cfg.SeriesWindow.D(), cfg.AlertEvery.D(), cfg.MachineN, cfg.MachineOwners)
	fmt.Fprintln(a.out, "письма по адресатам:")
	for _, to := range slices.Sorted(maps.Keys(letters)) {
		fmt.Fprintf(a.out, "  %s: %d\n", to, letters[to])
	}
	fmt.Fprintln(a.out, "письма по причинам:")
	for _, why := range slices.Sorted(maps.Keys(whys)) {
		fmt.Fprintf(a.out, "  %s: %d\n", why, whys[why])
	}
	fmt.Fprintln(a.out, "тикеты (хозяин, класс: случаев):")
	keys := slices.SortedFunc(maps.Keys(tickets), func(x, y [2]string) int { return strings.Compare(x[0]+"/"+x[1], y[0]+"/"+y[1]) })
	for _, k := range keys {
		fmt.Fprintf(a.out, "  %s, io-freeze-%s: %d\n", k[0], k[1], tickets[k])
	}
	if verbose {
		fmt.Fprintln(a.out, "письма:")
		for _, l := range lines {
			fmt.Fprintln(a.out, l)
		}
	}
	if ownerErr != nil {
		return fmt.Errorf("хозяева не всех случаев найдены (такие — без хозяина): %w", ownerErr)
	}
	return nil
}

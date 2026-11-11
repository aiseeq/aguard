package policy

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// Class — класс создателя записи.
type Class string

// Классы создателей.
const (
	ClassSession  Class = "сессия"        // сама сессия Claude Code или её инструменты
	ClassFighter  Class = "боец"          // opencode, codex, claude -p под сессией
	ClassNonAgent Class = "не агент"      // процесс пользователя вне сессий Claude
	ClassOtherUID Class = "чужая учётка"  // другой uid (root, службы)
	ClassCache    Class = "кеш"           // общий кеш из списка, без владельца
	ClassUnknown  Class = "без создателя" // запись старше сторожа
)

// Classes — порядок вывода классов.
var Classes = []Class{ClassSession, ClassFighter, ClassNonAgent, ClassOtherUID, ClassCache, ClassUnknown}

// GiBMin — байт·секунд в одном «ГБ × минута».
const GiBMin = float64(1<<30) * 60

// Ignored — путь в списке игнорируемых.
func (c *Config) Ignored(path string) bool { return matchAny(c.Ignore, path) }

// IsCache — путь в списке общих кешей.
func (c *Config) IsCache(path string) bool { return matchAny(c.Cache.Paths, path) }

func matchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, path); ok { //nolint:ignored-error // единственная ошибка Match — ErrBadPattern, шаблоны проверены в Config.Validate
			return true
		}
	}
	return false
}

// Classify определяет класс создателя и имя бойца (если запись от бойца).
// myUID — учётка, от которой работает user-часть.
func (c *Config) Classify(path string, o *record.Owner, fileUID, myUID int) (Class, string) {
	if c.IsCache(path) {
		return ClassCache, ""
	}
	if o == nil {
		return ClassUnknown, ""
	}
	if o.Session != nil {
		if f := c.fighter(o); f != "" {
			return ClassFighter, f
		}
		return ClassSession, ""
	}
	uid := o.Proc.UID
	if uid < 0 {
		uid = fileUID
	}
	if uid == myUID {
		return ClassNonAgent, ""
	}
	return ClassOtherUID, ""
}

// fighter — первый боец в цепочке ниже сессии (включая сам процесс-создатель).
// Ближайший к сессии боец главнее: под opencode может работать node.
func (c *Config) fighter(o *record.Owner) string {
	found := ""
	check := func(pid int, comm string) {
		if pid == o.Session.Pid {
			return
		}
		for _, f := range c.Fighters {
			if comm == f {
				found = comm
			}
		}
	}
	check(o.Proc.Pid, o.Proc.Comm)
	for _, l := range o.Chain {
		check(l.Pid, l.Comm)
	}
	return found
}

// Item — живая запись с классом и ключом владельца.
type Item struct {
	Entry        record.Entry
	Class        Class
	Fighter      string
	OwnerKey     string
	WorkerMaster string
	WorkerCwd    string
	WorkerJobID  string
	WorkerLive   bool
}

// GBMin — текущий размер записи, умноженный на её возраст.
func (it *Item) GBMin(now time.Time) float64 {
	age := max(now.Sub(it.Entry.Born), 0)
	return float64(it.Entry.Bytes) * age.Seconds() / GiBMin
}

// Group — записи одного владельца.
type Group struct {
	Key   string
	Class Class
	Items []Item // по убыванию меры
	Bytes int64
	GBMin float64
}

// OwnerKey — по кому суммируются записи: сессия; для не агентов — команда;
// для записей без создателя и кешей — каждая запись сама по себе.
func OwnerKey(cl Class, e *record.Entry) string {
	switch cl {
	case ClassSession, ClassFighter:
		s := e.Owner.Session
		if s.ID != "" {
			return "s:" + s.ID
		}
		return "s:" + strconv.Itoa(s.Pid) + ":" + s.ProcStart
	case ClassNonAgent:
		return "c:" + e.Owner.Proc.Comm
	case ClassOtherUID:
		return "u:" + strconv.Itoa(e.UID) + ":" + e.Owner.Proc.Comm
	}
	return "e:" + e.Path
}

func workerUnit(cgroup string) string {
	unit := filepath.Base(cgroup)
	if strings.HasPrefix(unit, "worker-") && strings.HasSuffix(unit, ".service") {
		return unit
	}
	return ""
}

// ResolveWorkerJobs определяет заказчика и живость задания до группировки по порогу.
func ResolveWorkerJobs(items []Item, live Live) error {
	var errs []error
	for i := range items {
		it := &items[i]
		if it.Class != ClassNonAgent || it.Entry.Owner == nil {
			continue
		}
		unit := workerUnit(it.Entry.Owner.Proc.Cgroup)
		if unit == "" {
			continue
		}
		job, ok, err := live.WorkerByUnit(unit)
		if err != nil {
			errs = append(errs, fmt.Errorf("unit %s: %w", unit, err))
			continue
		}
		if !ok || job.Master == "" {
			continue
		}
		it.WorkerMaster, it.WorkerCwd, it.WorkerJobID, it.WorkerLive =
			job.Master, job.Cwd, job.ID, job.Live
		it.OwnerKey = "w:" + job.Master
	}
	return errors.Join(errs...)
}

// Items классифицирует живые записи, отбрасывая игнорируемые.
func (c *Config) Items(entries []record.Entry, myUID int) []Item {
	out := make([]Item, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		if c.Ignored(e.Path) {
			continue
		}
		cl, f := c.Classify(e.Path, e.Owner, e.UID, myUID)
		out = append(out, Item{Entry: *e, Class: cl, Fighter: f, OwnerKey: OwnerKey(cl, e)})
	}
	return out
}

// Over — владельцы сверх порога. factor < 1 ужесточает порог (давление памяти).
// Кеши сюда не попадают: у них своя чистка.
func (c *Config) Over(now time.Time, items []Item, factor float64) []Group {
	groups := map[string]*Group{}
	for i := range items {
		it := &items[i]
		if it.Class == ClassCache || it.Entry.Bytes < int64(c.Policy.MinEntry) {
			continue
		}
		g := groups[it.OwnerKey]
		if g == nil {
			g = &Group{Key: it.OwnerKey, Class: it.Class}
			groups[it.OwnerKey] = g
		}
		g.Items = append(g.Items, *it)
		g.Bytes += it.Entry.Bytes
		g.GBMin += it.GBMin(now)
	}
	budget := c.Policy.BudgetGBM * factor
	var out []Group
	for _, g := range groups {
		if g.Bytes < int64(c.Policy.MinOwner) || g.GBMin < budget {
			continue
		}
		sort.Slice(g.Items, func(i, j int) bool { return g.Items[i].GBMin(now) > g.Items[j].GBMin(now) })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GBMin != out[j].GBMin {
			return out[i].GBMin > out[j].GBMin
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// CreatorText — короткое описание создателя для сводки (у отчёта своё — report.creatorName).
func CreatorText(o *record.Owner) string {
	if o == nil {
		return "неизвестно (запись старше сторожа)"
	}
	name := ""
	if o.Session != nil {
		name = o.Session.Name
	}
	pid := " (pid " + strconv.Itoa(o.Proc.Pid) + ")"
	if name != "" && o.Session.Pid == o.Proc.Pid {
		// командная строка claude называет файл настроек (git-master.json), а не сессию — её читали как имя
		return "сессия " + name + pid
	}
	cmd := o.Proc.Args
	if cmd == "" {
		cmd = o.Proc.Comm
	}
	if cmd == "" {
		cmd = "?"
	}
	if r := []rune(cmd); len(r) > 90 {
		cmd = string(r[:90]) + "…"
	}
	text := strings.TrimSpace(cmd) + pid
	if name != "" {
		text += ", сессия " + name
	}
	return text
}

// LinesBytes — сумма размеров записей строк сводки.
func LinesBytes(ls []Line) int64 {
	var n int64
	for i := range ls {
		n += ls[i].Item.Entry.Bytes
	}
	return n
}

// OverText — «владельцев сверх порога N (записей M, X)»: у записей без
// создателя владелец — сама запись, поэтому числа могут совпадать.
func OverText(items []Item, groups []Group, factor float64) string {
	n := 0
	var b int64
	for i := range groups {
		n += len(groups[i].Items)
		b += groups[i].Bytes
	}
	return fmt.Sprintf("записей всего %d; владельцев сверх порога %d (их записей %d, %s); порог ×%s",
		len(items), len(groups), n, HumanBytes(b), FormatGBMin(factor))
}

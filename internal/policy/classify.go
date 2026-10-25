package policy

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"tmpguard/internal/record"
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
		if ok, _ := filepath.Match(p, path); ok {
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
	Entry    record.Entry
	Class    Class
	Fighter  string
	OwnerKey string
}

// GBMin — накопленная мера записи.
func (it *Item) GBMin() float64 { return it.Entry.ByteSec / GiBMin }

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

// Items классифицирует живые записи, отбрасывая игнорируемые.
func (c *Config) Items(entries []record.Entry, myUID int) []Item {
	out := make([]Item, 0, len(entries))
	for _, e := range entries {
		if c.Ignored(e.Path) {
			continue
		}
		cl, f := c.Classify(e.Path, e.Owner, e.UID, myUID)
		out = append(out, Item{Entry: e, Class: cl, Fighter: f, OwnerKey: OwnerKey(cl, &e)})
	}
	return out
}

// Over — владельцы сверх порога. factor < 1 ужесточает порог (давление памяти).
// Кеши сюда не попадают: у них своя чистка.
func (c *Config) Over(items []Item, factor float64) []Group {
	groups := map[string]*Group{}
	for _, it := range items {
		if it.Class == ClassCache || it.Entry.Bytes < int64(c.Policy.MinEntry) {
			continue
		}
		g := groups[it.OwnerKey]
		if g == nil {
			g = &Group{Key: it.OwnerKey, Class: it.Class}
			groups[it.OwnerKey] = g
		}
		g.Items = append(g.Items, it)
		g.Bytes += it.Entry.Bytes
		g.GBMin += it.GBMin()
	}
	budget := c.Policy.BudgetGBM * factor
	var out []Group
	for _, g := range groups {
		if g.Bytes < int64(c.Policy.MinOwner) || g.GBMin < budget {
			continue
		}
		sort.Slice(g.Items, func(i, j int) bool { return g.Items[i].Entry.ByteSec > g.Items[j].Entry.ByteSec })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GBMin > out[j].GBMin })
	return out
}

// CreatorText — короткое описание создателя для сводки и отчёта.
func CreatorText(o *record.Owner) string {
	if o == nil {
		return "неизвестно (запись старше сторожа)"
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
	return strings.TrimSpace(cmd) + " (pid " + strconv.Itoa(o.Proc.Pid) + ")"
}

package agentwait

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// GraftStats — журнал затрат graft (stats.jsonl в ~/.cache/graft) и его старая копия .1: graft сам пишет, сколько
// задача ждала и чего (очередь тяжёлых, нагрузка, своп, блокировка, sudo деплоя).
func GraftStats(home string) []string {
	f := filepath.Join(home, ".cache", "graft", "stats.jsonl")
	return []string{f + ".1", f}
}

// graftLine — строка задачи журнала graft; In пусто — задача верхнего уровня: её wait_s включает вложенные.
type graftLine struct {
	Time    time.Time          `json:"time"`
	Kind    string             `json:"kind"`
	Project string             `json:"project"`
	Top     string             `json:"top"`
	In      string             `json:"in"`
	WallS   float64            `json:"wall_s"`
	WaitS   float64            `json:"wait_s"`
	WaitWhy map[string]float64 `json:"wait_why"`
}

// Graft — ожидания задач graft за окно.
type Graft struct {
	CoveredFrom time.Time          `json:"covered_from,omitzero"` // самая ранняя строка журнала: раньше не видно
	Tasks       int                `json:"tasks"`                 // задач верхнего уровня за окно
	Waited      int                `json:"waited"`                // из них ждали дольше секунды
	WaitS       float64            `json:"wait_s"`
	MaxS        float64            `json:"max_s"`
	Why         map[string]float64 `json:"why"`    // секунды по причине; unknown — строки без разбивки
	ByTop       map[string]float64 `json:"by_top"` // секунды по команде graft (exec, commit, test…)
}

// ReadGraft — ожидания задач верхнего уровня с концом в [froms[i], to), по окну на каждое начало, за один проход
// журнала. Нет журнала — пусто; битые строки (оборванная последняя) пропускаются и считаются в bad.
func ReadGraft(files []string, froms []time.Time, to time.Time) ([]*Graft, Bad, error) {
	gs := make([]*Graft, len(froms))
	for i := range gs {
		gs[i] = &Graft{Why: map[string]float64{}, ByTop: map[string]float64{}}
	}
	var bad Bad
	for _, file := range files {
		err := readGraft(file, &bad, func(l *graftLine) {
			for i, g := range gs {
				g.add(l, froms[i], to)
			}
		})
		if err != nil {
			return gs, bad, err
		}
	}
	return gs, bad, nil
}

// Bad — битые строки: сколько и первая с причиной.
type Bad struct {
	N     int
	First string
}

func (b *Bad) add(n int, what string) {
	b.N += n
	if b.First == "" {
		b.First = what
	}
}

func readGraft(file string, bad *Bad, fn func(*graftLine)) error {
	fh, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("журнал graft: %w", err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for n := 1; sc.Scan(); n++ {
		var l graftLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			bad.add(1, fmt.Sprintf("%s:%d: %v", file, n, err))
			continue
		}
		fn(&l)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("журнал graft %s: %w", file, err)
	}
	return nil
}

// add — строка журнала в окно [from, to).
func (g *Graft) add(l *graftLine, from, to time.Time) {
	if g.CoveredFrom.IsZero() || l.Time.Before(g.CoveredFrom) {
		g.CoveredFrom = l.Time
	}
	if l.Kind != "task" || l.In != "" || l.Time.Before(from) || !l.Time.Before(to) {
		return
	}
	g.Tasks++
	if l.WaitS < 1 {
		return
	}
	g.Waited++
	g.WaitS += l.WaitS
	g.MaxS = max(g.MaxS, l.WaitS)
	g.ByTop[l.Top] += l.WaitS
	if l.WaitWhy == nil { // строки graft до 2.40 (до 25.10) пишут wait_s без разбивки
		g.Why[WhyUnknown] += l.WaitS
	}
	for why, s := range l.WaitWhy {
		g.Why[why] += s
	}
}

// WhyUnknown — причина ожидания в строке graft без wait_why.
const WhyUnknown = "unknown"

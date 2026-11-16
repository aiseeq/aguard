package iowatch

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Сводки случаев живут в памяти службы; файл — их копия при остановке, по запросу сводки и раз в час.

// Копии в папке состояния io: сводки случаев, посекундный ряд машины, профили cgroup — строка JSON на
// запись, файл пишется целиком.
const (
	whatCases    = "сводки случаев"
	whatSeries   = "ряд машины"
	whatProfiles = "профили cgroup"
)

type machineSecondFileFields MachineSecond

// MachineSecondFileRow хранит секунду в формате JSON для series.jsonl и ответа сокета series.
type MachineSecondFileRow struct {
	machineSecondFileFields
	GraftSliceMem *int64 `json:"graft_slice_mem,omitempty"`
}

// MachineSecondFileRowOf переводит секунду машины в её JSON-представление.
func MachineSecondFileRowOf(second MachineSecond) MachineSecondFileRow {
	row := MachineSecondFileRow{machineSecondFileFields: machineSecondFileFields(second)}
	if second.GraftSliceMemKnown || second.GraftSliceMem != 0 {
		value := second.GraftSliceMem
		row.GraftSliceMem = &value
	}
	return row
}

func (row MachineSecondFileRow) MachineSecond() MachineSecond {
	second := MachineSecond(row.machineSecondFileFields)
	second.GraftSliceMemKnown = row.GraftSliceMem != nil
	if row.GraftSliceMem == nil {
		second.GraftSliceMem = 0
	} else {
		second.GraftSliceMem = *row.GraftSliceMem
	}
	return second
}

// SaveCases пишет случаи в path целиком (через path.part и переименование).
func SaveCases(path string, cases []Case) error { return saveLines(path, whatCases, cases) }

// SaveSeries пишет секунды ряда в path целиком.
func SaveSeries(path string, seconds []MachineSecond) error {
	return saveLineWriter(path, whatSeries, func(enc *json.Encoder) error {
		for i := range seconds {
			row := MachineSecondFileRowOf(seconds[i])
			if err := enc.Encode(&row); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveProfiles пишет профили в path целиком.
func SaveProfiles(path string, profiles []Profile) error {
	return saveLines(path, whatProfiles, profiles)
}

// saveLines пишет записи строками JSON в path целиком (через path.*.part и переименование).
func saveLines[T any](path, what string, items []T) error {
	return saveLineWriter(path, what, func(enc *json.Encoder) error {
		for i := range items {
			if err := enc.Encode(&items[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func saveLineWriter(path, what string, encode func(*json.Encoder) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("папка %s: %w", what, err)
	}
	err := writeAtomic(path, func(w *bufio.Writer) error {
		return encode(json.NewEncoder(w))
	})
	if err != nil {
		return fmt.Errorf("%s %s: %w", what, path, err)
	}
	return nil
}

// writeAtomic пишет файл через свой временный рядом (path.*.part) и переименование: под именем
// path он появляется целиком, два писателя друг другу не мешают.
func writeAtomic(path string, write func(*bufio.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.part")
	if err != nil {
		return fmt.Errorf("временный файл: %w", err)
	}
	buf := bufio.NewWriterSize(f, 64*1024)
	err = write(buf)
	if err == nil {
		err = buf.Flush()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Chmod(f.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return errors.Join(err, os.Remove(f.Name()))
	}
	return nil
}

// LoadCases читает случаи из path; файла нет — пусто. Битая строка — ошибка: файл пишет только
// SaveCases целиком, порча — повод разобраться, а не молча потерять историю.
func LoadCases(path string) ([]Case, error) { return loadLines[Case](path, whatCases) }

// LoadSeries читает секунды ряда с At не раньше since, как LoadCases.
func LoadSeries(path string, since time.Time) ([]MachineSecond, error) {
	seconds, err := loadLinesMap[MachineSecond](path, whatSeries, true, func(dec *json.Decoder) (MachineSecond, error) {
		var row MachineSecondFileRow
		if err := dec.Decode(&row); err != nil {
			return MachineSecond{}, err
		}
		return row.MachineSecond(), nil
	})
	if err != nil {
		return nil, err
	}
	return keepSince(seconds, func(s *MachineSecond) time.Time { return s.At }, since), nil
}

// LoadProfiles читает профили с концом не раньше since, как LoadCases.
func LoadProfiles(path string, since time.Time) ([]Profile, error) {
	profiles, err := loadLines[Profile](path, whatProfiles)
	return keepSince(profiles, func(p *Profile) time.Time { return p.End }, since), err
}

// keepSince — записи с временем at не раньше since, на месте.
func keepSince[T any](items []T, at func(*T) time.Time, since time.Time) []T {
	n := 0
	for i := range items {
		if !at(&items[i]).Before(since) {
			items[n] = items[i]
			n++
		}
	}
	return items[:n]
}

// loadLines читает записи строками JSON из path; файла нет — пусто, битая запись — ошибка.
func loadLines[T any](path, what string) ([]T, error) {
	return loadLinesMap[T](path, what, false, func(dec *json.Decoder) (T, error) {
		var item T
		err := dec.Decode(&item)
		return item, err
	})
}

func loadLinesMap[T any](path, what string, countFirst bool, decode func(*json.Decoder) (T, error)) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer f.Close()
	var out []T
	if countFirst {
		count, err := countJSONLines(f)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", what, path, err)
		}
		if _, err := f.Seek(0, 0); err != nil {
			return nil, fmt.Errorf("%s %s: %w", what, path, err)
		}
		out = make([]T, 0, count)
	}
	dec := json.NewDecoder(bufio.NewReaderSize(f, 64*1024))
	for dec.More() {
		item, err := decode(dec)
		if err != nil {
			return nil, fmt.Errorf("%s %s, запись %d: %w", what, path, len(out)+1, err)
		}
		out = append(out, item)
	}
	return out, nil
}

func countJSONLines(f *os.File) (int, error) {
	reader := bufio.NewReaderSize(f, 64*1024)
	count := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		switch err {
		case nil:
			count++
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(fragment) > 0 {
				count++
			}
			return count, nil
		default:
			return 0, err
		}
	}
}

// MergeCases — случаи обоих списков по времени начала, без повторов (одно начало — один случай),
// не старше since.
func MergeCases(a, b []Case, since time.Time) []Case {
	out := make([]Case, 0, len(a)+len(b))
	out = append(append(out, a...), b...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	n := 0
	for i := range out {
		if out[i].Start.Before(since) || n > 0 && out[i].Start.Equal(out[n-1].Start) {
			continue
		}
		out[n] = out[i]
		n++
	}
	return out[:n]
}

// Summary — итог случаев за период: для агента и раздела Сна.
type Summary struct {
	Since    time.Time     `json:"since"`
	Until    time.Time     `json:"until"`
	Total    int           `json:"total"`
	IdleMS   int64         `json:"idle_ms"`
	Buckets  []Bucket      `json:"buckets"`
	Hours    []Hour        `json:"hours"`
	Main     []RankedCases `json:"main"` // все главные причины: счётчики покрывают случаи-фризы от min
	Culprits SumCulprit    `json:"culprits"`
	Cases    []Case        `json:"cases"` // от min, по убыванию длительности
	Min      int64         `json:"min_ms"`
	// Тихие случаи (Case.Quiet) — отдельно и ни в одно поле выше не входят.
	Quiet       int           `json:"quiet"`
	QuietIdleMS int64         `json:"quiet_idle_ms"`
	QuietWho    []RankedCases `json:"quiet_who"` // cgroup с наибольшим простоем: в скольких тихих случаях от min
	// Ожидание под потолком io.max (Case.Capped) — тоже отдельно, вне полей выше; CappedWho — потолок с наибольшим простоем.
	Capped       int           `json:"capped"`
	CappedIdleMS int64         `json:"capped_idle_ms"`
	CappedWho    []RankedCases `json:"capped_who"`
	// Waiting — случаи ожидания одной cgroup при низкой занятости устройства; не считаются фризами машины.
	Waiting       int           `json:"waiting"`
	WaitingIdleMS int64         `json:"waiting_idle_ms"`
	WaitingWho    []RankedCases `json:"waiting_who"`
	// Coverage — за какую часть [Since, Until) замер шёл (Cover); `aguard io report` задаёт всегда.
	Coverage *Coverage `json:"coverage,omitempty"`
}

// Bucket — случаи с длительностью в [FromMS, ToMS); ToMS = 0 — без верхней границы.
type Bucket struct {
	Label  string `json:"label"`
	FromMS int64  `json:"from_ms"`
	ToMS   int64  `json:"to_ms,omitempty"`
	Count  int    `json:"count"`
	IdleMS int64  `json:"idle_ms"`
}

// Hour — случаи, начавшиеся в этот час.
type Hour struct {
	Hour     time.Time `json:"hour"`
	Count    int       `json:"count"`
	Long     int       `json:"long"` // от min
	IdleMS   int64     `json:"idle_ms"`
	MaxDurMS int64     `json:"max_dur_ms"`
}

// RankedCases — имя, сумма меры по случаям и число случаев, где имя среди виновников.
type RankedCases struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
	Cases int    `json:"cases"`
}

// SumCulprit — виновники по случаям от min, до summaryTop имён на меру.
type SumCulprit struct {
	ProcWrite    []RankedCases `json:"proc_write"`
	ProcRead     []RankedCases `json:"proc_read"`
	ProcMajflt   []RankedCases `json:"proc_majflt"`
	CgroupWrite  []RankedCases `json:"cgroup_write"`
	CgroupRead   []RankedCases `json:"cgroup_read"`
	CgroupSwapIn []RankedCases `json:"cgroup_swapin"`
	CgroupIdle   []RankedCases `json:"cgroup_idle"`
}

const summaryTop = 10

// bucketEdges — границы корзин длительности, мс, и их подписи.
var (
	bucketEdges  = []int64{0, 300, 500, 1000, 2000, 5000}
	bucketLabels = []string{"<300 мс", "300–500 мс", "0,5–1 с", "1–2 с", "2–5 с", "≥5 с"}
)

// FilterCases — случаи, где who есть в пути главной cgroup, в имени процесса топов записи, чтения и свопа или в его
// cmdline (`aguard io report -who`); случай без виновников (короче case_min) не подходит; who пусто — все случаи.
func FilterCases(cases []Case, who string) []Case {
	if who == "" {
		return cases
	}
	out := []Case{}
	for i := range cases {
		if cases[i].Culprits.mentions(who) {
			out = append(out, cases[i])
		}
	}
	return out
}

// mentions — who в главной cgroup или в процессе топов (имя, cmdline).
func (c *Culprit) mentions(who string) bool {
	if c == nil {
		return false
	}
	if strings.Contains(c.Main, who) {
		return true
	}
	for _, list := range [][]Ranked{c.ProcWrite, c.ProcRead, c.ProcMajflt} {
		for _, p := range list {
			if strings.Contains(p.Name, who) || strings.Contains(c.ProcessDetails[p.Name].Cmdline, who) {
				return true
			}
		}
	}
	return false
}

// Summarize — итог случаев, начавшихся в [since, until); у случаев от min — виновники.
func Summarize(cases []Case, since, until time.Time, min time.Duration) Summary {
	s := Summary{Since: since, Until: until, Min: min.Milliseconds(), Buckets: make([]Bucket, len(bucketEdges)),
		Hours: []Hour{}, Main: []RankedCases{}, Cases: []Case{}, QuietWho: []RankedCases{}, CappedWho: []RankedCases{}, WaitingWho: []RankedCases{}}
	for i, from := range bucketEdges {
		s.Buckets[i].FromMS, s.Buckets[i].Label = from, bucketLabels[i]
		if i+1 < len(bucketEdges) {
			s.Buckets[i].ToMS = bucketEdges[i+1]
		}
	}
	hours := map[time.Time]*Hour{}
	main, quiet, capped, waiting := map[string]*RankedCases{}, map[string]*RankedCases{}, map[string]*RankedCases{}, map[string]*RankedCases{}
	acc := newCulpritAcc()
	for i := range cases {
		c := &cases[i]
		if c.Start.Before(since) || !c.Start.Before(until) {
			continue
		}
		if s.notFreeze(c, quiet, capped, waiting) {
			continue
		}
		s.Total++
		s.IdleMS += c.IdleMS
		b := &s.Buckets[bucketOf(c.DurMS)]
		b.Count++
		b.IdleMS += c.IdleMS
		hour := c.Start.Truncate(time.Hour)
		h := hours[hour]
		if h == nil {
			h = &Hour{Hour: hour}
			hours[hour] = h
		}
		h.Count++
		h.IdleMS += c.IdleMS
		h.MaxDurMS = max(h.MaxDurMS, c.DurMS)
		if c.Dur() < min {
			continue
		}
		h.Long++
		s.Cases = append(s.Cases, *c)
		if c.Culprits == nil {
			continue
		}
		name := ""
		switch {
		case c.Culprits.SnapshotDeletion():
			name = "btrfs-cleaner"
		case c.Culprits.Main != "":
			name = CgroupName(c.Culprits.Main)
		case c.Culprits.KernelOver():
			name = "фон ядра"
		}
		if name != "" {
			countCase(main, name, c.DurMS)
		}
		acc.add(c.Culprits)
	}
	for _, h := range hours {
		s.Hours = append(s.Hours, *h)
	}
	slices.SortFunc(s.Hours, func(a, b Hour) int { return a.Hour.Compare(b.Hour) })
	for _, m := range main {
		s.Main = append(s.Main, *m)
	}
	s.Main = rankCasesAll(s.Main)
	for _, q := range quiet {
		s.QuietWho = append(s.QuietWho, *q)
	}
	s.QuietWho = rankCases(s.QuietWho)
	for _, q := range capped {
		s.CappedWho = append(s.CappedWho, *q)
	}
	s.CappedWho = rankCases(s.CappedWho)
	for _, q := range waiting {
		s.WaitingWho = append(s.WaitingWho, *q)
	}
	s.WaitingWho = rankCases(s.WaitingWho)
	sort.SliceStable(s.Cases, func(i, j int) bool { return s.Cases[i].DurMS > s.Cases[j].DurMS })
	s.Culprits = acc.result()
	return s
}

// notFreeze — тихий случай, ожидание под потолком или локальное ожидание cgroup: учтён отдельно, во фризы не идёт.
func (s *Summary) notFreeze(c *Case, quiet, capped, waiting map[string]*RankedCases) bool {
	switch {
	case c.Quiet:
		s.Quiet++
		s.QuietIdleMS += c.IdleMS
		if c.Culprits != nil && len(c.Culprits.CgroupIdle) > 0 {
			countCase(quiet, c.Culprits.CgroupIdle[0].Name, c.DurMS)
		}
	case c.Capped:
		s.Capped++
		s.CappedIdleMS += c.IdleMS
		if c.Culprits != nil && len(c.Culprits.CappedIdle) > 0 {
			countCase(capped, c.Culprits.CappedIdle[0].Name, c.DurMS)
		}
	case c.Waiting:
		s.Waiting++
		s.WaitingIdleMS += c.IdleMS
		if c.Culprits != nil && c.Culprits.Main != "" {
			countCase(waiting, CgroupName(c.Culprits.Main), c.DurMS)
		}
	default:
		return false
	}
	return true
}

// countCase — ещё один случай имени с длительностью durMS.
func countCase(m map[string]*RankedCases, name string, durMS int64) {
	if m[name] == nil {
		m[name] = &RankedCases{Name: name}
	}
	m[name].Cases++
	m[name].Value += durMS
}

func bucketOf(durMS int64) int {
	i := len(bucketEdges) - 1
	for i > 0 && durMS < bucketEdges[i] {
		i--
	}
	return i
}

// rankCases — по числу случаев, затем по сумме меры, затем по имени; до summaryTop.
func rankCases(list []RankedCases) []RankedCases {
	list = rankCasesAll(list)
	return list[:min(summaryTop, len(list))]
}

func rankCasesAll(list []RankedCases) []RankedCases {
	slices.SortFunc(list, func(a, b RankedCases) int {
		if c := cmp.Compare(b.Cases, a.Cases); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Value, a.Value); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return list
}

// culpritAcc — суммы виновников по мерам.
type culpritAcc [7]map[string]*RankedCases

func newCulpritAcc() *culpritAcc {
	var a culpritAcc
	for i := range a {
		a[i] = map[string]*RankedCases{}
	}
	return &a
}

// add — суммы мер случая; процессы — по имени без pid: один и тот же бинарь в разных случаях.
func (a *culpritAcc) add(c *Culprit) {
	for i, list := range [...][]Ranked{c.ProcWrite, c.ProcRead, c.ProcMajflt, c.CgroupWrite, c.CgroupRead, c.CgroupSwapIn, c.CgroupIdle} {
		seen := map[string]bool{} // случай считается имени один раз, сколько бы его pid ни было в списке
		for _, r := range list {
			name := r.Name
			if i < 3 {
				name = ProcBase(name)
			}
			if a[i][name] == nil {
				a[i][name] = &RankedCases{Name: name}
			}
			a[i][name].Value += r.Value
			if !seen[name] {
				seen[name] = true
				a[i][name].Cases++
			}
		}
	}
}

func (a *culpritAcc) result() SumCulprit {
	var lists [7][]RankedCases
	for i := range a {
		lists[i] = []RankedCases{}
		for _, r := range a[i] {
			lists[i] = append(lists[i], *r)
		}
		lists[i] = rankCases(lists[i])
	}
	return SumCulprit{ProcWrite: lists[0], ProcRead: lists[1], ProcMajflt: lists[2], CgroupWrite: lists[3],
		CgroupRead: lists[4], CgroupSwapIn: lists[5], CgroupIdle: lists[6]}
}

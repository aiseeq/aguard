package iowatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/akit/proc"
)

// Profile — итог завершившейся cgroup-листа по детальным проходам: сколько жила, сколько прочла и
// записала, пик за проход, простой IO и своп, CPU и память. По профилям вида допуск тяжёлых задач
// знает, чего ждать от следующей такой же.
type Profile struct {
	Name         string    `json:"name"` // CgroupName
	Path         string    `json:"path"`
	Kind         string    `json:"kind"`  // ProfileKind
	Start        time.Time `json:"start"` // начало первого прохода, где cgroup была видна
	End          time.Time `json:"end"`   // последний проход, где она была видна
	DurMS        int64     `json:"dur_ms"`
	ReadB        int64     `json:"rd_b"`
	WriteB       int64     `json:"wr_b"`
	PeakReadBps  int64     `json:"peak_rd_bps"` // наибольшее чтение за проход, байт/с
	PeakWriteBps int64     `json:"peak_wr_bps"`
	IOFullMS     int64     `json:"io_full_ms"`     // PSI full io самой cgroup
	SwapIn       int64     `json:"swapin"`         // страниц
	CPUMS        int64     `json:"cpu_ms"`         // usage_usec листа за жизнь, мс CPU
	PeakCPUMilli int64     `json:"peak_cpu_milli"` // наибольшая за проход загрузка, тысячных ядра: 3650 — 3,65 ядра
	PeakAnonB    int64     `json:"peak_anon_b"`    // наибольший anon из memory.stat за проходы, байт
	Majflt       int64     `json:"majflt"`         // своего листа
	// MachineSwapIn — swap-in всей машины за жизнь профиля (быстрые шаги), страниц: не вина задачи,
	// а что было с машиной, пока она шла.
	MachineSwapIn int64 `json:"machine_swapin"`
	// Procs — до profileProcs процессов cgroup с наибольшей записью (имя — comm, как ProcBase), байт:
	// у бойца так видна приёмка отдельно от codex.
	Procs []Ranked `json:"procs,omitempty"`
}

// Dur — длительность профиля.
func (p *Profile) Dur() time.Duration { return time.Duration(p.DurMS) * time.Millisecond }

const (
	profileProcs    = 5
	profileMinBytes = 10 << 20         // профиль сохраняется от стольких байт чтения и записи…
	profileMinDur   = 60 * time.Second // …или от такой длительности…
	profileMinCPUMS = 60_000           // …или от минуты CPU (glint check, go test): иначе тысячи мелких scope
)

// cgroupAcc — накопленное по cgroup, пока она жива. swapFrom и swapTo — счётчик swap-in машины
// (Profiles.swap) у начала первого прохода и у последнего прохода, где cgroup была видна.
type cgroupAcc struct {
	first, last                          time.Time
	rd, wr, peakRd, peakWr, ioFull, swap int64
	cpuUS, peakCPU, peakAnon, majflt     int64
	swapFrom, swapTo                     int64
	procs                                map[string]int64 // comm → запись
}

// Profiles — накопление профилей по детальным проходам: cgroup, которые видны в проходах (с
// ненулевыми дельтами), до их конца. Принадлежит горутине замера и, как Series, живёт дольше цикла.
type Profiles struct {
	open map[string]*cgroupAcc
	// swap — swap-in машины по быстрым шагам с начала службы, страниц; passSwap — он же на прошлом
	// детальном проходе (начало идущего прохода).
	swap, passSwap int64
}

func NewProfiles() *Profiles { return &Profiles{open: map[string]*cgroupAcc{}} }

// step копит swap-in машины быстрого шага.
func (p *Profiles) step(s *Step) { p.swap += s.SwapIn }

// detail копит проход s длительностью interval: суммы, пики в байт/с и тысячных ядра, пик anon,
// запись процессов из топа.
func (p *Profiles) detail(s *Step, interval time.Duration) {
	for i := range s.Cgroups {
		g := &s.Cgroups[i]
		acc := p.open[g.Path]
		if acc == nil {
			acc = &cgroupAcc{first: s.At, swapFrom: p.passSwap}
			p.open[g.Path] = acc
		}
		acc.last, acc.swapTo = s.At, p.swap
		acc.rd += g.ReadBytes
		acc.wr += g.WriteBytes
		acc.peakRd = max(acc.peakRd, perSecond(g.ReadBytes, interval))
		acc.peakWr = max(acc.peakWr, perSecond(g.WriteBytes, interval))
		acc.ioFull += g.IOFullUS
		acc.swap += g.SwapIn
		acc.cpuUS += g.CPUUS
		acc.peakCPU = max(acc.peakCPU, perSecond(g.CPUUS, interval)/1000) // мкс CPU в секунду → тысячные ядра
		acc.peakAnon = max(acc.peakAnon, g.AnonB)
		acc.majflt += g.Majflt
	}
	p.passSwap = p.swap
	for i := range s.Processes {
		pr := &s.Processes[i]
		acc := p.open[pr.Cgroup]
		if acc == nil || pr.WriteBytes == 0 {
			continue
		}
		if acc.procs == nil {
			acc.procs = map[string]int64{}
		}
		acc.procs[pr.Comm] += pr.WriteBytes
	}
}

// perSecond — байт за interval в байт/с.
func perSecond(b int64, interval time.Duration) int64 {
	if interval <= 0 {
		return b
	}
	return int64(float64(b) * float64(time.Second) / float64(interval))
}

// Open — пути cgroup, которые копятся: их конец проверяет служба (папки нет в /sys/fs/cgroup).
func (p *Profiles) Open() []string {
	return slices.Sorted(maps.Keys(p.open))
}

// End закрывает кончившиеся cgroup (путь → вид) и отдаёт профили, которые стоит хранить: от
// profileMinBytes обмена, profileMinDur или profileMinCPUMS. Начало — начало первого прохода (за
// interval до него).
func (p *Profiles) End(gone map[string]string, interval time.Duration) []Profile {
	var out []Profile
	for _, path := range slices.Sorted(maps.Keys(gone)) {
		acc := p.open[path]
		if acc == nil {
			continue
		}
		delete(p.open, path)
		start := acc.first.Add(-interval)
		pr := Profile{Name: CgroupName(path), Path: path, Kind: gone[path], Start: start, End: acc.last,
			DurMS: acc.last.Sub(start).Milliseconds(), ReadB: acc.rd, WriteB: acc.wr, PeakReadBps: acc.peakRd,
			PeakWriteBps: acc.peakWr, IOFullMS: acc.ioFull / 1000, SwapIn: acc.swap, CPUMS: acc.cpuUS / 1000,
			PeakCPUMilli: acc.peakCPU, PeakAnonB: acc.peakAnon, Majflt: acc.majflt, MachineSwapIn: acc.swapTo - acc.swapFrom}
		if pr.ReadB+pr.WriteB < profileMinBytes && pr.Dur() < profileMinDur && pr.CPUMS < profileMinCPUMS {
			continue
		}
		if len(acc.procs) > 0 {
			pr.Procs = Rank(acc.procs, profileProcs)
		}
		out = append(out, pr)
	}
	SortProfiles(out)
	return out
}

// SortProfiles — по концу, при равенстве порядок сохраняется.
func SortProfiles(list []Profile) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].End.Before(list[j].End) })
}

// CgroupGone — папки cgroup (путь от корня cgroup) больше нет: она кончилась.
func CgroupGone(root, path string) (bool, error) {
	_, err := os.Stat(filepath.Join(root, path))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODEV) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("cgroup %s: %w", path, err)
	}
	return false, nil
}

// Виды профилей. Лёгкие задачи graft идут в cgroup сессии агента (agent), своего листа у них нет. Тяжёлая работа
// graft (heavy-задача, `graft exec --heavy`) переезжает в свой scope graft-heavy-<pid>.scope, а вид задачи — тот, что
// graft шлёт в now (`<repo>:<task>`, у exec — `<repo>:exec:<программа>`), — кладёт в файл <GraftHeavy>/<имя scope>
// (встречный тикет git/graft/heavy-scope-kind-file): профиль копится под этим видом.
const (
	KindAgent         = "agent"
	kindWorkerPrefix  = "worker:"
	kindServicePrefix = "service:"
	// KindWorkerUnknown — боец без класса в job.json: вид известен, класс нет.
	KindWorkerUnknown = kindWorkerPrefix + "?"
	// graftHeavyPrefix — scope тяжёлой работы graft (graft internal/iosched: graft-heavy-<pid>.scope).
	graftHeavyPrefix = "graft-heavy-"
)

// KindSources — откуда ProfileKind берёт вид сверх пути cgroup.
type KindSources struct {
	WorkerState string // ~/.local/state/worker: класс бойца из <job>/job.json
	GraftHeavy  string // $XDG_RUNTIME_DIR/graft/heavy: вид задачи graft в файле по имени scope
}

// GraftHeavyDir — папка, куда graft кладёт вид задачи своих тяжёлых scope.
func GraftHeavyDir(runtime string) string { return filepath.Join(runtime, "graft", "heavy") }

// KindUnknownError — вид годен, но неполон: боец без класса (worker:?) или scope graft без файла вида
// (service:<unit>). Проход не ошибка: вызывающий предупреждает по Source.
type KindUnknownError struct {
	Source string // KindSourceWorker или KindSourceGraft
	Err    error
}

// Источники неполного вида (KindUnknownError.Source).
const (
	KindSourceWorker = "worker-class"     // нет класса бойца в job.json
	KindSourceGraft  = "graft-heavy-kind" // нет файла вида задачи graft для scope
)

func (e *KindUnknownError) Error() string { return e.Err.Error() }
func (e *KindUnknownError) Unwrap() error { return e.Err }

// ProfileKind — вид cgroup: unit бойца `worker-<job>.service`, повтор `worker-<job>-<N>.service` — `worker:<класс>` из
// <WorkerState>/<job>/job.json (id задания — cpuwatch.JobIDs; нет файла или поля — `worker:?`, причина — в unknown:
// вид известен, класс нет, проход не ошибка); scope сессии агента — agent; scope graft-heavy-<pid>.scope — вид задачи
// graft из <GraftHeavy>/<scope> (нет файла — `service:<unit>`, причина — в unknown); иначе `service:<unit>` (unit —
// systemd-сервис пути или последний компонент; служба worker-settle — тоже).
func ProfileKind(path string, src KindSources, unknown func(*KindUnknownError)) string {
	if _, _, ok := proc.SessionScope(path); ok {
		return KindAgent
	}
	unit := proc.ServiceUnit(path)
	if ids := cpuwatch.JobIDs(unit); ids != nil {
		var err error
		for _, id := range ids {
			var class string
			class, err = workerClass(filepath.Join(src.WorkerState, id, "job.json"))
			if err == nil {
				return kindWorkerPrefix + class
			}
			if !errors.Is(err, fs.ErrNotExist) { // файл задания есть, класса нет: соседняя папка — чужое задание
				break
			}
		}
		unknown(&KindUnknownError{Source: KindSourceWorker, Err: err})
		return KindWorkerUnknown
	}
	if unit == "" {
		unit = filepath.Base(filepath.Clean(path))
	}
	if strings.HasPrefix(unit, graftHeavyPrefix) && strings.HasSuffix(unit, ".scope") {
		kind, err := graftKind(filepath.Join(src.GraftHeavy, unit))
		if err == nil {
			return kind
		}
		unknown(&KindUnknownError{Source: KindSourceGraft, Err: err})
	}
	return kindServicePrefix + unit
}

// graftKind — вид задачи graft из файла scope: одна непустая строка без пробелов.
func graftKind(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("вид задачи graft: %w", err)
	}
	kind := strings.TrimSpace(string(b))
	if kind == "" || strings.ContainsAny(kind, " \t\n") {
		return "", fmt.Errorf("вид задачи graft %s: ждали одну строку без пробелов, а там %q", path, kind)
	}
	return kind, nil
}

// workerClass — поле class из job.json бойца.
func workerClass(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("класс бойца: %w", err)
	}
	var job struct {
		Class string `json:"class"`
	}
	if err := json.Unmarshal(b, &job); err != nil {
		return "", fmt.Errorf("класс бойца %s: %w", path, err)
	}
	if job.Class == "" {
		return "", fmt.Errorf("класс бойца %s: поля class нет", path)
	}
	return job.Class, nil
}

// ProfileStats — свод профилей одного вида: для допуска одним запросом now.
type ProfileStats struct {
	Kind          string    `json:"kind"`
	Count         int       `json:"count"`
	PeakWriteBps  Quantiles `json:"peak_wr_bps"`
	WriteB        Quantiles `json:"wr_b"`
	DurMS         Quantiles `json:"dur_ms"`
	CPUMS         Quantiles `json:"cpu_ms"`
	PeakCPUMilli  Quantiles `json:"peak_cpu_milli"`
	PeakAnonB     Quantiles `json:"peak_anon_b"`
	Majflt        Quantiles `json:"majflt"`
	MachineSwapIn Quantiles `json:"machine_swapin"`
	Last          time.Time `json:"last"` // конец последнего профиля
}

// profileStatFields — поля профиля, которые сводятся квантилями, и их место в ProfileStats.
var profileStatFields = [...]struct {
	value func(*Profile) int64
	stat  func(*ProfileStats) *Quantiles
}{
	{func(p *Profile) int64 { return p.PeakWriteBps }, func(s *ProfileStats) *Quantiles { return &s.PeakWriteBps }},
	{func(p *Profile) int64 { return p.WriteB }, func(s *ProfileStats) *Quantiles { return &s.WriteB }},
	{func(p *Profile) int64 { return p.DurMS }, func(s *ProfileStats) *Quantiles { return &s.DurMS }},
	{func(p *Profile) int64 { return p.CPUMS }, func(s *ProfileStats) *Quantiles { return &s.CPUMS }},
	{func(p *Profile) int64 { return p.PeakCPUMilli }, func(s *ProfileStats) *Quantiles { return &s.PeakCPUMilli }},
	{func(p *Profile) int64 { return p.PeakAnonB }, func(s *ProfileStats) *Quantiles { return &s.PeakAnonB }},
	{func(p *Profile) int64 { return p.Majflt }, func(s *ProfileStats) *Quantiles { return &s.Majflt }},
	{func(p *Profile) int64 { return p.MachineSwapIn }, func(s *ProfileStats) *Quantiles { return &s.MachineSwapIn }},
}

// Quantiles — медиана и 90-й перцентиль (ближайший ранг).
type Quantiles struct {
	P50 int64 `json:"p50"`
	P90 int64 `json:"p90"`
}

// SummarizeProfiles — свод по видам.
func SummarizeProfiles(profiles []Profile) map[string]ProfileStats {
	byKind := map[string][]*Profile{}
	for i := range profiles {
		byKind[profiles[i].Kind] = append(byKind[profiles[i].Kind], &profiles[i])
	}
	out := make(map[string]ProfileStats, len(byKind))
	for kind, list := range byKind {
		st := ProfileStats{Kind: kind, Count: len(list)}
		for _, p := range list {
			if p.End.After(st.Last) {
				st.Last = p.End
			}
		}
		values := make([]int64, len(list))
		for _, f := range profileStatFields {
			for i, p := range list {
				values[i] = f.value(p)
			}
			*f.stat(&st) = quantiles(values)
		}
		out[kind] = st
	}
	return out
}

// quantiles — p50 и p90 по ближайшему рангу; values сортируются на месте.
func quantiles(values []int64) Quantiles {
	slices.Sort(values)
	rank := func(p float64) int64 {
		i := int(math.Ceil(p*float64(len(values)))) - 1
		return values[max(0, min(len(values)-1, i))]
	}
	return Quantiles{P50: rank(.5), P90: rank(.9)}
}

// FilterProfiles — профили вида kind (пусто — все) с концом не раньше since.
func FilterProfiles(profiles []Profile, kind string, since time.Time) []Profile {
	out := []Profile{}
	for i := range profiles {
		if (kind == "" || profiles[i].Kind == kind) && !profiles[i].End.Before(since) {
			out = append(out, profiles[i])
		}
	}
	return out
}

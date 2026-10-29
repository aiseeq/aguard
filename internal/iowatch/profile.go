package iowatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aiseeq/akit/proc"
)

// Profile — итог завершившейся cgroup-листа по детальным проходам: сколько жила, сколько прочла и
// записала, пик за проход, простой IO и своп. По профилям вида допуск тяжёлых задач знает, чего ждать
// от следующей такой же.
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
	IOFullMS     int64     `json:"io_full_ms"` // PSI full io самой cgroup
	SwapIn       int64     `json:"swapin"`     // страниц
	// Procs — до profileProcs процессов cgroup с наибольшей записью (имя — comm, как ProcBase), байт:
	// у бойца так видна приёмка отдельно от codex.
	Procs []Ranked `json:"procs,omitempty"`
}

// Dur — длительность профиля.
func (p *Profile) Dur() time.Duration { return time.Duration(p.DurMS) * time.Millisecond }

const (
	profileProcs    = 5
	profileMinBytes = 10 << 20         // профиль сохраняется от стольких байт чтения и записи…
	profileMinDur   = 60 * time.Second // …или от такой длительности: иначе тысячи мелких scope
)

// cgroupAcc — накопленное по cgroup, пока она жива.
type cgroupAcc struct {
	first, last                          time.Time
	rd, wr, peakRd, peakWr, ioFull, swap int64
	procs                                map[string]int64 // comm → запись
}

// Profiles — накопление профилей по детальным проходам: cgroup, которые видны в проходах (с
// ненулевыми дельтами), до их конца. Принадлежит горутине замера и, как Series, живёт дольше цикла.
type Profiles struct {
	open map[string]*cgroupAcc
}

func NewProfiles() *Profiles { return &Profiles{open: map[string]*cgroupAcc{}} }

// detail копит проход s длительностью interval: суммы, пики в байт/с, запись процессов из топа.
func (p *Profiles) detail(s *Step, interval time.Duration) {
	for i := range s.Cgroups {
		g := &s.Cgroups[i]
		acc := p.open[g.Path]
		if acc == nil {
			acc = &cgroupAcc{first: s.At}
			p.open[g.Path] = acc
		}
		acc.last = s.At
		acc.rd += g.ReadBytes
		acc.wr += g.WriteBytes
		acc.peakRd = max(acc.peakRd, perSecond(g.ReadBytes, interval))
		acc.peakWr = max(acc.peakWr, perSecond(g.WriteBytes, interval))
		acc.ioFull += g.IOFullUS
		acc.swap += g.SwapIn
	}
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
	out := make([]string, 0, len(p.open))
	for path := range p.open {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// End закрывает кончившиеся cgroup (путь → вид) и отдаёт профили, которые стоит хранить: от
// profileMinBytes обмена или profileMinDur. Начало — начало первого прохода (за interval до него).
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
			PeakWriteBps: acc.peakWr, IOFullMS: acc.ioFull / 1000, SwapIn: acc.swap}
		if pr.ReadB+pr.WriteB < profileMinBytes && pr.Dur() < profileMinDur {
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

// Виды профилей. Вид graft (`graft:<проект>/<задача>`) займёт своё место, когда граф шагов придёт из
// журнала graft: сейчас его cgroup — service.
const (
	KindAgent         = "agent"
	kindWorkerPrefix  = "worker:"
	kindServicePrefix = "service:"
	// KindWorkerUnknown — боец без класса в job.json: вид известен, класс нет.
	KindWorkerUnknown = kindWorkerPrefix + "?"
)

// ProfileKind — вид cgroup: unit бойца `worker-<job>.service` — `worker:<класс>` из
// <workerState>/<job>/job.json (нет файла или поля — `worker:?`, причина — в noClass: вид известен,
// класс нет, проход не ошибка); scope сессии агента — agent; иначе `service:<unit>` (unit —
// systemd-сервис пути или последний компонент).
func ProfileKind(path, workerState string, noClass func(error)) string {
	if _, _, ok := proc.SessionScope(path); ok {
		return KindAgent
	}
	unit := proc.ServiceUnit(path)
	if job, ok := strings.CutPrefix(unit, "worker-"); ok {
		job = strings.TrimSuffix(job, ".service")
		class, err := workerClass(filepath.Join(workerState, job, "job.json"))
		if err != nil {
			noClass(err)
			return KindWorkerUnknown
		}
		return kindWorkerPrefix + class
	}
	if unit == "" {
		unit = filepath.Base(filepath.Clean(path))
	}
	return kindServicePrefix + unit
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
	Kind         string    `json:"kind"`
	Count        int       `json:"count"`
	PeakWriteBps Quantiles `json:"peak_wr_bps"`
	WriteB       Quantiles `json:"wr_b"`
	DurMS        Quantiles `json:"dur_ms"`
	Last         time.Time `json:"last"` // конец последнего профиля
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
		peak, write, dur := make([]int64, len(list)), make([]int64, len(list)), make([]int64, len(list))
		for i, p := range list {
			peak[i], write[i], dur[i] = p.PeakWriteBps, p.WriteB, p.DurMS
			if p.End.After(st.Last) {
				st.Last = p.End
			}
		}
		st.PeakWriteBps, st.WriteB, st.DurMS = quantiles(peak), quantiles(write), quantiles(dur)
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

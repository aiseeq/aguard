package cpuwatch

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/policy"
)

// Режимы — те же, что у bgtask: ModeOff — выключено, ModeLog — только журнал «написал бы», ModeAlert — письма
// мастерам и лиду.
const (
	ModeOff   = bgtask.ModeOff
	ModeLog   = bgtask.ModeLog
	ModeAlert = bgtask.ModeAlert
)

// Config — раздел [cpu] конфига aguard.
type Config struct {
	Mode     string          `toml:"mode"`
	Interval policy.Duration `toml:"interval"` // как часто замер
	Window   policy.Duration `toml:"window"`   // перегрузка — занятые замеры держатся столько
	Share    float64         `toml:"share"`    // доля занятых замеров в окне, с которой перегрузка
	Repeat   policy.Duration `toml:"repeat"`   // письма одному адресату за перегрузку — не чаще
	Notify   float64         `toml:"notify"`   // мастеру пишется, если его доля готовых задач не меньше (и самому большому)
	Hosts    []string        `toml:"hosts"`    // машины по ssh сверх своей
	Workers  string          `toml:"workers"`  // папка заданий worker
	Lead     string          `toml:"-"`
	SendAuth bool            `toml:"-"`
}

// Default — замер раз в 15 с, перегрузка — 70 % занятых замеров за 10 минут, повтор не чаще раза в 30 минут, PC по
// ssh. 10 минут и 30 — по журналу sar за 19–27.10 (замер раз в 10 минут): load за минуту не ниже 22 — 38 отрезков,
// из них 14 дольше одного замера, самый долгий — 30 минут; короче 10 минут — всплеск, который очередь graft переждёт.
func Default(home string) Config {
	return Config{
		Mode: ModeAlert, Interval: policy.Duration(15 * time.Second), Window: policy.Duration(10 * time.Minute),
		Share: 0.7, Repeat: policy.Duration(30 * time.Minute), Notify: 0.25, Hosts: []string{"pc"},
		Workers: home + "/.local/state/worker",
	}
}

// Validate проверяет раздел.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("cpu.mode = %q: off, log или alert", c.Mode)
	}
	if c.Interval.D() < 5*time.Second || c.Window.D() < 2*c.Interval.D() {
		return errors.New("cpu.interval >= 5s и cpu.window >= двух интервалов")
	}
	if c.Share <= 0 || c.Share > 1 || c.Notify < 0 || c.Notify > 1 {
		return errors.New("cpu.share в (0, 1], cpu.notify в [0, 1]")
	}
	c.Workers = policy.ExpandHome(c.Workers, home)
	return nil
}

// point — замер в окне: готовые потоки по владельцам и по их процессам.
type point struct {
	at      time.Time
	busy    bool
	running int
	threads int
	by      map[Owner]int
	comms   map[Owner]map[string]int
}

// Host — окно замеров одной машины и её перегрузка.
type Host struct {
	Name   string // как машину называть в письмах: «ноут», «pc»
	points []point
	since  time.Time            // начало перегрузки; нуль — машина не перегружена
	sent   map[string]time.Time // кому и когда писали в эту перегрузку
	Down   bool                 // прошлый замер не удался: о сбое пишется раз, а не каждый проход
}

// Add кладёт замер в окно и выбрасывает замеры старше window.
func (h *Host) Add(s *Sample, owner func(*Task) Owner, window time.Duration) {
	p := point{at: s.At, busy: s.Busy(), running: s.Verdict.Load.Runnable, threads: s.Verdict.Load.Threads,
		by: map[Owner]int{}, comms: map[Owner]map[string]int{}}
	for i := range s.Tasks {
		t := &s.Tasks[i]
		o := owner(t)
		p.by[o] += t.N
		if p.comms[o] == nil {
			p.comms[o] = map[string]int{}
		}
		p.comms[o][t.Comm] += t.N
	}
	h.points = append(h.points, p)
	cut := 0
	for cut < len(h.points) && s.At.Sub(h.points[cut].at) > window {
		cut++
	}
	h.points = h.points[cut:]
}

// Load — готовые задачи владельца в среднем по занятым замерам окна.
type Load struct {
	Owner Owner
	Avg   float64
	Comms []Comm
}

// Comm — процесс владельца и его готовые потоки в среднем.
type Comm struct {
	Name string
	Avg  float64
}

// Verdict — перегрузка машины по окну.
type Verdict struct {
	Over    bool      // перегружена: окно полное, занятых замеров не меньше доли share
	Started bool      // перегрузка началась в этот замер
	Ended   time.Time // перегрузка кончилась в этот замер: когда началась
	Since   time.Time // начало перегрузки (первый занятый замер окна)
	Busy    int       // занятых замеров в окне
	Total   int
	Running float64 // готовых к работе задач в среднем по занятым замерам
	Threads int
	Loads   []Load // по владельцам, больше — раньше
}

// Judge — перегрузка по окну на момент now. Окно неполное (замеры идут меньше 90 % window) — не перегружена.
func (h *Host) Judge(now time.Time, window time.Duration, share float64) Verdict {
	var v Verdict
	v.Total = len(h.points)
	full := v.Total > 0 && now.Sub(h.points[0].at) >= window*9/10
	sum := map[Owner]int{}
	comms := map[Owner]map[string]int{}
	for i := range h.points {
		p := &h.points[i]
		if !p.busy {
			continue
		}
		if v.Busy == 0 {
			v.Since = p.at
		}
		v.Busy++
		v.Running += float64(p.running)
		v.Threads = p.threads
		for o, n := range p.by {
			sum[o] += n
			if comms[o] == nil {
				comms[o] = map[string]int{}
			}
			for c, k := range p.comms[o] {
				comms[o][c] += k
			}
		}
	}
	if v.Busy > 0 {
		v.Running /= float64(v.Busy)
		for o, n := range sum {
			l := Load{Owner: o, Avg: float64(n) / float64(v.Busy)}
			for c, k := range comms[o] {
				l.Comms = append(l.Comms, Comm{Name: c, Avg: float64(k) / float64(v.Busy)})
			}
			slices.SortFunc(l.Comms, func(a, b Comm) int { return cmp.Or(cmp.Compare(b.Avg, a.Avg), cmp.Compare(a.Name, b.Name)) })
			v.Loads = append(v.Loads, l)
		}
		slices.SortFunc(v.Loads, func(a, b Load) int { return cmp.Or(cmp.Compare(b.Avg, a.Avg), ownerCmp(a.Owner, b.Owner)) })
	}
	v.Over = full && float64(v.Busy) >= share*float64(v.Total)
	switch {
	case v.Over && h.since.IsZero():
		h.since, h.sent, v.Started = v.Since, map[string]time.Time{}, true
	case !v.Over && !h.since.IsZero():
		v.Ended, h.since, h.sent = h.since, time.Time{}, nil
	}
	if v.Over {
		v.Since = h.since
	}
	return v
}

func ownerCmp(a, b Owner) int {
	return cmp.Or(cmp.Compare(a.Master, b.Master), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Label, b.Label))
}

// Master — готовые задачи одного мастера: его сессия, бойцы, ферма.
type Master struct {
	Name  string
	Avg   float64
	Parts []Load
}

// Masters — нагрузка по мастерам (больше — раньше) и вся сумма готовых потоков в среднем.
func (v *Verdict) Masters() (ms []Master, total float64) {
	idx := map[string]int{}
	for _, l := range v.Loads {
		total += l.Avg
		if l.Owner.Master == "" {
			continue
		}
		i, ok := idx[l.Owner.Master]
		if !ok {
			i = len(ms)
			idx[l.Owner.Master] = i
			ms = append(ms, Master{Name: l.Owner.Master})
		}
		ms[i].Avg += l.Avg
		ms[i].Parts = append(ms[i].Parts, l)
	}
	slices.SortStableFunc(ms, func(a, b Master) int { return cmp.Or(cmp.Compare(b.Avg, a.Avg), cmp.Compare(a.Name, b.Name)) })
	return ms, total
}

// Recipients — кому писать: мастера с долей готовых задач не меньше notify и самый большой.
func (v *Verdict) Recipients(notify float64) []Master {
	ms, total := v.Masters()
	var out []Master
	for i, m := range ms {
		if i == 0 || (total > 0 && m.Avg/total >= notify) {
			out = append(out, m)
		}
	}
	return out
}

// Seen — адресату to в эту перегрузку уже писали.
func (h *Host) Seen(to string) bool {
	_, ok := h.sent[to]
	return ok
}

// Due — письмо адресату to сейчас: в эту перегрузку ему не писали дольше repeat. Отмечает письмо.
func (h *Host) Due(to string, now time.Time, repeat time.Duration) bool {
	if h.sent == nil {
		return false
	}
	if last, ok := h.sent[to]; ok && now.Sub(last) < repeat {
		return false
	}
	h.sent[to] = now
	return true
}

// MasterMessage — письмо мастеру m о перегрузке машины host.
func MasterMessage(host string, v *Verdict, m *Master, now time.Time, first bool) string {
	var b strings.Builder
	state := "перегружен"
	if !first {
		state = "всё ещё перегружен"
	}
	fmt.Fprintf(&b, "aguard: %s %s %s — готовых к работе задач в среднем %s при %d потоках. Твоих из них %s: %s.",
		host, state, bgtask.Span(now.Sub(v.Since)), num(v.Running), v.Threads, num(m.Avg), parts(m.Parts))
	b.WriteString(" Ничего не останавливаю и не понижаю: посмотри, почему твои задачи дают столько, и не добавляй новых")
	if host == HostLocal {
		b.WriteString("; тяжёлое — на PC (`worker run --pc`, `worker pc --`)")
	}
	b.WriteString(".")
	return b.String()
}

// LeadMessage — сводка лиду: чьи готовые задачи и кому написано.
func LeadMessage(host string, v *Verdict, to []string, now time.Time) string {
	ms, total := v.Masters()
	var who []string
	for _, m := range ms {
		who = append(who, m.Name+" "+num(m.Avg))
	}
	byKind := map[string]float64{}
	for _, l := range v.Loads {
		if l.Owner.Master == "" {
			byKind[l.Owner.Kind] += l.Avg
		}
	}
	for _, k := range slices.Sorted(maps.Keys(byKind)) {
		if n := byKind[k]; n >= 0.5 {
			who = append(who, k+" "+num(n))
		}
	}
	sent := "никому"
	if len(to) > 0 {
		sent = strings.Join(to, ", ")
	}
	return fmt.Sprintf("aguard: %s перегружен %s — готовых к работе задач в среднем %s при %d потоках (по процессам %s). Чьи: %s. Написал: %s.",
		host, bgtask.Span(now.Sub(v.Since)), num(v.Running), v.Threads, num(total), strings.Join(who, ", "), sent)
}

// HostLocal — своя машина в письмах.
const HostLocal = "ноут"

// parts — «боец r-t14 — 18 (compile 18); сессия — 6 (go 4, claude 2)»: доли меньше половины потока не пишутся.
func parts(ls []Load) string {
	var out []string
	for _, l := range ls {
		if l.Avg < 0.5 {
			continue
		}
		name := l.Owner.Kind
		if l.Owner.Label != "" {
			name += " " + l.Owner.Label
		}
		var cs []string
		for _, c := range l.Comms {
			if c.Avg >= 0.5 && len(cs) < 3 {
				cs = append(cs, c.Name+" "+num(c.Avg))
			}
		}
		s := name + " — " + num(l.Avg)
		if len(cs) > 0 {
			s += " (" + strings.Join(cs, ", ") + ")"
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "меньше потока"
	}
	return strings.Join(out, "; ")
}

func num(x float64) string { return fmt.Sprint(int(math.Round(x))) }

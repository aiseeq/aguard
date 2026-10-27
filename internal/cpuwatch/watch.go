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
	Interval policy.Duration `toml:"interval"` // как часто читать load average и, в перегрузку, снимать готовые потоки
	Repeat   policy.Duration `toml:"repeat"`   // письма одному адресату за перегрузку — не чаще
	Notify   float64         `toml:"notify"`   // мастеру пишется, если его доля готовых задач не меньше (и самому большому)
	Hosts    []string        `toml:"hosts"`    // машины по ssh сверх своей
	Workers  string          `toml:"workers"`  // папка заданий worker
	Lead     string          `toml:"-"`
	SendAuth bool            `toml:"-"`
}

// Default — load average читается раз в минуту (за 5 минут он за минуту меняется мало), повтор письма не чаще раза в
// 30 минут, PC по ssh.
func Default(home string) Config {
	return Config{
		Mode: ModeAlert, Interval: policy.Duration(time.Minute), Repeat: policy.Duration(30 * time.Minute), Notify: 0.25,
		Hosts: []string{"pc"}, Workers: home + "/.local/state/worker",
	}
}

// Validate проверяет раздел.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("cpu.mode = %q: off, log или alert", c.Mode)
	}
	if c.Interval.D() < 10*time.Second || c.Repeat.D() < c.Interval.D() {
		return errors.New("cpu.interval >= 10s и cpu.repeat >= cpu.interval")
	}
	if c.Notify < 0 || c.Notify > 1 {
		return errors.New("cpu.notify в [0, 1]")
	}
	c.Workers = policy.ExpandHome(c.Workers, home)
	return nil
}

// snap — снимок готовых потоков в перегрузку: по владельцам и по их процессам.
type snap struct {
	by    map[Owner]int
	comms map[Owner]map[string]int
}

func newSnap(tasks []Task, owner func(*Task) Owner) snap {
	p := snap{by: map[Owner]int{}, comms: map[Owner]map[string]int{}}
	for i := range tasks {
		t := &tasks[i]
		o := owner(t)
		p.by[o] += t.N
		if p.comms[o] == nil {
			p.comms[o] = map[string]int{}
		}
		p.comms[o][t.Comm] += t.N
	}
	return p
}

// Host — перегрузка одной машины: с какого замера длится, снимки готовых потоков за неё и кому уже писали.
type Host struct {
	Name  string // как машину называть в письмах: «ноут», «pc»
	snaps []snap
	since time.Time            // начало перегрузки; нуль — машина не перегружена
	sent  map[string]time.Time // кому и когда писали о машине: переживает конец перегрузки, иначе перегрузка у порога
	// (кончилась — снова началась) сбрасывала бы repeat и слала письма каждый замер (ревью 5572a22, 27.10)
	told map[string]bool // кому писали в эту перегрузку
	Down bool            // прошлый замер не удался: о сбое пишется раз, а не каждый проход
}

// endShare — перегрузка кончается, когда load average за 5 минут ниже этой доли потоков, а не просто ниже потоков:
// у порога она не дробится на десяток коротких.
const endShare = 0.9

// Load — готовые задачи владельца в среднем по снимкам перегрузки.
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

// Verdict — перегрузка машины на момент замера.
type Verdict struct {
	Over    bool      // перегружена: load average за 5 минут не меньше потоков
	Started bool      // перегрузка началась в этот замер
	Ended   time.Time // перегрузка кончилась в этот замер: когда началась
	Since   time.Time // начало перегрузки
	Avg5    float64   // load average за 5 минут
	Threads int
	Snaps   int    // снимков готовых потоков за перегрузку
	Loads   []Load // по владельцам, больше — раньше
}

// Judge — перегрузка по замеру s. В перегрузку кладёт снимок готовых потоков s.Tasks с владельцами owner; доли
// владельцев — в среднем по снимкам всей перегрузки. Кончилась — снимки и отметки писем забываются.
func (h *Host) Judge(s *Sample, owner func(*Task) Owner) Verdict {
	v := Verdict{Over: s.Over(), Avg5: s.Load.Avg5, Threads: s.Threads}
	if !h.since.IsZero() && s.Load.Avg5 >= endShare*float64(s.Threads) {
		v.Over = true
	}
	switch {
	case v.Over && h.since.IsZero():
		h.since, h.told, v.Started = s.At, map[string]bool{}, true
	case !v.Over && !h.since.IsZero():
		v.Ended, h.since, h.told, h.snaps = h.since, time.Time{}, nil, nil
	}
	if !v.Over {
		return v
	}
	v.Since = h.since
	h.snaps = append(h.snaps, newSnap(s.Tasks, owner))
	v.Snaps, v.Loads = len(h.snaps), loads(h.snaps)
	return v
}

// Owners — готовые потоки одного снимка по владельцам, без суждения о перегрузке: для проб.
func Owners(tasks []Task, owner func(*Task) Owner) []Load {
	return loads([]snap{newSnap(tasks, owner)})
}

// loads — готовые потоки владельцев в среднем по снимкам, больше — раньше.
func loads(snaps []snap) []Load {
	if len(snaps) == 0 {
		return nil
	}
	sum := map[Owner]int{}
	comms := map[Owner]map[string]int{}
	for _, p := range snaps {
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
	n := float64(len(snaps))
	var out []Load
	for o, k := range sum {
		l := Load{Owner: o, Avg: float64(k) / n}
		for c, m := range comms[o] {
			l.Comms = append(l.Comms, Comm{Name: c, Avg: float64(m) / n})
		}
		slices.SortFunc(l.Comms, func(a, b Comm) int { return cmp.Or(cmp.Compare(b.Avg, a.Avg), cmp.Compare(a.Name, b.Name)) })
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b Load) int { return cmp.Or(cmp.Compare(b.Avg, a.Avg), ownerCmp(a.Owner, b.Owner)) })
	return out
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

// Recipients — кому писать: мастера с долей готовых задач не меньше notify. Перегрузку дают система и прочее —
// мастеру с потоком из двадцати не пишется «посмотри, почему твои задачи дают столько», только сводка лиду.
func (v *Verdict) Recipients(notify float64) []Master {
	ms, total := v.Masters()
	var out []Master
	for _, m := range ms {
		if total > 0 && m.Avg/total >= notify {
			out = append(out, m)
		}
	}
	return out
}

// Seen — адресату to в эту перегрузку уже писали.
func (h *Host) Seen(to string) bool { return h.told[to] }

// Due — письмо адресату to сейчас: машина перегружена и о ней ему не писали дольше repeat. Отмечает письмо. Ключ
// to — адресат и вид письма: сводка лиду и письмо ему же как мастеру считаются порознь.
func (h *Host) Due(to string, now time.Time, repeat time.Duration) bool {
	if h.since.IsZero() {
		return false
	}
	if last, ok := h.sent[to]; ok && now.Sub(last) < repeat {
		return false
	}
	if h.sent == nil {
		h.sent = map[string]time.Time{}
	}
	h.sent[to], h.told[to] = now, true
	return true
}

// MasterMessage — письмо мастеру m о перегрузке машины host.
func MasterMessage(host string, v *Verdict, m *Master, now time.Time, first bool) string {
	var b strings.Builder
	state := "перегружен"
	if !first {
		state = "всё ещё перегружен"
	}
	fmt.Fprintf(&b, "aguard: %s %s %s — load average за 5 минут %s при %d потоках. Готовых к работе задач твоих в среднем %s: %s.",
		host, state, bgtask.Span(now.Sub(v.Since)), dec(v.Avg5), v.Threads, num(m.Avg), parts(m.Parts))
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
	return fmt.Sprintf("aguard: %s перегружен %s — load average за 5 минут %s при %d потоках. Готовых к работе задач в среднем %s по %d снимкам, чьи: %s. Написал: %s.",
		host, bgtask.Span(now.Sub(v.Since)), dec(v.Avg5), v.Threads, num(total), v.Snaps, strings.Join(who, ", "), sent)
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

// dec — load average с одним знаком: 23.4.
func dec(x float64) string { return fmt.Sprintf("%.1f", x) }

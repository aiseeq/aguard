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
	After    policy.Duration `toml:"after"`    // письма — когда перегрузка держится подряд не меньше
	Repeat   policy.Duration `toml:"repeat"`   // письма одному адресату о машине — не чаще
	Notify   float64         `toml:"notify"`   // мастеру — если его готовых задач в снимке не меньше этой доли потоков машины
	Hosts    []string        `toml:"hosts"`    // машины по ssh сверх своей
	Workers  string          `toml:"workers"`  // папка заданий worker
	Lead     string          `toml:"-"`
	SendAuth bool            `toml:"-"`
}

// Default — замер раз в минуту (как и load average за минуту), письма — после 10 минут перегрузки подряд, повтор не
// чаще раза в 30 минут, мастеру — от четверти потоков машины (ноут — 6 задач из 22), PC по ssh.
func Default(home string) Config {
	return Config{
		Mode: ModeAlert, Interval: policy.Duration(time.Minute), After: policy.Duration(10 * time.Minute),
		Repeat: policy.Duration(30 * time.Minute), Notify: 0.25, Hosts: []string{"pc"}, Workers: home + "/.local/state/worker",
	}
}

// Validate проверяет раздел.
func (c *Config) Validate(home string) error {
	if c.Mode != ModeOff && c.Mode != ModeLog && c.Mode != ModeAlert {
		return fmt.Errorf("cpu.mode = %q: off, log или alert", c.Mode)
	}
	if c.Interval.D() < 10*time.Second || c.Repeat.D() < c.Interval.D() || c.After.D() < 0 {
		return errors.New("cpu.interval >= 10s, cpu.repeat >= cpu.interval, cpu.after >= 0")
	}
	if c.Notify <= 0 || c.Notify > 1 {
		return errors.New("cpu.notify в (0, 1]")
	}
	c.Workers = policy.ExpandHome(c.Workers, home)
	return nil
}

// Host — перегрузка одной машины: с какого замера длится и кому уже писали.
type Host struct {
	Name  string               // как машину называть в письмах: «ноут», «pc»
	since time.Time            // начало перегрузки: первый из замеров подряд с перегрузкой; нуль — не перегружена
	sent  map[string]time.Time // кому и когда писали о машине: переживает конец перегрузки, иначе перегрузка у порога
	// (кончилась — снова началась) сбрасывала бы repeat и слала письма каждый замер (ревью 5572a22, 27.10)
	told map[string]bool // кому писали в эту перегрузку
	Down bool            // прошлый замер не удался: о сбое пишется раз, а не каждый проход
}

// Load — готовые потоки владельца в снимке.
type Load struct {
	Owner Owner
	N     int
	Comms []Comm
}

// Comm — процесс владельца и его готовые потоки.
type Comm struct {
	Name string
	N    int
}

// Verdict — перегрузка машины на момент замера: load average за минуту и готовые задачи снимка не меньше потоков,
// длительность — сколько замеров подряд это держится (лид 28.10 ~00:05 CET по слову пользователя; прежние LA за 5 минут
// с порогом конца 0.9 потоков запаздывали: нагрузка уже кончилась, а письмо получал тот, кто крутился в момент снимка).
type Verdict struct {
	Over     bool      // перегружена в этот замер
	Started  bool      // перегрузка началась в этот замер
	Ended    time.Time // перегрузка кончилась в этот замер: когда началась
	Since    time.Time // начало перегрузки: первый замер из подряд идущих
	Avg1     float64   // load average за минуту
	Runnable int       // готовых к работе задач в момент снимка
	Threads  int
	Loads    []Load // снимок по владельцам, больше — раньше
}

// Long — перегрузка держится подряд не меньше after: о ней пора писать.
func (v *Verdict) Long(now time.Time, after time.Duration) bool {
	return v.Over && now.Sub(v.Since) >= after
}

// Judge — перегрузка по замеру s; в перегрузку — владельцы готовых потоков снимка s.Tasks. Кончилась — забывается,
// кому писали в эту перегрузку; когда писали — помнится (repeat).
func (h *Host) Judge(s *Sample, owner func(*Task) Owner) Verdict {
	v := Verdict{Over: s.Over(), Avg1: s.Load.Avg1, Runnable: s.Runnable, Threads: s.Threads}
	switch {
	case v.Over && h.since.IsZero():
		h.since, h.told, v.Started = s.At, map[string]bool{}, true
	case !v.Over && !h.since.IsZero():
		v.Ended, h.since, h.told = h.since, time.Time{}, nil
	}
	if !v.Over {
		return v
	}
	v.Since, v.Loads = h.since, Owners(s.Tasks, owner)
	return v
}

// Owners — готовые потоки снимка по владельцам, больше — раньше.
func Owners(tasks []Task, owner func(*Task) Owner) []Load {
	by := map[Owner]int{}
	comms := map[Owner]map[string]int{}
	for i := range tasks {
		t := &tasks[i]
		o := owner(t)
		by[o] += t.N
		if comms[o] == nil {
			comms[o] = map[string]int{}
		}
		comms[o][t.Comm] += t.N
	}
	var out []Load
	for o, n := range by {
		l := Load{Owner: o, N: n}
		for c, k := range comms[o] {
			l.Comms = append(l.Comms, Comm{Name: c, N: k})
		}
		slices.SortFunc(l.Comms, func(a, b Comm) int { return cmp.Or(cmp.Compare(b.N, a.N), cmp.Compare(a.Name, b.Name)) })
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b Load) int { return cmp.Or(cmp.Compare(b.N, a.N), ownerCmp(a.Owner, b.Owner)) })
	return out
}

func ownerCmp(a, b Owner) int {
	return cmp.Or(cmp.Compare(a.Master, b.Master), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Label, b.Label))
}

// Master — готовые потоки одного мастера в снимке: его сессия, бойцы, ферма.
type Master struct {
	Name  string
	N     int
	Parts []Load
}

// Masters — готовые потоки снимка по мастерам (больше — раньше) и всего по процессам.
func (v *Verdict) Masters() (ms []Master, total int) {
	idx := map[string]int{}
	for _, l := range v.Loads {
		total += l.N
		if l.Owner.Master == "" {
			continue
		}
		i, ok := idx[l.Owner.Master]
		if !ok {
			i = len(ms)
			idx[l.Owner.Master] = i
			ms = append(ms, Master{Name: l.Owner.Master})
		}
		ms[i].N += l.N
		ms[i].Parts = append(ms[i].Parts, l)
	}
	slices.SortStableFunc(ms, func(a, b Master) int { return cmp.Or(cmp.Compare(b.N, a.N), cmp.Compare(a.Name, b.Name)) })
	return ms, total
}

// Recipients — кому писать: мастера, у которых готовых задач в снимке не меньше доли notify потоков машины. Доля от
// всех готовых не годится: при трёх готовых и одна — «четверть» (лид 27.10: «Доля ≥25% от трёх задач — не основание
// для письма»).
func (v *Verdict) Recipients(notify float64) []Master {
	ms, _ := v.Masters()
	var out []Master
	for _, m := range ms {
		if float64(m.N) >= notify*float64(v.Threads) {
			out = append(out, m)
		}
	}
	return out
}

// Told — сессии, которым писали в эту перегрузку: без служебных ключей «вид:адресат» (сводка лиду, строка журнала).
func (h *Host) Told() []string {
	var out []string
	for to := range h.told {
		if !strings.Contains(to, ":") {
			out = append(out, to)
		}
	}
	slices.Sort(out)
	return out
}

// Due — письмо адресату to сейчас: машина перегружена и о ней ему не писали дольше repeat. Ключ to — адресат и вид
// письма: сводка лиду и письмо ему же как мастеру считаются порознь. Отметка — Sent, после отправки: не дошедшее
// письмо repeat не откладывает.
func (h *Host) Due(to string, now time.Time, repeat time.Duration) bool {
	if h.since.IsZero() {
		return false
	}
	last, ok := h.sent[to]
	return !ok || now.Sub(last) >= repeat
}

// First — адресату to в эту перегрузку ещё не писали.
func (h *Host) First(to string) bool { return !h.told[to] }

// Sent отмечает письмо адресату to.
func (h *Host) Sent(to string, now time.Time) {
	if h.sent == nil {
		h.sent = map[string]time.Time{}
	}
	if h.told == nil {
		h.told = map[string]bool{}
	}
	h.sent[to], h.told[to] = now, true
}

// MasterMessage — письмо мастеру m о перегрузке машины host.
func MasterMessage(host string, v *Verdict, m *Master, now time.Time, first bool) string {
	var b strings.Builder
	state := "перегружен"
	if !first {
		state = "всё ещё перегружен"
	}
	fmt.Fprintf(&b, "aguard: %s %s %s подряд — load average за минуту %s, сейчас готовых к работе задач %d при %d потоках. Твоих сейчас %d: %s.",
		host, state, bgtask.Span(now.Sub(v.Since)), dec(v.Avg1), v.Runnable, v.Threads, m.N, parts(m.Parts))
	b.WriteString(" Ничего не останавливаю и не понижаю: посмотри, почему твои задачи дают столько, и не добавляй новых")
	if host == HostLocal {
		b.WriteString("; тяжёлое — на PC (`worker run --pc`, `worker pc --`)")
	}
	b.WriteString(".")
	return b.String()
}

// LeadMessage — сводка лиду: чьи готовые задачи и кому за перегрузку написано.
func LeadMessage(host string, v *Verdict, told []string, now time.Time) string {
	ms, total := v.Masters()
	var who []string
	for _, m := range ms {
		who = append(who, m.Name+" "+fmt.Sprint(m.N))
	}
	byKind := map[string]int{}
	for _, l := range v.Loads {
		if l.Owner.Master == "" {
			byKind[l.Owner.Kind] += l.N
		}
	}
	for _, k := range slices.Sorted(maps.Keys(byKind)) {
		who = append(who, k+" "+fmt.Sprint(byKind[k]))
	}
	sent := "никому"
	if len(told) > 0 {
		sent = strings.Join(told, ", ")
	}
	return fmt.Sprintf("aguard: %s перегружен %s подряд — load average за минуту %s, сейчас готовых к работе задач %d при %d потоках (по процессам %d). Чьи: %s. Написал за перегрузку: %s.",
		host, bgtask.Span(now.Sub(v.Since)), dec(v.Avg1), v.Runnable, v.Threads, total, strings.Join(who, ", "), sent)
}

// HostLocal — своя машина в письмах.
const HostLocal = "ноут"

// parts — «боец r-t14 — 18 (compile 18); сессия — 6 (go 4, claude 2)».
func parts(ls []Load) string {
	var out []string
	for _, l := range ls {
		name := l.Owner.Kind
		if l.Owner.Label != "" {
			name += " " + l.Owner.Label
		}
		var cs []string
		for _, c := range l.Comms {
			if len(cs) < 3 {
				cs = append(cs, c.Name+" "+fmt.Sprint(c.N))
			}
		}
		out = append(out, name+" — "+fmt.Sprint(l.N)+" ("+strings.Join(cs, ", ")+")")
	}
	return strings.Join(out, "; ")
}

// dec — load average с одним знаком: 23.4.
func dec(x float64) string { return fmt.Sprintf("%.1f", math.Round(x*10)/10) }

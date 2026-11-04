package cpuwatch

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/bgtask"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

// Процессы без родителя (тикет stray-processes, задание лида 03.11 14:15 CET: сирота теста 31,6 ч ела 100 % ядра на PC, а
// заметил пользователь в htop; живой образец от git-admin — та же сирота с CPU 0 %). Перегрузка машины такого не видит:
// одно ядро из 32 не даёт порога Over; сторож orphans — только на ноуте и только по CLAUDE_PID, которого у процессов из
// ssh нет. Правило — два случая: остаток сессии ssh или входа (session-*.scope) старше After при любой загрузке, и
// другой scope, где законные процессы без родителя бывают (контейнеры, фон): дерево занимает не меньше Hot ядра на
// каждом проходе подряд After. Не считаются: главный процесс приложения сеанса (его запускает desktop, родитель init по
// замыслу) и дерево с живой сессией агента или законным фоном (tmux) внутри (занятость — их работа, снимать нельзя;
// 03.11 konsole со всеми мастерами). Демон входа (живая местная сессия, родился вместе с ней: лид 04.11, ksecretd от PAM,
// снимать нельзя) не остаток: идёт под правилом занятости. Удалённая сессия так не считается: мультиплексор ssh держит
// её долго, а первая команда в ней родилась вместе с сессией — это и есть сироты тестов.
// Только письмо, ничего не снимает.

// StrayConfig — раздел [cpu.strays].
type StrayConfig struct {
	Every   policy.Duration `toml:"every"`   // как часто смотреть
	After   policy.Duration `toml:"after"`   // возраст остатка сессии и срок занятости дерева
	Hot     float64         `toml:"hot"`     // занятость дерева в ядрах, с которой другой scope считается
	Repeat  policy.Duration `toml:"repeat"`  // повтор письма о том же процессе; с него — копия лиду
	Exclude string          `toml:"exclude"` // regexp по comm целиком: законный фон
}

// DefaultStrays — раз в 5 мин: письмо не позже 35 мин от начала; порог и срок — признаки из задания лида.
// tmux и screen сами отцепляются от родителя, catatonit держит namespace podman.
func DefaultStrays() StrayConfig {
	return StrayConfig{Every: policy.Duration(5 * time.Minute), After: policy.Duration(30 * time.Minute), Hot: 0.5,
		Repeat: policy.Duration(6 * time.Hour), Exclude: "tmux.*|screen|SCREEN|catatonit"}
}

// Validate проверяет раздел и разбирает Exclude.
func (c *StrayConfig) Validate() error {
	if c.Every.D() < time.Minute || c.After.D() < c.Every.D() || c.Repeat.D() < c.After.D() {
		return errors.New("cpu.strays: every >= 1m, after >= every, repeat >= after")
	}
	if c.Hot <= 0 {
		return errors.New("cpu.strays.hot > 0")
	}
	if _, err := regexp.Compile(c.Exclude); err != nil {
		return fmt.Errorf("cpu.strays.exclude: %w", err)
	}
	return nil
}

// Proc — свой процесс машины в проходе.
type Proc struct {
	Pid, PPid int
	Age       time.Duration
	CPU       float64 // секунд процессора за жизнь
	Comm      string
}

// Detail — процесс без родителя: то, что нужно письму и адресату.
type Detail struct {
	Pid, Start int
	Cgroup     string
	Cwd        string
	Claude     int
	SSH        string
	Cmd        string
	Login      bool          // процесс в живой местной сессии logind, а не в ушедшей и не в удалённой
	LoginAge   time.Duration // возраст этой сессии
}

// sessionAwk — возраст сессии logind в секундах по её файлу, если она живая и местная; иначе пусто.
const sessionAwk = `/^STATE=/{s=$2} /^REMOTE=/{r=$2} /^REALTIME=/{t=$2} END{if((s=="active"||s=="online")&&r=="0"&&t!="") printf "%d", now-t/1000000}`

// sessionLookup — по cgroup в $g возраст сессии в $l: сессия без места — session-c1.scope, файл c1.
const sessionLookup = `l=''
	case "$g" in */session-*.scope)
		n=${g##*/session-}
		l=$(awk -F= -v now="$(date +%s)" '` + sessionAwk + `' "/run/systemd/sessions/${n%.scope}" 2>/dev/null)
		;;
	esac
`

// StrayScript — один замер машины: свои процессы и подробности тех, чей родитель — init или systemd --user, кроме служб
// (systemd за ними следит сам). Поля — через табуляцию; разбирает ParseStrays. Без временных файлов: на PC идёт по ssh.
const StrayScript = `all=$(ps -u "$(id -u)" -o pid=,ppid=,etimes=,cputimes=,comm=) || exit 1
echo '##procs'
printf '%s\n' "$all"
echo '##strays'
printf '%s\n' "$all" | awk '$5=="systemd"{m[$1]=1} {p[NR]=$1; q[NR]=$2} END{for(i=1;i<=NR;i++) if(q[i]==1||m[q[i]]) print p[i]}' | while read -r p; do
	s=$(cat /proc/$p/stat 2>/dev/null) || continue
	s=${s##*) }
	set -- $s
	st=${20}
	g=$(sed -n 's/^0:://p' /proc/$p/cgroup 2>/dev/null)
	case "$g" in *.service|*/init.scope|'') continue ;; esac
	w=$(readlink /proc/$p/cwd 2>/dev/null)
	e=$(tr '\0' '\n' </proc/$p/environ 2>/dev/null)
	cl=$(printf '%s\n' "$e" | sed -n 's/^CLAUDE_PID=//p')
	ss=$(printf '%s\n' "$e" | sed -n 's/^SSH_CONNECTION=//p')
	c=$(tr '\0\t\n' '   ' </proc/$p/cmdline 2>/dev/null | cut -c1-200)
	` + sessionLookup + `	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$p" "$st" "$g" "$w" "$cl" "$ss" "$c" "$l"
done
echo '##end'
`

// ParseStrays разбирает вывод StrayScript; нет ##end — вывод оборван, ошибка.
func ParseStrays(raw []byte) ([]Proc, []Detail, error) {
	_, rest, ok := bytes.Cut(raw, []byte("##procs\n"))
	if !ok {
		return nil, nil, errors.New("замер процессов: нет раздела ##procs")
	}
	procs, rest, ok := bytes.Cut(rest, []byte("##strays\n"))
	if !ok {
		return nil, nil, errors.New("замер процессов: нет раздела ##strays")
	}
	details, _, ok := bytes.Cut(rest, []byte("##end\n"))
	if !ok {
		return nil, nil, errors.New("замер процессов оборван: нет ##end")
	}
	ps, err := parseProcs(string(procs))
	if err != nil {
		return nil, nil, err
	}
	ds, err := parseDetails(string(details))
	if err != nil {
		return nil, nil, err
	}
	return ps, ds, nil
}

func parseProcs(s string) ([]Proc, error) {
	var out []Proc
	for l := range strings.SplitSeq(s, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if len(f) < 5 {
			return nil, fmt.Errorf("замер процессов: строка %q", l)
		}
		nums := make([]int, 4)
		for i := range nums {
			n, err := strconv.Atoi(f[i])
			if err != nil {
				return nil, fmt.Errorf("замер процессов: %q: %w", l, err)
			}
			nums[i] = n
		}
		out = append(out, Proc{Pid: nums[0], PPid: nums[1], Age: time.Duration(nums[2]) * time.Second, CPU: float64(nums[3]),
			Comm: strings.Join(f[4:], " ")})
	}
	return out, nil
}

func parseDetails(s string) ([]Detail, error) {
	var out []Detail
	for l := range strings.SplitSeq(s, "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) != 8 {
			return nil, fmt.Errorf("замер процессов: строка %q", l)
		}
		d := Detail{Cgroup: f[2], Cwd: f[3], SSH: f[5], Cmd: f[6]}
		var err error
		if d.Pid, err = strconv.Atoi(f[0]); err != nil {
			return nil, fmt.Errorf("замер процессов: %q: %w", l, err)
		}
		if d.Start, err = strconv.Atoi(f[1]); err != nil {
			return nil, fmt.Errorf("замер процессов: %q: %w", l, err)
		}
		if d.Claude, err = claudePid(f[4]); err != nil {
			return nil, fmt.Errorf("замер процессов: %q: %w", l, err)
		}
		if f[7] != "" {
			secs, err := strconv.Atoi(f[7])
			if err != nil {
				return nil, fmt.Errorf("замер процессов: %q: %w", l, err)
			}
			d.Login, d.LoginAge = true, time.Duration(secs)*time.Second
		}
		out = append(out, d)
	}
	return out, nil
}

// Stray — процесс без родителя, о котором пора писать.
type Stray struct {
	Detail
	Comm    string
	Age     time.Duration
	Rate    float64 // занятость дерева за прошлый проход, ядер; 0 — первый проход
	CPU     float64 // секунд процессора дерева за жизнь живых процессов
	Descend int     // потомков
}

// Key — процесс на машине: pid переиспользуется, starttime — нет.
func (d *Detail) Key(host string) string { return fmt.Sprintf("%s:%d:%d", host, d.Pid, d.Start) }

// StrayWatch — память прохода по одной машине: прирост процессора считается между проходами. Рестарт демона её теряет, а
// пропуск замеров дольше двух every обесценивает (средняя за пропуск — не занятость на каждом проходе): занятость другого
// scope считается заново, остаток сессии от неё не зависит.
type StrayWatch struct {
	cpu  map[int]float64
	at   time.Time
	busy map[string]time.Time // процесс -> с какого прохода дерево занято подряд
}

// sessionScope — сессия logind: номер или, без места, c1, c2.
var sessionScope = regexp.MustCompile(`/session-[^/]+\.scope$`)

// desktopApp — главный процесс приложения сеанса: app-<id>.scope, у терминала — его main.scope. Вкладки терминала
// (tab(N).scope) сюда не входят: орфан в них остаётся под правилом занятости.
var desktopApp = regexp.MustCompile(`/app-[^/]+\.scope(?:/main\.scope)?$`)

// loginWindow — насколько возраст демона входа отличается от возраста сессии: PAM поднял ksecretd за 0,04 с до её отметки
// (замер 04.11), запас на медленный вход. Чем шире, тем дольше сирота первых секунд сеанса молчит.
const loginWindow = time.Minute

// agentComm — имя процесса сессии агента; по нему, а не по реестру сессий: реестр знает только свою машину, а PC — по ssh.
const agentComm = "claude"

// Judge — процессы, о которых пора писать, по замеру машины.
func (w *StrayWatch) Judge(now time.Time, procs []Proc, details []Detail, c *StrayConfig) []Stray {
	exclude := regexp.MustCompile("^(?:" + c.Exclude + ")$") // проверен Validate
	byPid := make(map[int]*Proc, len(procs))
	kids := map[int][]int{}
	for i := range procs {
		p := &procs[i]
		byPid[p.Pid] = p
		kids[p.PPid] = append(kids[p.PPid], p.Pid)
	}
	dt := now.Sub(w.at).Seconds()
	first := w.cpu == nil || dt > 2*c.Every.D().Seconds()
	busy := map[string]time.Time{}
	var out []Stray
	for i := range details {
		d := &details[i]
		p := byPid[d.Pid]
		if p == nil || exclude.MatchString(p.Comm) || desktopApp.MatchString(d.Cgroup) { // ушёл между строками или законный фон
			continue
		}
		s := Stray{Detail: *d, Comm: p.Comm, Age: p.Age}
		use := w.treeUse(d.Pid, byPid, kids, first, exclude)
		if use.live {
			continue
		}
		s.CPU, s.Descend = use.cpu, use.count-1
		grew := use.grew
		if !first && dt > 0 {
			s.Rate = grew / dt
		}
		key := s.Key("")
		if !first && s.Rate >= c.Hot {
			since, ok := w.busy[key]
			if !ok {
				since = w.at
			}
			busy[key] = since
		}
		_, hot := busy[key]
		if sessionScope.MatchString(d.Cgroup) && !(d.Login && (d.LoginAge-p.Age).Abs() <= loginWindow) {
			if p.Age >= c.After.D() {
				out = append(out, s)
			}
		} else if hot && now.Sub(busy[key]) >= c.After.D() {
			out = append(out, s)
		}
	}
	w.at, w.busy = now, busy
	w.cpu = make(map[int]float64, len(procs))
	for i := range procs {
		w.cpu[procs[i].Pid] = procs[i].CPU
	}
	return out
}

// treeUse — итог дерева за проход.
type treeUse struct {
	cpu, grew float64 // процессора за жизнь живых и прирост за проход, секунд
	count     int
	live      bool // внутри живая сессия агента или законный фон
}

func (w *StrayWatch) treeUse(root int, byPid map[int]*Proc, kids map[int][]int, first bool, exclude *regexp.Regexp) treeUse {
	var u treeUse
	for _, pid := range tree(root, kids) {
		q := byPid[pid]
		u.count++
		u.live = u.live || q.Comm == agentComm || exclude.MatchString(q.Comm)
		u.cpu += q.CPU
		if prev, ok := w.cpu[pid]; ok {
			u.grew += max(0, q.CPU-prev)
		} else if !first { // родился между проходами: весь его процессор — за этот проход
			u.grew += q.CPU
		}
	}
	return u
}

// tree — pid и все потомки; seen — снимок ps не атомарен, петля по ppid не должна его подвесить.
func tree(root int, kids map[int][]int) []int {
	out := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, k := range kids[out[i]] {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// StrayState — кому и когда писали о процессе; на диске: рестарт демона не повторяет письма.
type StrayState struct {
	Told map[string]time.Time `json:"told"`
}

// LoadStrayState — состояние из path; файла нет — пустое.
func LoadStrayState(path string) (StrayState, error) {
	st := StrayState{Told: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("состояние процессов без родителя: %w", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("состояние процессов без родителя %s: %w", path, err)
	}
	if st.Told == nil {
		st.Told = map[string]time.Time{}
	}
	return st, nil
}

// Save — на диск.
func (st *StrayState) Save(path string) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("сериализация состояния процессов без родителя: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("состояние процессов без родителя: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("состояние процессов без родителя: %w", err)
	}
	return nil
}

// Forget убирает отметки машины о процессах, которых больше нет среди живых без родителя (present — замер прохода).
// Спокойный процесс отметку держит: первый проход после рестарта занятости не знает, а дерево, занятое рывками, иначе
// писало бы каждые After, а не раз в Repeat.
func (st *StrayState) Forget(host string, present []Detail) bool {
	keep := make(map[string]bool, len(present))
	for i := range present {
		keep[present[i].Key(host)] = true
	}
	changed := false
	for k := range st.Told {
		if strings.HasPrefix(k, host+":") && !keep[k] {
			delete(st.Told, k)
			changed = true
		}
	}
	return changed
}

// secret — значение после слова-признака секрета в строке запуска: письмо и журнал его не несут.
var secret = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|api[-_]?key|auth)[a-z_-]*[=: ]+(?:(?:bearer|basic) +)?|\bbearer +)\S+|\b(?:sk|ghp|gho|glpat|xox[bp])-?[a-z0-9_-]{16,}`)

// cmdMax — сколько символов строки запуска в письме: узнать процесс, а не читать аргументы.
const cmdMax = 100

// CleanCmd — начало строки запуска без значений секретов.
func CleanCmd(cmd string) string {
	cmd = secret.ReplaceAllStringFunc(cmd, func(m string) string {
		if g := secret.FindStringSubmatch(m); g[1] != "" {
			return g[1] + "***"
		}
		return "***"
	})
	if r := []rune(strings.TrimSpace(cmd)); len(r) > cmdMax {
		return string(r[:cmdMax]) + "…"
	}
	return strings.TrimSpace(cmd)
}

// strayShown — сколько процессов письмо называет: шторм сирот — одно письмо, а не сотни.
const strayShown = 5

// StrayMessage — письмо о процессах без родителя на машине host, больше процессора — раньше; repeat — пишу не впервые;
// absent — мастер, которому письмо шло бы, но его нет в живых.
func StrayMessage(host string, strays []Stray, repeat bool, absent string) string {
	sorted := make([]*Stray, len(strays))
	for i := range strays {
		sorted[i] = &strays[i]
	}
	slices.SortFunc(sorted, func(a, b *Stray) int { return cmp.Or(cmp.Compare(b.CPU, a.CPU), cmp.Compare(a.Pid, b.Pid)) })
	var b strings.Builder
	fmt.Fprintf(&b, "aguard: на %s процессы без родителя (%d)", host, len(sorted))
	if repeat {
		b.WriteString(", пишу повторно")
	}
	if absent != "" {
		fmt.Fprintf(&b, "; %s нет в сессиях, потому тебе", absent)
	}
	b.WriteString(":")
	for i := range sorted {
		if i == strayShown {
			fmt.Fprintf(&b, "\n… и ещё %d", len(sorted)-strayShown)
			break
		}
		s := sorted[i]
		fmt.Fprintf(&b, "\n- pid %d %s, живёт %s, процессора %s, сейчас %.0f %% ядра, потомков %d, cwd %s, %s: `%s`", s.Pid, s.Comm,
			bgtask.Span(s.Age), bgtask.Span(time.Duration(s.CPU)*time.Second), s.Rate*100, s.Descend, s.Cwd, unitOf(s.Cgroup), CleanCmd(s.Cmd))
	}
	b.WriteString("\nНичего не снимаю. Не нужен — сними дерево (TERM может держать, тогда KILL). Нужен как фон — запусти службой (`systemd-run --user`), о ней не пишу.")
	return b.String()
}

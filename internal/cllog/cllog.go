// Package cllog — строки журнала cl (~/.local/state/cl/restart.log) о том, что cl сделал с беседой: сжал
// перезапуском, сбросил, набрал /compact или /clear в живой сессии. cl пишет эти строки отсюда, aguard compact report
// разбирает их здесь же: формат один на обоих.
package cllog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TimeFormat — время в начале строки журнала, местное с поясом.
const TimeFormat = "2006-01-02T15:04:05-07:00"

// Path — журнал cl по умолчанию (restart.journal в конфиге cl).
func Path(home string) string {
	return filepath.Join(home, ".local", "state", "cl", "restart.log")
}

// Line — строка журнала: время и текст.
func Line(at time.Time, text string) string {
	return at.Format(TimeFormat) + " " + text + "\n"
}

// Тела строк о сделанном. Начало строки — «<имя> pid=<N> act: » у Compact, Reset и Typing, «<имя> pid=<N>: » у
// Typed, HumanTyping, TypeFailed и Cancelled; CompactResult — строка целиком.

// Compact — сжатие перезапуском: SIGTERM, `claude -p --resume <беседа> /compact`, подъём с --resume.
func Compact(tokens int64, sessionID string) string {
	return fmt.Sprintf("сжимаю: контекст %d тыс.; SIGTERM, /compact, поднимаю --resume %s", tokens/1000, sessionID)
}

// NoReset — хвост строки о сжатии: почему не сброс (сброс там, где можно, — вместо сжатия; пользователь 27.10 10:48
// CET «Компакт это компромисс»), доводы aguard против сброса; доводов нет — пусто.
func NoReset(against []string) string {
	if len(against) == 0 {
		return ""
	}
	return "; сброс нельзя: " + strings.Join(against, "; ")
}

// Reset — сброс: SIGTERM и новая беседа.
func Reset(tokens int64, sessionID string) string {
	return fmt.Sprintf("сбрасываю: контекст %d тыс.; SIGTERM, поднимаю новую беседу вместо %s", tokens/1000, sessionID)
}

// Typing — набор typed (/compact или /clear) в живой сессии под pty; итог — строкой Typed, HumanTyping или TypeFailed.
func Typing(tokens int64, typed string) string {
	verb := "сжимаю"
	if typed == TypedClear {
		verb = "сбрасываю"
	}
	return fmt.Sprintf("%s: контекст %d тыс.; набираю %s в живой сессии", verb, tokens/1000, typed)
}

// Что cl набирает в живой сессии.
const (
	TypedCompact = "/compact"
	TypedClear   = "/clear"
)

// Typed — набрал.
func Typed(typed, sessionID string) string {
	return fmt.Sprintf("набрал %s в беседе %s", typed, sessionID)
}

// HumanTyping — не набрал: человек вводил quiet назад.
func HumanTyping(quiet time.Duration) string {
	return fmt.Sprintf("человек вводил %v назад — не набираю", quiet.Round(time.Second))
}

// TypeFailed — набор не удался.
func TypeFailed(typed string, err error) string {
	return fmt.Sprintf("набор %s: %v", typed, err)
}

// Начала строк cl об окне доверия: по ним их находит TrustEvents. trustLeft — окно осталось (TrustWriteFailed; до
// 28.10 10:30 CET — и «человек у терминала … — не подтверждаю / Enter не нажимаю»), trustDone — cl его подтвердил.
const (
	trustLeft = "окно доверия к "
	trustDone = "подтвердил доверие к "
)

// TrustWriteFailed — ввод в pty окна доверия не удался.
func TrustWriteFailed(dir string, err error) string {
	return fmt.Sprintf(trustLeft+"%s: %v", dir, err)
}

// TrustConfirmed — cl подтвердил доверие к домашней папке.
func TrustConfirmed(dir string) string {
	return fmt.Sprintf(trustDone+"%s: Claude Code не сохраняет доверие к домашней папке", dir)
}

// TrustEvent — окно доверия при подъёме сессии (подтверждённое cl или оставшееся) и более поздний подъём той же
// сессии: подтверждение тоже может не дойти, проверяется итог.
type TrustEvent struct {
	At        time.Time
	Name      string
	Dir       string
	Confirmed bool
	Recovered time.Time
	PID       int // pid cl; 0 — строка без него (до 28.10 11:30 CET)
}

// reUp — строка решения cl о живой сессии с прочитанными версиями («lead pid=2077626 2.1.312→2.1.312 act: …»): cl
// пишет её, только когда claude прошёл окно и записался в реестр (28.10: lead стоял на окне с 00:38:38, такая строка —
// в 09:34:38). «нет записи→нет записи» — реестр не прочитан, подъёмом не считается; строки «pid=N: …» (SIGTERM, переход
// в новый бинарник) пишет cl, а не поднявшийся claude.
var reUp = regexp.MustCompile(`^(.+?) pid=\d+ [0-9—][^ ]*→`)

// TrustEvents находит окна доверия; более поздняя строка решения о той же сессии (reUp) — она поднялась.
func TrustEvents(r io.Reader) ([]TrustEvent, int, error) {
	type entry struct {
		at    time.Time
		text  string
		trust string // имя сессии, если строка — окно доверия
	}
	var lines []entry
	var events []TrustEvent
	var at []int // строка каждого события в lines
	bad := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		stamp, text, ok := strings.Cut(sc.Text(), " ")
		when, err := time.Parse(TimeFormat, stamp)
		if !ok || err != nil {
			bad++
			continue
		}
		lines = append(lines, entry{at: when, text: text})
		name, body, ok := strings.Cut(text, ": ")
		rest, left := strings.CutPrefix(body, trustLeft)
		done, confirmed := strings.CutPrefix(body, trustDone)
		if !ok || !left && !confirmed {
			continue
		}
		if confirmed {
			rest = done
		}
		dir, _, _ := strings.Cut(rest, ": ")
		if dir == "" {
			continue
		}
		pid := 0
		if n, p, ok := strings.Cut(name, " pid="); ok {
			if pid, err = strconv.Atoi(p); err != nil {
				bad++
				continue
			}
			name = n
		}
		lines[len(lines)-1].trust = name
		at = append(at, len(lines)-1)
		events = append(events, TrustEvent{At: when, Name: name, Dir: dir, Confirmed: confirmed, PID: pid})
	}
	if err := sc.Err(); err != nil {
		return nil, bad, fmt.Errorf("журнал cl: %w", err)
	}
	// подъём — до следующего окна той же сессии: строка решения после него — уже о следующем подъёме (28.10 проба:
	// окно 10:04:35 без подъёма, окно 10:36:10, строка решения 10:37:08)
	for i := range events {
		for _, line := range lines[at[i]+1:] {
			if line.trust == events[i].Name {
				break
			}
			if m := reUp.FindStringSubmatch(line.text); m != nil && m[1] == events[i].Name {
				events[i].Recovered = line.at
				break
			}
		}
	}
	return events, bad, nil
}

// Cancelled — SIGTERM не дошёл, перезапуск (и сжатие или сброс с ним) не состоялся.
func Cancelled(err error) string {
	return fmt.Sprintf("%v — перезапуск отменён", err)
}

// CompactResult — итог `claude -p --resume … /compact`: остановлен сигналом stop (не пусто), сжал (err == nil) или не
// сжалось — с хвостом вывода claude, последние 300 байт одной строкой.
func CompactResult(name, sessionID string, took time.Duration, err error, stop, output string) string {
	head := fmt.Sprintf("%s: /compact --resume %s за %v: ", name, sessionID, took.Round(time.Second))
	switch {
	case stop != "":
		return head + fmt.Sprintf("остановлено сигналом %s, выхожу", stop)
	case err == nil:
		return head + "сжал, поднимаю"
	}
	tail := strings.Join(strings.Fields(output), " ")
	if len(tail) > 300 {
		tail = "…" + tail[len(tail)-300:]
	}
	return head + fmt.Sprintf("не сжалось (%v: %s) — поднимаю без сжатия", err, tail)
}

// Виды сделанного.
const (
	KindCompact     = "compact"      // сжатие перезапуском
	KindReset       = "reset"        // сброс перезапуском
	KindTypeCompact = "type_compact" // набор /compact в живой сессии
	KindTypeClear   = "type_clear"   // набор /clear в живой сессии
)

// Итоги сделанного.
const (
	ResultDone    = "done"    // сжал, поднял новую беседу, набрал
	ResultFailed  = "failed"  // не сжалось (поднял без сжатия), набор не удался, SIGTERM не дошёл
	ResultSkipped = "skipped" // не набрал: человек вводил
	ResultStopped = "stopped" // сжатие остановлено сигналом, cl вышел
	ResultNone    = "none"    // итога в журнале нет: cl умер посреди
)

// Action — что cl сделал с беседой. Tokens — контекст из строки журнала (там в тысячах, с точностью до тысячи).
type Action struct {
	At        time.Time `json:"at"`
	Name      string    `json:"name"`
	SessionID string    `json:"session_id"`
	Kind      string    `json:"kind"`
	Tokens    int64     `json:"tokens"`
	Result    string    `json:"result"`
}

var (
	reStart     = regexp.MustCompile(`^(.+?) pid=(\d+) act: (?:сжимаю|сбрасываю): контекст (\d+) тыс\.; (.*)$`)
	reCompact   = regexp.MustCompile(`^SIGTERM, /compact, поднимаю --resume ([^\s;]+)`)
	reReset     = regexp.MustCompile(`^SIGTERM, поднимаю новую беседу вместо (\S+)`)
	reTyping    = regexp.MustCompile(`^набираю (\S+) в живой сессии(?:;|$)`)
	rePid       = regexp.MustCompile(`^(.+?) pid=(\d+): (.*)$`)
	reTyped     = regexp.MustCompile(`^набрал \S+ в беседе (\S+)$`)
	reHuman     = regexp.MustCompile(`^человек вводил .* — не набираю$`)
	reTypeFail  = regexp.MustCompile(`^набор \S+: `)
	reCancelled = regexp.MustCompile(` — перезапуск отменён$`)
	reResult    = regexp.MustCompile(`^(.+?): /compact --resume (\S+) за \S+: (сжал, поднимаю|не сжалось|остановлено сигналом)`)
)

// Parse — сделанное из журнала по порядку строк; итог ищется в последующих строках той же сессии. Чужие строки
// (решения, «сжал бы») пропускаются, строки без времени считаются в bad.
func Parse(r io.Reader) ([]Action, int, error) {
	p := parser{open: map[string]int{}, keys: map[int][]string{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		stamp, text, ok := strings.Cut(sc.Text(), " ")
		at, err := time.Parse(TimeFormat, stamp)
		if !ok || err != nil {
			p.bad++
			continue
		}
		started, err := p.start(at, text)
		if err != nil {
			return p.out, p.bad, fmt.Errorf("журнал cl, строка %d: %w", n, err)
		}
		if !started && !p.result(text) {
			p.pidLine(text)
		}
	}
	if err := sc.Err(); err != nil {
		return p.out, p.bad, fmt.Errorf("журнал cl: %w", err)
	}
	return p.out, p.bad, nil
}

// parser — разбор журнала: сделанное и то, что ещё ждёт итога.
type parser struct {
	out  []Action
	bad  int
	open map[string]int   // «имя pid» и у сжатия «имя беседа» → индекс в out без итога
	keys map[int][]string // индекс → его ключи в open
}

// settle — итог сделанного i; дальше его итог не ищется.
func (p *parser) settle(i int, result string) {
	p.out[i].Result = result
	for _, k := range p.keys[i] {
		delete(p.open, k)
	}
	delete(p.keys, i)
}

// start — строка «сжимаю» или «сбрасываю»: начало сделанного. Ложь — строка не такая; ошибка — контекст в ней не
// читается.
func (p *parser) start(at time.Time, text string) (bool, error) {
	m := reStart.FindStringSubmatch(text)
	if m == nil {
		return false, nil
	}
	k, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return false, fmt.Errorf("контекст %q: %w", m[3], err)
	}
	a := Action{At: at, Name: m[1], Tokens: k * 1000, Result: ResultNone}
	ks := []string{m[1] + " " + m[2]}
	switch tail := m[4]; {
	case reCompact.MatchString(tail):
		a.Kind, a.SessionID = KindCompact, reCompact.FindStringSubmatch(tail)[1]
		ks = append(ks, a.Name+" "+a.SessionID)
	case reReset.MatchString(tail):
		a.Kind, a.SessionID, a.Result = KindReset, reReset.FindStringSubmatch(tail)[1], ResultDone
	case reTyping.MatchString(tail):
		a.Kind = KindTypeCompact
		if reTyping.FindStringSubmatch(tail)[1] == TypedClear {
			a.Kind = KindTypeClear
		}
	default:
		return true, nil
	}
	for _, k := range ks {
		p.open[k] = len(p.out)
	}
	p.keys[len(p.out)] = ks
	p.out = append(p.out, a)
	return true, nil
}

// result — итог сжатия перезапуском. Ложь — строка не такая.
func (p *parser) result(text string) bool {
	m := reResult.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	result := ResultFailed
	switch {
	case strings.HasPrefix(m[3], "сжал"):
		result = ResultDone
	case strings.HasPrefix(m[3], "остановлено"):
		result = ResultStopped
	}
	if i, ok := p.open[m[1]+" "+m[2]]; ok {
		p.settle(i, result)
	}
	return true
}

// pidLine — строка «<имя> pid=<N>: …»: итог набора или отменённый перезапуск.
func (p *parser) pidLine(text string) {
	m := rePid.FindStringSubmatch(text)
	if m == nil {
		return
	}
	i, ok := p.open[m[1]+" "+m[2]]
	if !ok {
		return
	}
	typed := p.out[i].Kind == KindTypeCompact || p.out[i].Kind == KindTypeClear
	switch body := m[3]; {
	case reCancelled.MatchString(body):
		p.settle(i, ResultFailed)
	case typed && reTyped.MatchString(body):
		p.out[i].SessionID = reTyped.FindStringSubmatch(body)[1]
		p.settle(i, ResultDone)
	case typed && reHuman.MatchString(body):
		p.settle(i, ResultSkipped)
	case typed && reTypeFail.MatchString(body):
		p.settle(i, ResultFailed)
	}
}

// Read — сделанное из журнала file с моментом в [from, to); журнала нет — ничего.
func Read(file string, from, to time.Time) ([]Action, int, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("журнал cl: %w", err)
	}
	defer f.Close()
	all, bad, err := Parse(f)
	var out []Action
	for _, a := range all {
		if !a.At.Before(from) && a.At.Before(to) {
			out = append(out, a)
		}
	}
	return out, bad, err
}

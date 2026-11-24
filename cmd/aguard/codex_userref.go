package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Одобрение досрочного сброса держится на реплике пользователя: строка файла одобрений несёт ссылку tl на неё
// (`tl <сессия>:<строка>`), и ссылка ведёт к реплике с ролью user внутри окна, которое одобрено. Слово 23.11 14:01
// «Не спеши, сбросы только с моего одобрения» (tl 330519de:3189); тикет codex-approve-user-ref.

// codexWindow — длина окна лимита Codex: окно со сбросом resetsAt началось за неделю до него.
const codexWindow = 7 * 24 * time.Hour

// codexRefPattern — ссылка tl в тексте одобрения: «tl ff4b27b6:298» или старое «e7192203:278» в начале строки.
var codexRefPattern = regexp.MustCompile(`(?:^|\btl )([0-9a-f]{8}[0-9a-f-]{0,28}:[0-9]+)\b`)

// codexQuotePattern — цитата «…» в тексте одобрения.
var codexQuotePattern = regexp.MustCompile(`«([^»]+)»`)

// userEvent — событие транскрипта по ссылке tl.
type userEvent struct {
	At   time.Time
	Role string
	Text string
}

// userRefResolver — событие по ссылке tl; подменяется в тестах.
type userRefResolver func(ctx context.Context, ref string) (userEvent, error)

// codexSourceRefs — ссылки tl из текста одобрения по порядку.
func codexSourceRefs(source string) []string {
	var refs []string
	for _, m := range codexRefPattern.FindAllStringSubmatch(source, -1) {
		refs = append(refs, m[1])
	}
	return refs
}

// codexQuoteMin — короче цитата не привязывает одобрение к реплике: «да» или «ok» есть почти в любом ответе.
const codexQuoteMin = 8

// userRole — роль слова пользователя в tl: реплика или ответ в окне вопроса.
func userRole(role string) bool { return role == "user" || role == "ask" }

// askSep — разделитель ответа и вопроса в тексте события ask у tl: «ответ ← вопрос» по строке на вопрос
// (akit transcript.askAnswers).
const askSep = " ← "

// userWords — слова самого пользователя в событии: у ask — только ответы, без вопроса агента; строка без разделителя
// (продолжение многострочного вопроса или ответа) отбрасывается — цитата из неё не найдётся, отказ закрытый.
func userWords(ev userEvent) string {
	if ev.Role != "ask" {
		return ev.Text
	}
	var answers []string
	for line := range strings.SplitSeq(ev.Text, "\n") {
		if answer, _, ok := strings.Cut(line, askSep); ok {
			answers = append(answers, answer)
		}
	}
	return strings.Join(answers, "\n")
}

// errApprovalRejected — строка одобрения на окно есть, но не держится на реплике пользователя.
var errApprovalRejected = errors.New("одобрение не принято")

// errTLUnavailable — tl не ответил (таймаут, не запустился): строку не проверить на этом проходе, но это не отказ —
// следующий проход проверит её заново, и лиду переписывать нечего.
var errTLUnavailable = errors.New("tl не ответил")

// checkCodexApproval — текст одобрения держится на реплике пользователя в одобряемом окне: есть цитата «…» не короче
// codexQuoteMin знаков и ссылка tl на реплику (user или ask), сказанную после начала окна (resetsAt − неделя) и не
// позже now, и каждая цитата — из неё. Ссылок несколько (письмо лида и слово пользователя) — годится любая.
func checkCodexApproval(ctx context.Context, resolve userRefResolver, source string, resetsAt, now time.Time) error {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	quotes := codexQuotePattern.FindAllStringSubmatch(source, -1)
	if len(quotes) == 0 {
		return errors.New("нет цитаты «…» реплики пользователя")
	}
	for _, q := range quotes {
		if len([]rune(norm(q[1]))) < codexQuoteMin {
			return fmt.Errorf("цитата «%s» короче %d знаков — не привязывает к реплике", q[1], codexQuoteMin)
		}
	}
	refs := codexSourceRefs(source)
	if len(refs) == 0 {
		return errors.New("нет ссылки tl на реплику пользователя (tl <сессия>:<строка>)")
	}
	start := resetsAt.Add(-codexWindow)
	var why []string
	var unavailable error
	for _, ref := range refs {
		ev, err := resolve(ctx, ref)
		switch {
		case errors.Is(err, errTLUnavailable):
			unavailable = fmt.Errorf("ссылка tl %s: %w", ref, err)
			continue
		case err != nil:
			why = append(why, fmt.Sprintf("ссылка tl %s: %v", ref, err))
			continue
		case !userRole(ev.Role):
			why = append(why, fmt.Sprintf("tl %s — реплика %q, а не пользователя", ref, ev.Role))
			continue
		case ev.At.Before(start) || ev.At.After(now):
			why = append(why, fmt.Sprintf("tl %s сказана %s — вне окна %s … %s", ref, ev.At.Format(time.RFC3339),
				start.Format(time.RFC3339), resetsAt.Format(time.RFC3339)))
			continue
		}
		said := norm(userWords(ev))
		missing := ""
		for _, q := range quotes {
			if !strings.Contains(said, norm(q[1])) {
				missing = q[1]
				break
			}
		}
		if missing == "" {
			return nil
		}
		why = append(why, fmt.Sprintf("цитаты «%s» нет в реплике tl %s", missing, ref))
	}
	if unavailable != nil { // непроверенная ссылка могла бы держать строку: отказа нет, повтор на следующем проходе
		return unavailable
	}
	return errors.New(strings.Join(why, "; "))
}

// approvalCache — принятые проверки (файл, источник, окно): tl зовётся раз на строку, а не на каждом проходе.
type approvalCache struct {
	mu sync.Mutex
	ok map[string]bool
}

// defaultCodexApprovals — взвод демона: строка файла одобрений, проверенная по реплике через tl, с памятью принятых.
func defaultCodexApprovals() codexApprovals { return (&approvalCache{}).checker(tlUserRef) }

// checker — codexApprovals поверх approvedCodexChecked с этой памятью.
func (c *approvalCache) checker(resolve userRefResolver) codexApprovals {
	return func(ctx context.Context, path string, resetsAt time.Time) (string, error) {
		return approvedCodexChecked(ctx, c, resolve, path, resetsAt)
	}
}

// approvedCodexChecked — «да» на окно из файла одобрений, если оно держится на реплике пользователя; иначе пусто и
// ошибка: errApprovalRejected — строка есть, но не принята (причина — в письме лиду), иначе — файл не прочитан.
func approvedCodexChecked(ctx context.Context, cache *approvalCache, resolve userRefResolver, path string, resetsAt time.Time) (string, error) {
	src, err := approvedCodex(path, resetsAt)
	if err != nil || src == "" {
		return "", err
	}
	key := path + "\x00" + src + "\x00" + resetsAt.UTC().Format(time.RFC3339)
	cache.mu.Lock()
	hit := cache.ok[key]
	cache.mu.Unlock()
	if hit {
		return src, nil
	}
	if err := checkCodexApproval(ctx, resolve, src, resetsAt, time.Now()); err != nil {
		if errors.Is(err, errTLUnavailable) {
			return "", fmt.Errorf("окно %s: %w", resetsAt.Format(time.RFC3339), err)
		}
		return "", fmt.Errorf("%w: окно %s: %w", errApprovalRejected, resetsAt.Format(time.RFC3339), err)
	}
	cache.mu.Lock()
	if cache.ok == nil {
		cache.ok = map[string]bool{}
	}
	cache.ok[key] = true
	cache.mu.Unlock()
	return src, nil
}

// tlRefTimeout — сколько ждать tl по одной ссылке.
const tlRefTimeout = time.Minute

// tlHeader — первая строка события tl: `--- 22.11.2026 14:50:04 CET ff4b27b6:298 user`.
var tlHeader = regexp.MustCompile(`^--- ([0-9]{2}\.[0-9]{2}\.[0-9]{4} [0-9]{2}:[0-9]{2}:[0-9]{2}) \S+ (\S+) (\S+)$`)

// tlUserRef — событие по ссылке через `tl <ref>`: время, роль и текст.
func tlUserRef(ctx context.Context, ref string) (userEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, tlRefTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tl", ref)
	cmd.WaitDelay = queueExternalKillWait
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil && ee.Exited() {
			// tl ответил кодом (1 — нет строки, 2 — нет сессии): это ответ о ссылке, не сбой
			return userEvent{}, fmt.Errorf("tl: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return userEvent{}, fmt.Errorf("%w: %w", errTLUnavailable, err)
	}
	return parseTLEvent(string(out), ref)
}

// sameRef — та же строка той же сессии: tl печатает 8 знаков id, в ссылке их может быть больше.
func sameRef(printed, ref string) bool {
	ps, pl, ok1 := strings.Cut(printed, ":")
	rs, rl, ok2 := strings.Cut(ref, ":")
	return ok1 && ok2 && pl == rl && ps != "" && rs != "" && (strings.HasPrefix(ps, rs) || strings.HasPrefix(rs, ps))
}

// parseTLEvent — разбор вывода `tl <ref>`: заголовок события с этой ссылкой и текст до следующего заголовка.
func parseTLEvent(out, ref string) (userEvent, error) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		m := tlHeader.FindStringSubmatch(line)
		if m == nil || !sameRef(m[2], ref) {
			continue
		}
		at, err := time.ParseInLocation("02.01.2006 15:04:05", m[1], time.Local)
		if err != nil {
			return userEvent{}, fmt.Errorf("время события tl %s: %w", ref, err)
		}
		var text []string
		for _, l := range lines[i+1:] {
			if strings.HasPrefix(l, "--- ") {
				break
			}
			text = append(text, l)
		}
		return userEvent{At: at, Role: m[3], Text: strings.TrimSpace(strings.Join(text, "\n"))}, nil
	}
	return userEvent{}, fmt.Errorf("в выводе tl нет события %s", ref)
}

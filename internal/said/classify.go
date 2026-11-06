package said

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/akit/oneshot"
	"github.com/aiseeq/akit/price"
)

// Model — разовый вызов модели: инструкция Prompt, ответ по Schema; в ответе id модели и токены вызова.
type Model func(ctx context.Context, input string) (oneshot.Result, error)

// OneShot — Model через akit/oneshot: модель из списка одобренных, без инструментов; codexHome — учётка OpenAI вызова.
func (c *Config) OneShot(codexHome string) Model {
	return func(ctx context.Context, input string) (oneshot.Result, error) {
		return oneshot.Ask(ctx, oneshot.Request{Model: c.Model, Effort: c.Effort, System: Prompt, Schema: Schema,
			Input: input, CodexHome: codexHome})
	}
}

// OnCodex — модель идёт через codex и выбирает учётку OpenAI (в akit/oneshot всё, кроме opus).
func (c *Config) OnCodex() bool { return c.Model != "opus" }

// Item — находка модели.
type Item struct {
	Kind       string `json:"kind"`
	Quote      string `json:"quote"`
	Scope      string `json:"scope"`
	Words      string `json:"words"`
	Supersedes string `json:"supersedes"`
}

// Record — строка журнала said: одна реплика, что нашла модель, что записано и во что обошлось.
type Record struct {
	At      time.Time `json:"at"` // время разметки
	Said    time.Time `json:"said"`
	Session string    `json:"session"`
	Name    string    `json:"name"`
	Ref     string    `json:"ref"`
	Chars   int       `json:"chars"` // длина реплики
	Items   []Item    `json:"items,omitempty"`
	// Dropped — находки, чья цитата не нашлась в реплике дословно: в указатель не идут.
	Dropped []Item      `json:"dropped,omitempty"`
	Model   string      `json:"model,omitempty"`   // id модели: по нему price.Cost
	Account string      `json:"account,omitempty"` // учётка OpenAI вызова
	Usage   price.Usage `json:"usage"`
	Ms      int64       `json:"ms"`
	Attempt int         `json:"attempt"`
	Err     string      `json:"err,omitempty"`
	// Down — модель не ответила (лимит, сеть, таймаут): реплика не виновата, попытка не считается.
	Down    bool `json:"down,omitempty"`
	Written bool `json:"written,omitempty"` // записано в said.tsv (режим write)
}

// Ref — ссылка на реплику: <8 знаков id>:<строка jsonl>.
func Ref(session string, line int64) string {
	return fmt.Sprintf("%s:%d", session[:min(8, len(session))], line)
}

// tilde — путь с ~ вместо домашней папки.
func tilde(path, home string) string {
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rest
	}
	return path
}

// Input — текст для модели: сессия, время, конец сообщения агента, реплика и прежние записи-кандидаты.
func Input(p *Pending, home string, candidates []Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Сессия: %s, папка %s\nВремя: %s\n", p.Event.Name, tilde(p.Event.Cwd, home),
		p.Event.At.Local().Format("02.01 15:04 MST"))
	fmt.Fprintf(&b, "В ответ на (конец сообщения агента): %s\nРеплика:\n%s\n", strings.TrimSpace(p.InReply+" "), p.Event.Text)
	if len(candidates) == 0 {
		b.WriteString("Прежние записи: нет\n")
		return b.String()
	}
	b.WriteString("Прежние записи (ref | вид | область | цитата):\n")
	for i := range candidates {
		e := &candidates[i]
		q := []rune(e.Quote)
		fmt.Fprintf(&b, "%s | %s | %s | %s\n", e.Ref, e.Kind, e.Scope, string(q[:min(300, len(q))]))
	}
	return b.String()
}

// squash — пробелы схлопнуты: цитата сверяется с репликой без учёта переносов и двойных пробелов.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// Classify — разметить реплику: запись журнала и записи указателя (цитата — дословная подстрока реплики).
func (c *Config) Classify(ctx context.Context, model Model, p *Pending, home string, prior []Entry, now time.Time) (
	Record, []Entry) {
	ev := &p.Event
	ref := Ref(ev.Session, ev.Line)
	rec := Record{At: now, Said: ev.At, Session: ev.Session, Name: ev.Name, Ref: ref, Chars: len([]rune(ev.Text)),
		Attempt: p.Attempts}
	project := tilde(ev.Cwd, home)
	candidates := Candidates(prior, project, ev.Text, c.Candidates)
	ctx, cancel := context.WithTimeout(ctx, c.Timeout.D())
	defer cancel()
	start := time.Now()
	out, err := model(ctx, Input(p, home, candidates))
	rec.Ms, rec.Model, rec.Usage = time.Since(start).Milliseconds(), out.ID, out.Usage
	if err != nil {
		rec.Err, rec.Down = err.Error(), true
		return rec, nil
	}
	var res struct {
		Items []Item `json:"items"`
	}
	if err := json.Unmarshal([]byte(out.Text), &res); err != nil {
		rec.Err = fmt.Sprintf("ответ модели не JSON: %v", err)
		return rec, nil
	}
	known := map[string]bool{}
	for i := range candidates {
		known[candidates[i].Ref] = true
	}
	text := squash(ev.Text)
	var entries []Entry
	for _, it := range res.Items {
		if len(entries) > 0 { // вторая запись той же реплики — своя ссылка, чтобы supersedes отменял одну
			ref = fmt.Sprintf("%s#%d", Ref(ev.Session, ev.Line), len(entries)+1)
		}
		if it.Kind != KindRule && it.Kind != KindDecision && it.Kind != KindPromise || squash(it.Quote) == "" ||
			!strings.Contains(text, squash(it.Quote)) {
			rec.Dropped = append(rec.Dropped, it)
			continue
		}
		if it.Scope != "global" && !strings.HasPrefix(it.Scope, "~") {
			it.Scope = project
		}
		it.Scope = filepath.Clean(it.Scope)
		if !known[it.Supersedes] {
			it.Supersedes = ""
		}
		rec.Items = append(rec.Items, it)
		entries = append(entries, Entry{Time: ev.At.Local(), Session: ev.Session, Name: ev.Name, Project: project,
			Scope: it.Scope, Kind: it.Kind, Words: strings.ToLower(squash(it.Words)), Quote: strings.TrimSpace(it.Quote),
			Ref: ref, Supersedes: it.Supersedes})
	}
	return rec, entries
}

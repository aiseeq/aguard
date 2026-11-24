package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Одобрение держится на реплике пользователя в одобряемом окне: ссылка tl есть, роль user, время в окне, цитата из неё.
func TestCheckCodexApproval(t *testing.T) {
	now := time.Date(2026, 11, 22, 15, 0, 0, 0, time.UTC)
	resetsAt := time.Date(2026, 11, 24, 11, 55, 27, 0, time.UTC)
	said := time.Date(2026, 11, 22, 13, 50, 4, 0, time.UTC)
	ev := userEvent{At: said, Role: "user", Text: "Триггерни сброс личной учётки openai когда она кончится"}
	resolve := func(ev userEvent, err error) userRefResolver {
		return func(_ context.Context, ref string) (userEvent, error) {
			switch ref {
			case "ff4b27b6:298":
				return ev, err
			case "0badbad0:1":
				return userEvent{}, errors.New("no event at 0badbad0:1")
			case "0123abcd:4":
				return userEvent{At: ev.At, Role: "peer", Text: "письмо лида"}, nil
			}
			t.Fatalf("ссылка %q", ref)
			return ev, err
		}
	}
	good := "«Триггерни сброс личной учётки openai когда она кончится» (22.11 14:50:04 CET, tl ff4b27b6:298)"
	with := func(f func(*userEvent)) userEvent { e := ev; f(&e); return e }
	cases := []struct {
		name   string
		source string
		ev     userEvent
		err    error
		want   string // пусто — принято
	}{
		{"реплика пользователя", good, ev, nil, ""},
		{"старая запись: ссылка в начале", "ff4b27b6:298 22.11 14:50 «Триггерни сброс»", ev, nil, ""},
		{"без цитаты", "22.11 14:50, tl ff4b27b6:298", ev, nil, "нет цитаты"},
		{"короткая цитата", "«да, ок» (tl ff4b27b6:298)", ev, nil, "короче"},
		{"ответ в окне вопроса", "«сбрось при 100%» (tl ff4b27b6:298)", with(func(e *userEvent) {
			e.Role, e.Text = "ask", "Да, сбрось при 100% ← Сбросить лимит codex?\nпозже ← Когда?"
		}), nil, ""},
		{"цитата из вопроса агента в ask", "«Сбросить лимит codex при 100%» (tl ff4b27b6:298)", with(func(e *userEvent) {
			e.Role, e.Text = "ask", "Нет ← Сбросить лимит codex при 100%?"
		}), nil, "нет в реплике"},
		{"цитата из продолжения вопроса в ask", "«при 100% сразу» (tl ff4b27b6:298)", with(func(e *userEvent) {
			e.Role, e.Text = "ask", "Нет ← Сбросить лимит codex\nпри 100% сразу?"
		}), nil, "нет в реплике"},
		{"цитата 8 знаков", "«триггерн» (tl ff4b27b6:298)", ev, nil, ""},
		{"цитата 7 знаков", "«триггер» (tl ff4b27b6:298)", ev, nil, "короче"},
		{"первая ссылка не находится, вторая — реплика", "«Триггерни сброс личной» (tl 0badbad0:1, tl ff4b27b6:298)", ev, nil, ""},
		{"сначала письмо лида, потом реплика", "лид (tl 0123abcd:4): «Триггерни сброс личной» (tl ff4b27b6:298)", ev, nil, ""},
		{"нет ссылки", "«Триггерни сброс личной учётки» (22.11 14:50)", ev, nil, "нет ссылки tl"},
		{"ссылка не находится", good, ev, errors.New("no event"), "no event"},
		{"реплика агента", good, with(func(e *userEvent) { e.Role = "agent" }), nil, "а не пользователя"},
		{"письмо сессии", good, with(func(e *userEvent) { e.Role = "peer" }), nil, "а не пользователя"},
		{"до начала окна", good, with(func(e *userEvent) { e.At = resetsAt.Add(-codexWindow - time.Second) }), nil, "вне окна"},
		{"ровно начало окна", good, with(func(e *userEvent) { e.At = resetsAt.Add(-codexWindow) }), nil, ""},
		{"позже now", good, with(func(e *userEvent) { e.At = now.Add(time.Second) }), nil, "вне окна"},
		{"чужая цитата", "«сбрось обе учётки» (tl ff4b27b6:298)", ev, nil, "нет в реплике"},
		{"цитата в другом регистре и пробелах", "«триггерни  сброс личной» (tl ff4b27b6:298)", ev, nil, ""},
	}
	for _, c := range cases {
		err := checkCodexApproval(context.Background(), resolve(c.ev, c.err), c.source, resetsAt, now)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: отклонено: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, ждали %q", c.name, err, c.want)
		}
	}
}

// Взвод на проходе: строка окна без реплики пользователя не взводит (errApprovalRejected), с ней — взводит, и tl
// зовётся раз на строку; другого окна нет — пусто без tl; битый файл — ошибка чтения, не отказ строки.
func TestApprovedCodexChecked(t *testing.T) {
	resetsAt := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "approved.tsv")
	calls := 0
	user := func(_ context.Context, _ string) (userEvent, error) {
		calls++
		return userEvent{At: time.Now().Add(-time.Hour), Role: "user", Text: "сбрось при 100%"}, nil
	}
	write := func(source string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(resetsAt.Format(time.RFC3339)+"\t"+source+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := (&approvalCache{}).checker(user)
	ctx := context.Background()
	write("«сбрось при 100%» (tl abcdef12:7)")
	for range 3 {
		if src, err := check(ctx, path, resetsAt); err != nil || !strings.Contains(src, "abcdef12:7") {
			t.Fatalf("с ссылкой: %q %v", src, err)
		}
	}
	if calls != 1 {
		t.Fatalf("tl вызван %d раз на одну строку, ждали 1", calls)
	}
	if src, err := check(ctx, path, resetsAt.Add(time.Hour)); err != nil || src != "" || calls != 1 {
		t.Fatalf("другое окно: %q %v, tl %d", src, err, calls)
	}
	write("«сбрось при 100%» (tl abcdef12:8)")
	if _, err := check(ctx, path, resetsAt); err != nil || calls != 2 {
		t.Fatalf("новая строка не проверена заново: %v, tl %d", err, calls)
	}
	write("лид: пользователь согласен")
	src, err := check(ctx, path, resetsAt)
	if src != "" || !errors.Is(err, errApprovalRejected) || !strings.Contains(err.Error(), "нет цитаты") {
		t.Fatalf("без реплики взвёл: %q %v", src, err)
	}
	if err := os.WriteFile(path, []byte("битая строка\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if src, err := check(ctx, path, resetsAt); err == nil || src != "" || errors.Is(err, errApprovalRejected) {
		t.Fatalf("битый файл: %q %v", src, err)
	}
	// взвод демона: строка без реплики отклоняется до вызова tl
	write("лид: пользователь согласен")
	if src, err := defaultCodexApprovals()(ctx, path, resetsAt); src != "" || !errors.Is(err, errApprovalRejected) {
		t.Fatalf("взвод демона принял строку без реплики: %q %v", src, err)
	}
}

// approve без ссылки на реплику пользователя и с репликой агента — отказ, файл не пишется.
func TestCodexApproveRejectsWithoutUserRef(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		said string
		role string
	}{
		"без ссылки":     {"«сбрось при 100%» (16.11 11:00)", "user"},
		"реплика агента": {codexApproveSaid, "agent"},
	} {
		a, cfg, path := approveFixture(t, now, now.Add(48*time.Hour))
		a.codexUserRef = func(ctx context.Context, ref string) (userEvent, error) {
			ev, err := fakeUserRef(ctx, ref)
			ev.Role = c.role
			return ev, err
		}
		if err := a.codexApprove(context.Background(), cfg, []string{"--account", "work", "--said", c.said}); !errors.Is(err, resetRefused) {
			t.Fatalf("%s: код %v, ждали отказ", name, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: файл одобрений записан: %v", name, err)
		}
	}
}

// Разбор вывода tl: заголовок события, многострочный текст до следующего заголовка, id длиннее печатного.
func TestParseTLEvent(t *testing.T) {
	out := "--- 22.11.2026 14:50:04 CET ff4b27b6:298 user\nТриггерни сброс\nвторая строка\n--- 22.11.2026 14:51:00 CET ff4b27b6:299 agent\nответ\n"
	ev, err := parseTLEvent(out, "ff4b27b6:298")
	want := time.Date(2026, 11, 22, 14, 50, 4, 0, time.Local)
	if err != nil || ev.Role != "user" || ev.Text != "Триггерни сброс\nвторая строка" || !ev.At.Equal(want) {
		t.Fatalf("%+v %v", ev, err)
	}
	if ev, err := parseTLEvent(out, "ff4b27b6-1234:299"); err != nil || ev.Role != "agent" || ev.Text != "ответ" {
		t.Fatalf("длинный id: %+v %v", ev, err)
	}
	if ev, err := parseTLEvent(out, "ff4b27b6abcd:298"); err != nil || ev.Role != "user" {
		t.Fatalf("ссылка длиннее печатного id: %+v %v", ev, err)
	}
	if ev, err := parseTLEvent("--- 22.11.2026 14:50:04 CET ff4b27b6-1234-5678:298 user\nx\n", "ff4b27b6:298"); err != nil || ev.Text != "x" {
		t.Fatalf("печатный id длиннее ссылки: %+v %v", ev, err)
	}
	for _, ref := range []string{"ff4b27b6:29", "00000000:298", "ff4b27b6", ":298"} {
		if _, err := parseTLEvent(out, ref); err == nil {
			t.Fatalf("%s: найдено чужое событие", ref)
		}
	}
	if _, err := parseTLEvent("--- 99.99.2026 14:50:04 CET ff4b27b6:298 user\nx\n", "ff4b27b6:298"); err == nil {
		t.Fatal("битое время принято")
	}
}

// Сбой tl (таймаут, не запустился) — не отказ строке: письма лиду «перепишите строку» нет, следующий проход проверит
// её заново; отказ по другой ссылке при непроверенной тоже не отказ — непроверенная могла бы строку держать.
func TestApprovedCodexTLUnavailableNotRejected(t *testing.T) {
	resetsAt := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "approved.tsv")
	if err := os.WriteFile(path, []byte(resetsAt.Format(time.RFC3339)+"\t«сбрось при 100%» (tl 0badbad0:1, tl abcdef12:7)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	down := true
	resolve := func(_ context.Context, ref string) (userEvent, error) {
		switch {
		case ref == "0badbad0:1":
			return userEvent{}, errors.New("tl: exit status 1")
		case down:
			return userEvent{}, fmt.Errorf("%w: %w", errTLUnavailable, context.DeadlineExceeded)
		}
		return userEvent{At: time.Now().Add(-time.Hour), Role: "user", Text: "сбрось при 100%"}, nil
	}
	check := (&approvalCache{}).checker(resolve)
	src, err := check(context.Background(), path, resetsAt)
	if src != "" || err == nil || errors.Is(err, errApprovalRejected) || !errors.Is(err, errTLUnavailable) {
		t.Fatalf("сбой tl: %q %v, ждали errTLUnavailable без отказа", src, err)
	}
	down = false
	if src, err := check(context.Background(), path, resetsAt); err != nil || src == "" {
		t.Fatalf("после сбоя tl строка не взвела: %q %v", src, err)
	}
}

// tlUserRef: код выхода tl — ответ о ссылке (отказ), таймаут — сбой tl.
func TestTLUserRefExitVersusTimeout(t *testing.T) {
	dir := t.TempDir()
	tl := filepath.Join(dir, "tl")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(tl, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	write("echo 'tl: no event at abcdef12:7 (service record, unparsable line or out of range)' >&1; exit 1")
	if _, err := tlUserRef(context.Background(), "abcdef12:7"); err == nil || errors.Is(err, errTLUnavailable) {
		t.Fatalf("код 1: %v, ждали ответ о ссылке", err)
	}
	write("echo 'tl: \"s\": no live session with this name and no transcript with this id' >&2; exit 2")
	if _, err := tlUserRef(context.Background(), "abcdef12:7"); err == nil || errors.Is(err, errTLUnavailable) {
		t.Fatalf("код 2, нет сессии: %v, ждали ответ о ссылке", err)
	}
	write("echo 'tl: transcript: open /fixture/x.jsonl: permission denied' >&2; exit 2")
	if _, err := tlUserRef(context.Background(), "abcdef12:7"); !errors.Is(err, errTLUnavailable) {
		t.Fatalf("код 2, ошибка чтения файлового режима: %v, ждали errTLUnavailable", err)
	}
	write("printf '%s\\n' 'tl: read mask key: x' 'tl: \"s\": no live session with this name and no transcript with this id' >&2; exit 2")
	if _, err := tlUserRef(context.Background(), "abcdef12:7"); err == nil || errors.Is(err, errTLUnavailable) {
		t.Fatalf("код 2, предупреждение перед отсутствующей сессией: %v, ждали ответ об отказе", err)
	}
	write("echo 'tl: read /fixture/transcript.jsonl: permission denied' >&2; exit 2")
	if _, err := tlUserRef(context.Background(), "abcdef12:7"); !errors.Is(err, errTLUnavailable) {
		t.Fatalf("код 2, ошибка чтения db-режима: %v, ждали errTLUnavailable", err)
	}
	write("exec sleep 5")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := tlUserRef(ctx, "abcdef12:7"); !errors.Is(err, errTLUnavailable) {
		t.Fatalf("таймаут: %v, ждали errTLUnavailable", err)
	}
}

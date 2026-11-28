package iowatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Ответ cases пишется по случаю, а не одним буфером (rss-growth-1118: ответ cases — ступень пика 99 МБ), и байт в байт
// совпадает с json.Marshal(Answer{…}): клиенты читают тот же JSON.
func TestCasesAnswerStreamMatchesMarshal(t *testing.T) {
	at := time.Date(2026, 11, 27, 6, 27, 0, 0, time.UTC)
	cover := &CoverRecord{}
	many := []Case{{BootID: "freeze <&>", Start: at, End: at.Add(time.Second)}, {BootID: "stall", Start: at}, {}}
	for _, tc := range []struct {
		name  string
		cases []Case
		cover *CoverRecord
	}{
		{"пусто без учёта", nil, nil},
		{"пустой срез с учётом", []Case{}, cover},
		{"один", many[:1], cover},
		{"много, html-символы", many, cover},
		{"без учёта", many, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := json.Marshal(Answer{V: ProtoVersion, Cases: tc.cases, Coverage: tc.cover})
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := (CasesAnswer{Cases: tc.cases, Coverage: tc.cover}).StreamJSON(&got); err != nil {
				t.Fatal(err)
			}
			if got.String() != string(want) {
				t.Fatalf("поток\n%s\nMarshal\n%s", got.String(), want)
			}
		})
	}
}

// Большой ответ не собирается целиком: самая большая одна запись — меньше десятой части ответа
// (json.Marshal всего ответа — одна запись размером с ответ).
func TestCasesAnswerStreamMemory(t *testing.T) {
	cases := make([]Case, 20000)
	for i := range cases {
		cases[i] = Case{BootID: strings.Repeat("k", 200), Start: time.Unix(int64(i), 0).UTC()}
	}
	var w maxWriter
	if err := (CasesAnswer{Cases: cases, Coverage: &CoverRecord{}}).StreamJSON(&w); err != nil {
		t.Fatal(err)
	}
	if w.max*10 > w.total {
		t.Fatalf("ответ %d Б, самая большая запись %d Б", w.total, w.max)
	}
}

type maxWriter struct{ total, max int }

func (w *maxWriter) Write(b []byte) (int, error) {
	w.total += len(b)
	w.max = max(w.max, len(b))
	return len(b), nil
}

// failWriter отказывает с записи номер failAt и считает попытки после отказа.
type failWriter struct{ calls, failAt, after int }

func (w *failWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls > w.failAt {
		w.after++
	}
	if w.calls >= w.failAt {
		return 0, errors.New("сокет закрыт")
	}
	return len(b), nil
}

// Отказ записи на любом шаге — ошибка с причиной, и после отказа в сокет больше ничего не пишется.
func TestCasesAnswerStreamWriteError(t *testing.T) {
	cases := make([]Case, 3)
	for failAt := 1; failAt <= 11; failAt++ {
		w := &failWriter{failAt: failAt}
		err := CasesAnswer{Cases: cases, Coverage: &CoverRecord{}}.StreamJSON(w)
		if err == nil || !strings.Contains(err.Error(), "сокет закрыт") {
			t.Fatalf("отказ на записи %d: %v", failAt, err)
		}
		if w.after != 0 {
			t.Fatalf("отказ на записи %d: после отказа ещё %d записей", failAt, w.after)
		}
	}
}

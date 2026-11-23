package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/said"
)

// statusApp — app без конфига (путь к битому файлу) и с состоянием said: status конфиг не читает.
func statusApp(t *testing.T, st *said.State) (*app, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("liveness = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{home: dir, cfgPath: cfg, stateDir: filepath.Join(dir, "state"), out: &out, log: slog.New(slog.DiscardHandler)}
	if st != nil {
		if err := os.MkdirAll(a.dir("said"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := st.Save(filepath.Join(a.dir("said"), "state.json")); err != nil {
			t.Fatal(err)
		}
	}
	return a, &out
}

func statusCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	code, ok := exitStatus(err)
	if !ok {
		t.Fatalf("не код выхода: %v", err)
	}
	return code
}

// aguard said status: код 3 — проход стоит, режим не write, реплика ждёт дольше -max; без -max — только вывод; битый
// конфиг не мешает (случай 22.11 14:03).
func TestSaidStatusCommand(t *testing.T) {
	now := time.Now()
	fresh := func() *said.State { return &said.State{Pass: now.Add(-time.Minute), Mode: said.ModeWrite} }
	old := fresh()
	old.Pending = []said.Pending{{Event: said.Event{At: now.Add(-2 * time.Hour)}}}
	stopped := fresh()
	stopped.Pass = now.Add(-2 * time.Hour)
	logMode := fresh()
	logMode.Mode = said.ModeLog
	future := fresh()
	future.Pass = now.Add(time.Hour) // сдвиг часов: не отставание
	offMode := fresh()
	offMode.Mode = said.ModeOff
	unread := fresh()
	unread.Unread = "a.jsonl: permission denied"
	cases := []struct {
		name string
		st   *said.State
		max  string
		code int
		want string
	}{
		{"живой", fresh(), "30m", 0, "очередь пуста"},
		{"реплика ждёт", old, "30m", 3, "ждёт разметки"},
		{"реплика в пределах", old, "3h", 0, "в очереди 1"},
		{"реплика чуть дольше", old, "119m", 3, "ждёт разметки"},
		{"проход из будущего", future, "30m", 0, "очередь пуста"},
		{"демон стоит", stopped, "30m", 3, "последний проход"},
		{"режим log", logMode, "30m", 3, "said.tsv не пишется"},
		{"режим off", offMode, "30m", 3, "said.tsv не пишется"},
		{"не прочиталось", unread, "30m", 3, "не прочиталась"},
		{"нет состояния", nil, "30m", 3, "прохода с записью состояния не было"},
		{"без max", old, "0", 0, "в очереди 1"},
	}
	for _, c := range cases {
		a, out := statusApp(t, c.st)
		err := a.saidCmd(context.Background(), []string{"status", "-max", c.max})
		if got := statusCode(t, err); got != c.code || !strings.Contains(out.String(), c.want) {
			t.Fatalf("%s: код %d (ждали %d), вывод %q", c.name, got, c.code, out.String())
		}
	}
	broken, _ := statusApp(t, fresh())
	if err := os.WriteFile(filepath.Join(broken.dir("said"), "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := broken.saidCmd(context.Background(), []string{"status"}); err == nil {
		t.Fatal("битое состояние — ошибка, а не «не отстаёт»")
	}
	a, out := statusApp(t, old)
	if got := statusCode(t, a.saidCmd(context.Background(), []string{"status", "-max", "30m", "-json"})); got != 3 {
		t.Fatalf("json: код %d", got)
	}
	var got struct {
		said.Lag
		Stale []string `json:"stale"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Queue != 1 || got.Mode != said.ModeWrite || len(got.Stale) != 1 {
		t.Fatalf("%v %+v %s", err, got, out.String())
	}
	none, out := statusApp(t, nil)
	if got := statusCode(t, none.saidCmd(context.Background(), []string{"status", "-json"})); got != 0 || !strings.Contains(out.String(), `"pass":`) {
		t.Fatalf("без прохода поле pass всё равно есть — по нему ответ отличают от справки: %d %s", got, out.String())
	}
}

// Проход, дошедший до записи, ставит метку и режим: по ним status видит живой указатель.
func TestSaidRoundStampsPass(t *testing.T) {
	b := newSaidBench(t)
	b.roundDirs(t)
	b.c.Said.Mode = said.ModeLog
	if err := b.a.saidRound(context.Background(), b.c, false); err != nil {
		t.Fatal(err)
	}
	st, err := said.LoadState(filepath.Join(b.a.dir("said"), "state.json"))
	if err != nil || st.Mode != said.ModeLog {
		t.Fatalf("режим log: %v %+v", err, st)
	}
	b.c.Said.Mode = said.ModeWrite
	start := time.Now()
	if err := b.a.saidRound(context.Background(), b.c, false); err != nil {
		t.Fatal(err)
	}
	st, err = said.LoadState(filepath.Join(b.a.dir("said"), "state.json"))
	if err != nil || st.Pass.Before(start) || st.Mode != said.ModeWrite || st.Unread != "" {
		t.Fatalf("%v %+v", err, st)
	}
	// Нечитаемый транскрипт: проход идёт дальше, ошибка — в состоянии для status; починка её снимает.
	dir := filepath.Join(b.a.projects, "-h-git-p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "0123456789ab.jsonl")
	if err := os.WriteFile(f, []byte("{}\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := b.a.saidRound(context.Background(), b.c, false); err != nil {
		t.Fatal(err)
	}
	if st, err = said.LoadState(filepath.Join(b.a.dir("said"), "state.json")); err != nil || st.Unread == "" {
		t.Fatalf("ошибка чтения не в состоянии: %v %+v", err, st)
	}
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.a.saidRound(context.Background(), b.c, false); err != nil {
		t.Fatal(err)
	}
	if st, err = said.LoadState(filepath.Join(b.a.dir("said"), "state.json")); err != nil || st.Unread != "" {
		t.Fatalf("починка не сняла ошибку: %v %+v", err, st)
	}
}

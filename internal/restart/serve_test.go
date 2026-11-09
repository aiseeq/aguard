package restart

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/linesock"
	"github.com/aiseeq/aguard/internal/screen"
)

func ask(t *testing.T, socket, line string) Answer {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatal(err)
	}
	b, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var a Answer
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestServe(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "aguard", "aguard.sock")
	ln, err := linesock.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, func(r Request) Answer {
			return Answer{V: Version, Verdict: Verdict{Restart: r.Pid == 7}, Name: r.Name}
		}, slog.New(slog.DiscardHandler))
	}()
	if a := ask(t, socket, `{"v":1,"pid":7,"name":"x-master"}`); !a.Restart || a.Name != "x-master" {
		t.Errorf("ответ %+v", a)
	}
	if a := ask(t, socket, `не json`); a.Error == "" || a.Restart {
		t.Errorf("кривой вопрос: %+v", a)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAnswerVersion(t *testing.T) {
	cfg, cc := Default(t.TempDir(), t.TempDir()), DefaultCompact()
	a := NewJudge(t.TempDir()).Answer(&cfg, &cc, Request{V: Version + 1, Pid: 1}, time.Now())
	if a.Error == "" || a.Restart || a.V != Version {
		t.Errorf("чужая версия: %+v", a)
	}
}

func TestAnswerCompactOff(t *testing.T) {
	cfg, cc := Default(t.TempDir(), t.TempDir()), DefaultCompact()
	cc.Mode = CompactOff
	a := NewJudge(t.TempDir()).Answer(&cfg, &cc, Request{V: Version, Pid: 1 << 22}, time.Now())
	if a.Compact || len(a.CompactAgainst) == 0 || a.CompactAgainst[0].Code != "off" {
		t.Errorf("сжатие выключено: %+v", a)
	}
}

// Judge целиком на подменённой сессии: что cl сказал в вопросе, доходит до решений — унаследованные таймеры из
// транскрипта уходят в lost при любом typed, clean снимает «набранный текст» с подсказки и для перезапуска, и для
// сжатия, а пустое по экрану поле — в ответе field_empty (c794bfa).
func TestAnswerUsesRequest(t *testing.T) {
	now := time.Now()
	carry := `{"type":"attachment","timestamp":"` + now.Add(-30*time.Minute).UTC().Format(time.RFC3339Nano) +
		`","attachment":{"type":"session_cron_carry","otherArmed":[{"id":"B","cron":"11 6 * * *","prompt":"Дайджест","recurring":true}]}}`
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 20 * time.Minute, ttyAgo: 30 * time.Minute}, carry)
	w.link(t, 4242)
	cfg, cc := Default(t.TempDir(), t.TempDir()), DefaultCompact()
	j := NewJudge(t.TempDir())
	j.paths = &w.p
	answer := func(r Request) Answer {
		r.V, r.Pid = Version, 4242
		return j.Answer(&cfg, &cc, r, now)
	}
	held := func(rs []Reason, code string) bool {
		return slices.ContainsFunc(rs, func(r Reason) bool { return r.Code == code })
	}

	for _, typed := range []bool{false, true} {
		a := answer(Request{Typed: typed})
		if held(a.Against, "crons") || len(a.Lost) != 1 || a.Lost[0].Cron != "11 6 * * *" || a.Lost[0].Prompt != "Дайджест" || !a.FieldEmpty {
			t.Errorf("typed=%v: унаследованный таймер не держит, а уходит в lost; поле пусто: %+v", typed, a)
		}
	}

	if a := answer(Request{Stale: "файл настроек роли: нет → /x.json"}); a.Stale != "файл настроек роли: нет → /x.json" {
		t.Errorf("stale вопроса — в ответе для журнала cl: %q", a.Stale)
	}

	w.p.prompt = func(int) (screen.Prompt, error) { return screen.Draft, nil }
	if a := answer(Request{}); !held(a.Against, "screen") || !held(a.CompactAgainst, "screen") || a.FieldEmpty {
		t.Errorf("текст в поле, cl о нём не знает — держит перезапуск и сжатие: %+v", a)
	}
	if a := answer(Request{Clean: true}); held(a.Against, "screen") || held(a.CompactAgainst, "screen") {
		t.Errorf("cl: человек не набирал — текст в поле подсказка, не держит ни перезапуск, ни сжатие: against %+v, compact %+v",
			a.Against, a.CompactAgainst)
	}
}

// Ответ проходит через JSON целиком: поля вердикта не затеняются полями ответа с тем же именем.
func TestAnswerJSONRoundTrip(t *testing.T) {
	in := Answer{V: Version, Verdict: Verdict{Restart: true, ArgsStale: true}, Stale: "файл настроек роли: нет → /x.json"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Answer
	if err := json.Unmarshal(data, &out); err != nil || !out.ArgsStale || out.Stale != in.Stale || !out.Restart {
		t.Errorf("%s → %+v, %v", data, out, err)
	}
}

// Judge целиком на настоящем транскрипте: сжатие разрешено — таймеры прошлой беседы в lost при любой версии, а вердикт
// перезапуска при свежей транскрипт не читает, при нечитаемой установленной вообще выходит раньше (тикет cl-compact-lost-crons).
func TestAnswerCompactLostCrons(t *testing.T) {
	const b = `{"id":"B","cron":"11 6 * * *","prompt":"Дайджест","recurring":true}`
	inheritedOnly := `{"type":"session_cron_carry","otherArmed":[` + b + `]}`
	mixed := `{"type":"session_cron_carry","tasks":[{"id":"A","cron":"5 5 * * *","prompt":"Свой","recurring":true}],"otherArmed":[` + b + `]}`
	world := func(t *testing.T, now time.Time, carry string) (sessionWorld, func() Answer) {
		req := now.Add(-55 * time.Minute)
		at := func(x time.Time) string { return x.UTC().Format(time.RFC3339Nano) }
		w := newSessionWorld(t, worldTimes{now: now, turnAgo: 54 * time.Minute, ttyAgo: 30 * time.Minute},
			`{"type":"attachment","timestamp":"`+at(req.Add(-time.Minute))+`","attachment":`+carry+`}`,
			`{"type":"user","timestamp":"`+at(req)+`","message":{"role":"user","content":"hi"}}`,
			answer(at(req.Add(time.Minute)), "false", 10, 150000, 10, 10))
		w.link(t, 4244)
		cfg, cc := Default(t.TempDir(), t.TempDir()), DefaultCompact()
		cc.ResetMinTokens = 1 << 40
		j := NewJudge(t.TempDir())
		j.paths = &w.p
		return w, func() Answer { return j.Answer(&cfg, &cc, Request{V: Version, Pid: 4244}, now) }
	}
	wantB := func(t *testing.T, a Answer) {
		t.Helper()
		if !a.Compact || len(a.Lost) != 1 || a.Lost[0].ID != "B" || a.Lost[0].Cron != "11 6 * * *" {
			t.Errorf("сжатие разрешено, в lost только B: %+v", a)
		}
	}
	now := time.Now()
	for _, c := range []struct {
		name, carry string
		fresh       bool
	}{{"свежая, только унаследованный", inheritedOnly, true}, {"свежая, свой и унаследованный", mixed, true},
		{"старая, только унаследованный", inheritedOnly, false}, {"старая, свой и унаследованный", mixed, false}} {
		t.Run(c.name, func(t *testing.T) {
			w, ask := world(t, now, c.carry)
			if c.fresh {
				w.setRunning(t, "2.1.310")
			}
			a := ask()
			if a.Fresh != c.fresh {
				t.Fatalf("fresh=%v, want %v", a.Fresh, c.fresh)
			}
			wantB(t, a)
		})
	}
	t.Run("установленная версия не читается", func(t *testing.T) {
		w, ask := world(t, now, inheritedOnly)
		if err := os.Remove(w.p.claude); err != nil {
			t.Fatal(err)
		}
		wantB(t, ask())
	})
}

// Judge целиком: stale вопроса доходит до решения — версия свежая, без stale Fresh, со stale простаивающая сессия
// получает перезапуск ради аргументов (args_stale).
func TestAnswerStale(t *testing.T) {
	now := time.Now()
	w := newSessionWorld(t, worldTimes{now: now, turnAgo: 20 * time.Minute, ttyAgo: 30 * time.Minute})
	w.setRunning(t, "2.1.310")
	w.link(t, 4243)
	cfg, cc := Default(t.TempDir(), t.TempDir()), DefaultCompact()
	j := NewJudge(t.TempDir())
	j.paths = &w.p
	// Сначала stale: вопрос читает терминал, и его atime у второго вопроса был бы свежим.
	stale := "файл настроек роли: нет → /x.json"
	if a := j.Answer(&cfg, &cc, Request{V: Version, Pid: 4243, Stale: stale}, now); a.Fresh || !a.Restart || !a.ArgsStale || a.Stale != stale {
		t.Errorf("stale в простое — перезапуск ради аргументов: %+v, stale %q", a.Verdict, a.Stale)
	}
	if a := j.Answer(&cfg, &cc, Request{V: Version, Pid: 4243}, now); !a.Fresh || a.Restart {
		t.Errorf("свежая версия без stale: %+v", a.Verdict)
	}
}

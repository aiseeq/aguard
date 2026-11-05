package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const resetHistory = `{"at":"2026-10-31T07:28:26+01:00","sevenDayUsedPercentage":100,"sevenDayResetsAt":"2026-11-04T01:00:00+01:00","account":"waited@example.com"}
{"at":"2026-11-03T23:53:40+01:00","sevenDayUsedPercentage":99,"sevenDayResetsAt":"2026-11-09T11:00:00+01:00","account":"active@example.com"}
{"at":"2026-11-04T01:00:01+01:00","sevenDayUsedPercentage":0,"sevenDayResetsAt":"2026-11-11T00:00:00Z","account":"waited@example.com"}
`

func TestLimitResetCheckExitCodes(t *testing.T) {
	file := func(body string) string {
		p := filepath.Join(t.TempDir(), "limits-history.jsonl")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	before := resetHistory[:strings.LastIndex(strings.TrimSuffix(resetHistory, "\n"), "\n")+1]
	const waited = "waited@example.com"
	const since = "2026-11-03T20:22:11+01:00"
	for _, c := range []struct {
		name string
		args []string
		at   string
		want error
		out  string
	}{
		{"00:03 отказ", []string{"--account", waited, "--since", since, "--history", file(before)}, "2026-11-04T00:03:09+01:00", resetRefused, "через 57 мин"},
		{"01:04 проходит", []string{"--account", waited, "--since", since, "--history", file(resetHistory)}, "2026-11-04T01:04:00+01:00", nil, "сброс подтверждён"},
		{"короткое имя", []string{"--account", "waited", "--since", since, "--history", file(resetHistory)}, "2026-11-04T01:04:00+01:00", resetBadArgs, ""},
		{"битая строка", []string{"--account", waited, "--since", since, "--history", file(resetHistory + `{"at":"2026-11-05`)}, "2026-11-05T14:12:00+01:00", resetBadArgs, ""},
		{"нет файла", []string{"--account", waited, "--since", since, "--history", filepath.Join(t.TempDir(), "нет")}, "2026-11-04T01:04:00+01:00", resetBadArgs, ""},
		{"без since", []string{"--account", waited, "--history", file(resetHistory)}, "2026-11-04T01:04:00+01:00", resetBadArgs, ""},
		{"since не RFC3339", []string{"--account", waited, "--since", "полночь", "--history", file(resetHistory)}, "2026-11-04T01:04:00+01:00", resetBadArgs, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			a := &app{out: &out}
			at, err := time.Parse(time.RFC3339, c.at)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.limitResetCheck(c.args, at); !errors.Is(got, c.want) {
				t.Fatalf("код %v, нужен %v; вывод: %s", got, c.want, out.String())
			}
			if !strings.Contains(out.String(), c.out) {
				t.Fatalf("в выводе нет %q: %s", c.out, out.String())
			}
		})
	}
}

// Битая строка называет свой номер: иначе лид не найдёт, что чинить.
func TestLimitResetCheckBrokenLineNumber(t *testing.T) {
	p := filepath.Join(t.TempDir(), "limits-history.jsonl")
	if err := os.WriteFile(p, []byte(resetHistory+`{"at":"2026-11-05`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{out: &bytes.Buffer{}}
	_, err := a.runResetCheck("waited@example.com", "2026-11-03T20:22:11+01:00", p, time.Now())
	if err == nil || !strings.Contains(err.Error(), "строка 4") {
		t.Fatalf("нужна ошибка со строкой 4, получено %v", err)
	}
}

// Таймер лида смотрит на код выхода процесса: проверяем его самого, через main() в дочернем процессе теста.
func TestLimitResetCheckProcessExit(t *testing.T) {
	if args := os.Getenv("AGUARD_TEST_MAIN_ARGS"); args != "" {
		os.Args = append([]string{"aguard"}, strings.Split(args, "\x1f")...)
		main()
		return
	}
	now := time.Now()
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	row := func(at, resets time.Duration) string {
		return fmt.Sprintf(`{"at":%q,"sevenDayResetsAt":%q,"sevenDayUsedPercentage":50,"account":"g"}`+"\n", stamp(at), stamp(resets))
	}
	home := t.TempDir()
	hist := filepath.Join(home, "limits-history.jsonl")
	if err := os.WriteFile(hist, []byte(row(-48*time.Hour, -24*time.Hour)+row(-time.Hour, 144*time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(since string, extra ...string) []string {
		return append([]string{"limit-reset-check", "--account", "g", "--since", since, "--history", hist}, extra...)
	}
	for _, c := range []struct {
		name string
		args []string
		want int
	}{
		{"сброс есть", check(stamp(-72 * time.Hour)), 0},
		{"сброса нет", check(stamp(-time.Minute)), 1},
		{"нет --since", []string{"limit-reset-check", "--account", "g", "--history", hist}, 2},
		{"лишний аргумент", check(stamp(-72*time.Hour), "хвост"), 2},
		{"неизвестный флаг", []string{"limit-reset-check", "--nope"}, 2},
		{"ошибка другой команды — как раньше", []string{"report", "-at", "nonsense"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestLimitResetCheckProcessExit$")
			cmd.Env = append(os.Environ(), "HOME="+home, "AGUARD_TEST_MAIN_ARGS="+strings.Join(c.args, "\x1f"))
			out, err := cmd.CombinedOutput()
			got := 0
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("код %d, нужен %d; вывод: %s", got, c.want, out)
			}
		})
	}
}

func TestExitStatus(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		ok   bool
	}{{resetRefused, 1, true}, {resetBadArgs, 2, true}, {errors.New("сбой"), 0, false}, {nil, 0, false}} {
		if code, ok := exitStatus(c.err); code != c.code || ok != c.ok {
			t.Errorf("exitStatus(%v) = %d, %v", c.err, code, ok)
		}
	}
}

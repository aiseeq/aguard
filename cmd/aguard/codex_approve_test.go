package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/akit/codexlimit"
)

const codexApproveSaid = "«сбрось при 100%» (2026-11-16 11:00 CET, tl abcdef12:5)"

// fakeUserRef — реплика пользователя 16.11 10:00 UTC со словами одобрения; другие ссылки — ошибка.
func fakeUserRef(_ context.Context, ref string) (userEvent, error) {
	if ref != "abcdef12:5" {
		return userEvent{}, fmt.Errorf("no event at %s", ref)
	}
	return userEvent{At: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), Role: "user", Text: "Сбрось при 100%, пожалуйста"}, nil
}

func TestCodexApproveWritesSelectedAccountAndRejectsRepeat(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 11, 16, 13, 42, 10, 0, time.FixedZone("CET", 3600))
	var out bytes.Buffer
	a := &app{
		home: home, cfgPath: config.Path(home), uid: os.Getuid(), out: &out,
		now: func() time.Time { return at }, codexUserRef: fakeUserRef,
		codexRead: func(_ context.Context, _, codexHome string, now time.Time) (codexwatch.Reading, error) {
			if codexHome != filepath.Join(home, ".codex-personal") || !now.Equal(at) {
				t.Fatalf("codexRead home/time = %q/%s", codexHome, now)
			}
			return codexwatch.Reading{ResetsAt: at.Add(48 * time.Hour)}, nil
		},
	}
	args := []string{"codex", "approve", "--account", "personal", "--said", codexApproveSaid}
	if err := a.dispatch(context.Background(), args[0], args[1:]); err != nil {
		t.Fatalf("approve: %v", err)
	}
	cfg := codexwatch.Default(home)
	for _, acc := range cfg.Accounts {
		data, err := os.ReadFile(acc.Approvals)
		if acc.Name == "personal" {
			if err != nil {
				t.Fatal(err)
			}
			wantLine := at.Add(48*time.Hour).Format(time.RFC3339) + "\t" + args[5] + "\n"
			if string(data) != wantLine {
				t.Fatalf("personal approvals = %q, want %q", data, wantLine)
			}
			approvals, err := codexlimit.ReadApproved(acc.Approvals)
			if err != nil {
				t.Fatal(err)
			}
			if source, ok := approvals.For(at.Add(48 * time.Hour)); !ok || source != args[5] {
				t.Fatalf("parsed approval = %q, %v", source, ok)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("account %s approvals read: %q, %v", acc.Name, data, err)
		}
	}
	if !strings.Contains(out.String(), at.Add(48*time.Hour).Format(time.RFC3339)) || !strings.Contains(out.String(), "при 100%") {
		t.Fatalf("unexpected output: %s", out.String())
	}
	if err := a.dispatch(context.Background(), args[0], args[1:]); !errors.Is(err, resetRefused) {
		t.Fatalf("repeat code = %v, want 1", err)
	}
	data, err := os.ReadFile(cfg.Accounts[1].Approvals)
	if err != nil || strings.Count(string(data), args[5]) != 1 {
		t.Fatalf("repeat duplicated line: %q, %v", data, err)
	}
}

func TestCodexApproveUnknownAccountAndMissingReset(t *testing.T) {
	home := t.TempDir()
	a := &app{
		home: home, cfgPath: config.Path(home), uid: os.Getuid(), out: &bytes.Buffer{},
		codexRead: func(context.Context, string, string, time.Time) (codexwatch.Reading, error) {
			return codexwatch.Reading{}, nil
		},
	}
	for _, tc := range []struct {
		name string
		args []string
		want error
	}{
		{"unknown", []string{"approve", "--account", "other", "--said", "yes"}, resetBadArgs},
		{"missing resetsAt", []string{"approve", "--account", "work", "--said", "yes"}, resetRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := a.codexCmd(context.Background(), tc.args); !errors.Is(err, tc.want) {
				t.Fatalf("codexCmd error = %v, want %v", err, tc.want)
			}
		})
	}
}

func approveFixture(t *testing.T, now, resetsAt time.Time) (*app, *codexwatch.Config, string) {
	t.Helper()
	home := t.TempDir()
	accounts := codexwatch.Default(home).Accounts
	cfg := &codexwatch.Config{Mode: codexwatch.ModeAlert, MinIdle: policy.Duration(24 * time.Hour), Accounts: accounts}
	var out bytes.Buffer
	a := &app{home: home, out: &out, now: func() time.Time { return now }, codexUserRef: fakeUserRef}
	a.codexRead = func(context.Context, string, string, time.Time) (codexwatch.Reading, error) {
		return codexwatch.Reading{ResetsAt: resetsAt}, nil
	}
	return a, cfg, accounts[0].Approvals
}

func approveArgs(extra ...string) []string {
	return append([]string{"--account", "work", "--said", codexApproveSaid}, extra...)
}

func TestCodexApproveValidationAndConfigErrorCodes(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	resetsAt := now.Add(48 * time.Hour)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"empty", []string{"--account", "work", "--said", ""}},
		{"tab", []string{"--account", "work", "--said", "bad\tsource"}},
		{"newline", []string{"--account", "work", "--said", "bad\nsource"}},
		{"positional", append(approveArgs(), "extra")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, cfg, path := approveFixture(t, now, resetsAt)
			if err := a.codexApprove(context.Background(), cfg, tc.args); !errors.Is(err, resetBadArgs) {
				t.Fatalf("approve error = %v, want code 2", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("approval file exists after invalid input: %v", err)
			}
		})
	}

	home := t.TempDir()
	configPath := config.Path(home)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{home: home, cfgPath: configPath, uid: os.Getuid(), out: &bytes.Buffer{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.codexCmd(context.Background(), append([]string{"approve"}, approveArgs()...)); !errors.Is(err, resetBadArgs) {
		t.Fatalf("config failure code = %v, want 2", err)
	}
}

func TestCodexApproveAppendWithoutTrailingNewlineUsesOneWrite(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	resetsAt := now.Add(48 * time.Hour)
	a, cfg, path := approveFixture(t, now, resetsAt)
	old := "2026-11-10T12:00:00Z\tprevious approval"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	writes := 0
	a.codexApprovalWrite = func(f *os.File, b []byte) (int, error) {
		writes++
		if got, want := string(b), "\n"+resetsAt.Format(time.RFC3339)+"\t"+codexApproveSaid+"\n"; got != want {
			t.Errorf("single append payload = %q, want %q", got, want)
		}
		return f.Write(b)
	}
	if err := a.codexApprove(context.Background(), cfg, approveArgs()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := old + "\n" + resetsAt.Format(time.RFC3339) + "\t" + codexApproveSaid + "\n"
	if string(data) != want || writes != 1 {
		t.Fatalf("approval = %q, writes %d; want %q, one write", data, writes, want)
	}
}

func TestCodexApproveWriteFailuresDoNotChangeApprovalFile(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	resetsAt := now.Add(48 * time.Hour)
	for _, tc := range []struct {
		name  string
		setup func(*app)
	}{
		{"read only", func(a *app) {
			a.codexApprovalOpen = func(string, int, os.FileMode) (*os.File, error) { return nil, os.ErrPermission }
		}},
		{"write error", func(a *app) {
			a.codexApprovalWrite = func(*os.File, []byte) (int, error) { return 0, errors.New("injected write failure") }
		}},
		{"short write", func(a *app) {
			a.codexApprovalWrite = func(f *os.File, b []byte) (int, error) {
				return f.Write(b[:len(b)/2])
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, cfg, path := approveFixture(t, now, resetsAt)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			before := []byte("2026-11-10T12:00:00Z\tprevious approval\n")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			tc.setup(a)
			if err := a.codexApprove(context.Background(), cfg, approveArgs()); !errors.Is(err, resetBadArgs) {
				t.Fatalf("approve error = %v, want code 2", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("approval file changed: %q, %v", after, err)
			}
		})
	}
}

func TestCodexApproveRejectsUnverifiedWrite(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	resetsAt := now.Add(48 * time.Hour)
	a, cfg, path := approveFixture(t, now, resetsAt)
	a.codexApprovalWrite = func(f *os.File, b []byte) (int, error) {
		bad := append([]byte(nil), b...)
		bad[len(bad)-2] ^= 1 // change the source while still reporting a full successful write
		return f.Write(bad)
	}
	if err := a.codexApprove(context.Background(), cfg, approveArgs()); !errors.Is(err, resetBadArgs) {
		t.Fatalf("approve error = %v, want code 2 for failed verification", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("test did not write the deliberately altered approval: %v", err)
	}
}

func TestCodexApproveReadErrorsAndBrokenApprovalFile(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	resetsAt := now.Add(48 * time.Hour)
	t.Run("limit read error", func(t *testing.T) {
		a, cfg, path := approveFixture(t, now, resetsAt)
		a.codexRead = func(context.Context, string, string, time.Time) (codexwatch.Reading, error) {
			return codexwatch.Reading{}, errors.New("injected read error")
		}
		if err := a.codexApprove(context.Background(), cfg, approveArgs()); !errors.Is(err, resetRefused) {
			t.Fatalf("approve error = %v, want code 1", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("approval file created after read error: %v", err)
		}
	})
	t.Run("broken existing file", func(t *testing.T) {
		a, cfg, path := approveFixture(t, now, resetsAt)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		before := []byte("not an approval line\n")
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := a.codexApprove(context.Background(), cfg, approveArgs()); !errors.Is(err, resetBadArgs) {
			t.Fatalf("approve error = %v, want code 2", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("broken approval file changed: %q, %v", after, err)
		}
	})
}

func TestCodexApproveDeadlineAndLogMode(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	t.Run("deadline passed", func(t *testing.T) {
		resetsAt := now.Add(24*time.Hour - time.Second)
		a, cfg, path := approveFixture(t, now, resetsAt)
		if err := a.codexApprove(context.Background(), cfg, approveArgs()); !errors.Is(err, resetRefused) {
			t.Fatalf("approve error = %v, want code 1", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("approval file created after deadline: %v", err)
		}
		if got := a.out.(*bytes.Buffer).String(); !strings.Contains(got, "до планового сброса меньше 24 ч: aguard не сбросит") {
			t.Fatalf("deadline message missing: %s", got)
		}
	})
	t.Run("log mode", func(t *testing.T) {
		resetsAt := now.Add(48 * time.Hour)
		a, cfg, path := approveFixture(t, now, resetsAt)
		cfg.Mode = codexwatch.ModeLog
		if err := a.codexApprove(context.Background(), cfg, approveArgs()); err != nil {
			t.Fatal(err)
		}
		approvals, err := codexlimit.ReadApproved(path)
		if err != nil {
			t.Fatal(err)
		}
		if source, ok := approvals.For(resetsAt); !ok || source != codexApproveSaid {
			t.Fatalf("log mode approval = %q, %v", source, ok)
		}
		if got := a.out.(*bytes.Buffer).String(); !strings.Contains(got, "режим log: aguard сброс не выполнит") {
			t.Fatalf("log mode message missing: %s", got)
		}
	})
}

func TestCodexApproveDeadlineMatchesCodexDecide(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cfg := codexwatch.Default(t.TempDir())
	resetsAt := now.Add(48 * time.Hour)
	deadline := cfg.EarlyResetDeadline(resetsAt)
	state := codexwatch.State{}
	reading := codexwatch.Reading{At: deadline, ResetsAt: resetsAt, Used: 100, Armed: "source", Available: 1}
	_, _, consume := state.Apply(reading, &cfg)
	if consume {
		t.Fatal("decide consumed at deadline; it must match EarlyResetDeadline's strict cutoff")
	}
}

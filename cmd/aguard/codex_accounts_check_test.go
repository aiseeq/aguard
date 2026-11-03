package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/codexwatch"
)

// Сбой соседнего замера и потеря ответа не смешивают ключи и не повторяют успешный сброс.
func TestCodexAccountsRetryOwnKeys(t *testing.T) {
	reset := time.Now().Add(5 * 24 * time.Hour)
	failRead := true
	a, _, cfg := twoAccounts(t, map[string]func(time.Time) (codexwatch.Reading, error){
		"work": func(at time.Time) (codexwatch.Reading, error) {
			if failRead {
				return codexwatch.Reading{}, fmt.Errorf("нет замера")
			}
			return limit(100, reset, 1)(at)
		},
		"personal": limit(100, reset, 1),
	})
	cfg.Mode = codexwatch.ModeAlert
	a.codexApproved = func(string, time.Time) (string, error) { return "да", nil }
	a.codexNote = func(context.Context, string, string, string, string) error { return nil }
	calls := map[string][]string{}
	a.codexConsume = func(_ context.Context, _, home, key string) (string, error) {
		calls[home] = append(calls[home], key)
		if home == cfg.Accounts[1].Home && len(calls[home]) == 1 {
			return "", fmt.Errorf("нет ответа")
		}
		return "reset", nil
	}
	if err := a.codexRound(context.Background(), &cfg, false); err == nil {
		t.Fatal("нет ошибки замера")
	}
	failRead = false
	for range 2 {
		if err := a.codexRound(context.Background(), &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	work, personal := calls[cfg.Accounts[0].Home], calls[cfg.Accounts[1].Home]
	if len(work) != 1 || len(personal) != 2 || work[0] == "" || personal[0] == "" || work[0] == personal[0] || personal[0] != personal[1] {
		t.Fatalf("ключи work=%q personal=%q", work, personal)
	}
}

// После сбоя записи исхода на диске остаётся ключ вызова: повтор использует его же.
func TestCodexSaveFailureKeepsConsumeKey(t *testing.T) {
	a, _, cfg := armedApp(t)
	path := filepath.Join(a.dir("codex"), "work.json")
	backup := path + ".backup"
	var keys []string
	a.codexNote = func(context.Context, string, string, string, string) error { return nil }
	a.codexConsume = func(_ context.Context, _, _, key string) (string, error) {
		keys = append(keys, key)
		if len(keys) == 1 {
			if err := os.Rename(path, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return "reset", nil
	}
	if err := a.codexRound(context.Background(), &cfg, false); err == nil {
		t.Fatal("нет ошибки сохранения")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.codexRound(context.Background(), &cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("ключи вызовов: %q", keys)
	}
}

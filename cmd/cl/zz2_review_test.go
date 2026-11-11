package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aiseeq/akit/session"
)

// Кривой memory.max: loadConfig отдаёт роли, но с ошибкой — вызывающие считают конфиг битым.
func TestReview2BadMemoryMaxDropsRoles(t *testing.T) {
	dir := t.TempDir()
	good := `{"restart":{"mode":"log"},"roles":[{"names":["x-master"],"prompt":"ROLE {name}"}]}`
	bad := `{"memory":{"max":"zzz"},"restart":{"mode":"log"},"roles":[{"names":["x-master"],"prompt":"ROLE {name}"}]}`
	for label, body := range map[string]string{"good": good, "bad": bad} {
		file := filepath.Join(dir, label+".json")
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		l := launcher{home: dir, configFile: file, name: "x-master", dir: dir}
		got := l.withRoleFlags([]string{"-n", "x-master"}, "x-master")
		cfg, err := loadConfig(file, dir)
		t.Logf("%s: roles=%d err=%v withRoleFlags=%q", label, len(cfg.Roles), err != nil, got)
		if !slices.Contains(got, "--append-system-prompt") {
			t.Errorf("%s: role prompt dropped", label)
		}
	}
}

func TestReview2BadMemoryConfigDoesNotAbortCheck(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	body := `{"memory":{"max":"broken"},"restart":{"mode":"log"},"roles":[{"names":["x-master"],"prompt":"ROLE {name}"}]}`
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	l := launcher{home: home, configFile: configPath, registry: session.ForHome(home)}
	cfg, err := l.readConfig()
	if err != nil || cfg.Memory.Max != "" || len(cfg.Roles) != 1 {
		t.Fatalf("memory error should disable only ceiling: cfg=%+v err=%v", cfg, err)
	}
	if code := runCheck(l); code != 0 {
		t.Fatalf("cl --cl-check aborted on memory.max error: %d", code)
	}
}

package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/ceiling"
)

func TestCeilingDefault(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "cl", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		a := &app{home: home, out: &out}
		err := a.ceilingCmd(args)
		return strings.TrimSpace(out.String()), err
	}
	if got, err := run("--default"); err != nil || got != "потолка нет" {
		t.Fatalf("missing config: got %q, err %v", got, err)
	}
	for _, tc := range []struct{ value, want string }{
		{`{"memory":{"max":""}}`, "потолка нет"},
		{`{"memory":{"max":"  ","swap_max":"abc"}}`, "потолка нет"},
		{`{"memory":{"max":"12G"}}`, "12884901888"},
	} {
		if err := os.WriteFile(configPath, []byte(tc.value), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := run("--default"); err != nil || got != tc.want {
			t.Errorf("config %s: got %q, err %v; want %q", tc.value, got, err, tc.want)
		}
	}
	if err := os.WriteFile(configPath, []byte(`{"memory":{"max":"abc"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := run("--default"); err == nil || got != "" {
		t.Errorf("bad max: got %q, err %v", got, err)
	}
	if got, err := run("-h"); err != nil || !strings.Contains(got, "--default") {
		t.Errorf("help: got %q, err %v", got, err)
	}
	if err := os.WriteFile(configPath, []byte(`{"memory":{"max":"12G"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := run("--default", "5G"); err == nil || got != "" {
		t.Errorf("--default with extra size: got %q, err %v", got, err)
	}
	if err := os.WriteFile(configPath, []byte(`{"memory":{"max":"12G","swap_max":"abc"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := run("--default"); err == nil || got != "" {
		t.Errorf("invalid swap must disable configured base: got %q, err %v", got, err)
	}
}

func TestCeilingDefaultPercentUsesMemTotal(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "cl", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"memory":{"max":"34%"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	memTotal, err := ceiling.HostMemoryTotal()
	if err != nil {
		t.Fatal(err)
	}
	want := int64(math.Round(float64(memTotal) * 0.34))
	var out bytes.Buffer
	a := &app{home: home, out: &out}
	if err := a.ceilingCmd([]string{"--default"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != fmt.Sprint(want) {
		t.Fatalf("34%% of MemTotal: got %q, want %d", got, want)
	}
}

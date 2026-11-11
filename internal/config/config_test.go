package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/orphans"
	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/queue"
	"github.com/aiseeq/aguard/internal/restart"
	"github.com/aiseeq/aguard/internal/sigguard"
)

const home = "/home/u"

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	p := write(t, `lead = "boss"
[admins]
"~/proj" = "proj-admin"
[orphans]
mode = "act"
min_dead = "2h"
[orphans.mute_until]
"~/proj" = "2026-10-26"
[[orphans.exclude]]
comm = "catatonit"
reason = "pause"
[tmp]
mode = "alert"
interval = "2m"
[tmp.policy]
min_entry = "2MB"
budget_gb_min = 6
[memory]
interval = "30s"
`)
	c, err := Load(p, home, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if c.Orphans.Mode != orphans.ModeAct || c.Orphans.MinDead.D() != 2*time.Hour || len(c.Orphans.Exclude) != 1 {
		t.Errorf("orphans разобран неверно: %+v", c.Orphans)
	}
	if c.Orphans.MuteUntil["/home/u/proj"] != "2026-10-26" {
		t.Errorf("~ в orphans.mute_until не раскрыт: %v", c.Orphans.MuteUntil)
	}
	if c.Tmp.Mode != policy.ModeAlert || c.Tmp.Interval.D() != 2*time.Minute || c.Tmp.Policy.MinEntry != policy.Size(2<<20) {
		t.Errorf("tmp разобран неверно: %+v", c.Tmp)
	}
	if c.Tmp.Policy.MinOwner != policy.Size(5<<20) || c.Tmp.Escalation.After != 2 {
		t.Error("значения tmp по умолчанию потерялись")
	}
	for _, admins := range []map[string]string{c.Orphans.Admins, c.Tmp.Escalation.Admins} {
		if admins["/home/u/proj"] != "proj-admin" || len(admins) != 1 {
			t.Errorf("общие admins не дошли до раздела: %v", admins)
		}
	}
	if c.Orphans.Lead != "boss" || c.Tmp.Lead != "boss" {
		t.Errorf("общий lead не дошёл до разделов: %q %q", c.Orphans.Lead, c.Tmp.Lead)
	}
	if c.Memory.Interval.D() != 30*time.Second || !strings.HasSuffix(c.Memory.Slice, "user@1000.service/agents.slice") {
		t.Errorf("memory разобран неверно: %+v", c.Memory)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"неизвестный режим": "[tmp]\nmode = \"loud\"\n",
		"опечатка в ключе":  "[orphans]\nmin_ded = \"2h\"\n",
		"ключ старого вида": "mode = \"act\"\n",
		"частый memory":     "[memory]\ninterval = \"1s\"\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body), home, 1000); err == nil {
			t.Errorf("%s: ждали ошибку", name)
		}
	}
}

func TestLoadMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "нет.toml"), home, 1000)
	if err != nil || c.Orphans.Mode != orphans.ModeObserve || c.Tmp.Mode != policy.ModeObserve {
		t.Errorf("без файла — observe по умолчанию: %v %v %v", c.Orphans.Mode, c.Tmp.Mode, err)
	}
	if c.Tmp.Escalation.Admins["/home/u/git"] != "git-admin" {
		t.Errorf("админы по умолчанию: %v", c.Tmp.Escalation.Admins)
	}
	if c.Queue.BootGrace.D() != time.Hour {
		t.Errorf("queue.boot_grace по умолчанию %s, хотим 1h", c.Queue.BootGrace.D())
	}
}

func TestSigguardConfig(t *testing.T) {
	c, err := Load(write(t, "[sigguard]\nmode = \"log\"\nwindow = \"7s\"\nstale = \"12m\"\n"), home, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sigguard.Mode != "log" || c.Sigguard.Window.D() != 7*time.Second || c.Sigguard.Stale.D() != 12*time.Minute || c.Sigguard.Lead != c.Lead {
		t.Fatalf("sigguard config: %+v", c.Sigguard)
	}
	defaults, err := Load(filepath.Join(t.TempDir(), "absent"), home, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Sigguard.Mode != sigguard.ModeAlert || defaults.Sigguard.Window.D() != 3*time.Second || defaults.Sigguard.Stale.D() != 10*time.Minute {
		t.Fatalf("sigguard defaults: %+v", defaults.Sigguard)
	}
}

func TestLoadQueueBootGrace(t *testing.T) {
	c, err := Load(write(t, "[queue]\nboot_grace = \"45m\"\n"), home, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.BootGrace.D() != 45*time.Minute {
		t.Errorf("queue.boot_grace = %s, хотим 45m", c.Queue.BootGrace.D())
	}
}

// TestExample — пример из deploy читается без ошибок: он же ставится первым конфигом.
func TestExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "deploy", "config.example.toml"), home, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if c.Restart.Mode != restart.ModeLog {
		t.Errorf("раздел restart примера: %+v", c.Restart)
	}
}

// Ступени ворот — слово пользователя: строка HIGH, MED или LOW; прежний сдвиг числом — ошибка с понятным текстом,
// старые ключи таблицы и поправок — неизвестные ключи (пользователь 10.11: «Только этот механизм должен действовать сейчас»).
func TestLoadQueueLevels(t *testing.T) {
	c, err := Load(write(t, "[queue]\nspaces = {\"~/git\" = \"MED\", \"~/work\" = \"LOW\"}\n"), home, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.Spaces["/home/u/git"] != queue.LevelMed || c.Queue.Spaces["/home/u/work"] != queue.LevelLow || c.Queue.Level != queue.LevelHigh {
		t.Fatalf("ступени: %q %v", c.Queue.Level, c.Queue.Spaces)
	}
	if c, err = Load(write(t, "[queue]\nlevel = \"MED\"\n"), home, 1000); err != nil || c.Queue.Level != queue.LevelMed {
		t.Fatalf("level: %q %v", c.Queue.Level, err)
	}
	for name, body := range map[string]string{
		"число в spaces": "[queue]\nspaces = {\"~/git\" = 1}\n",
		"неизвестная":    "[queue]\nspaces = {\"~/git\" = \"MEDIUM\"}\n",
		"число в level":  "[queue]\nlevel = 2\n",
		"таблица ворот":  "[queue]\ntable = \"~/.config/queue/gate.tsv\"\n",
		"поправки ворот": "[queue]\noverride = \"~/.config/queue/override.tsv\"\n",
	} {
		_, err := Load(write(t, body), home, 1000)
		if err == nil {
			t.Errorf("%s: принято", name)
			continue
		}
		if strings.HasPrefix(name, "число") && (!strings.Contains(err.Error(), "HIGH, MED или LOW") || !strings.Contains(err.Error(), "сдвиг")) {
			t.Errorf("%s: текст не говорит, что нужно: %v", name, err)
		}
	}
}

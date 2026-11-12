package cpuwatch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// systemdName — имя unit, как его пишет systemd (systemd-escape --mangle); нет утилиты — тест пропускается.
func systemdName(t *testing.T, names ...string) []string {
	t.Helper()
	out, err := exec.Command("systemd-escape", append([]string{"--mangle"}, names...)...).Output()
	if err != nil {
		t.Fatalf("systemd-escape: %v", err)
	}
	return strings.Fields(string(out))
}

func writeUnitJob(t *testing.T, dir, id string, created time.Time, master string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"` + id + `","master":"` + master + `","unit":"worker-` + id + `","created":"` + created.Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, id, "job.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestUnescapeUnitSystemd — сверка с systemd-escape и обрезка на каждом байте: всегда начало настоящего имени.
func TestUnescapeUnitSystemd(t *testing.T) {
	ids := []string{"1112-0123-проба-kill-пустышки", "1031-1815-ask-проверка-брифа", "1112-0123-a:b_c.d", `1112-0123-x\y`, "1112-0123-ёЁ№—«»", "1112-0123-日本語-😀"}
	var full []string
	for _, id := range ids {
		full = append(full, "worker-"+id+".service")
	}
	esc := systemdName(t, full...)
	for i, e := range esc {
		if got := UnescapeUnit(e); got != full[i] {
			t.Errorf("%q -> %q, want %q", e, got, full[i])
		}
		for k := 0; k <= len(e); k++ {
			if got := UnescapeUnit(e[:k]); !strings.HasPrefix(full[i], got) {
				t.Errorf("обрезок %d %q -> %q: не начало %q", k, e[:k], got, full[i])
			}
		}
	}
}

// TestByUnitPrefixCutInTail — обрезка внутри «-<n>.service» повтора: задание находится на каждом байте.
func TestByUnitPrefixCutInTail(t *testing.T) {
	dir := t.TempDir()
	id := "1112-0123-проба-"
	for len(systemdName(t, "worker-"+id)[0]) < 120 {
		id += "x"
	}
	writeUnitJob(t, dir, id, time.Now(), "alpha-master")
	full := systemdName(t, "worker-"+id+"-12.service")[0]
	j := &Jobs{Dir: dir}
	for k := 120; k < len(full); k++ {
		if v, err := j.ByUnitPrefix(full[:k]); err != nil || v == nil || v.ID != id {
			t.Errorf("обрезок …%q (%d): %+v %v", full[100:k], k, v, err)
		}
	}
}

// TestByUnitPrefixAmbiguous — у задания и его повтора <id>-2 общее начало: берётся живое; живых нет — nil, не угадываем.
func TestByUnitPrefixAmbiguous(t *testing.T) {
	dir, units := t.TempDir(), t.TempDir()
	base := "1112-0123-проба-номер-задачи"
	now := time.Now()
	writeUnitJob(t, dir, base, now, "first")
	writeUnitJob(t, dir, base+"-2", now.Add(time.Second), "second")
	unit := systemdName(t, "worker-"+base+".service")[0][:60]
	if v, err := (&Jobs{Dir: dir, Units: units}).ByUnitPrefix(unit); err != nil || v != nil {
		t.Errorf("живых нет: %+v %v, want nil", v, err)
	}
	if v, err := (&Jobs{Dir: dir}).ByUnitPrefix(unit); err != nil || v != nil {
		t.Errorf("папки unit нет: %+v %v, want nil", v, err)
	}
	if err := os.MkdirAll(filepath.Join(units, systemdName(t, "worker-"+base+".service")[0]), 0o700); err != nil {
		t.Fatal(err)
	}
	if v, err := (&Jobs{Dir: dir, Units: units}).ByUnitPrefix(unit); err != nil || v == nil || v.Master != "first" {
		t.Errorf("живо первое: %+v %v", v, err)
	}
}

// TestUnitLiveEscaped — unit в cgroup экранирован, в job.json — нет: задание живое.
func TestUnitLiveEscaped(t *testing.T) {
	dir, units := t.TempDir(), t.TempDir()
	id := "1112-0123-проба"
	writeUnitJob(t, dir, id, time.Now(), "alpha-master")
	esc := systemdName(t, "worker-"+id+".service")[0]
	if err := os.MkdirAll(filepath.Join(units, esc), 0o700); err != nil {
		t.Fatal(err)
	}
	j := &Jobs{Dir: dir, Units: units}
	v, err := j.ByUnit(esc)
	if err != nil || v == nil {
		t.Fatalf("ByUnit %q: %+v %v", esc, v, err)
	}
	if live, err := j.UnitLive(v.Unit); err != nil || !live {
		t.Errorf("UnitLive(%q) = %v %v", v.Unit, live, err)
	}
	fresh := &Jobs{Dir: dir, Units: units}
	if err := fresh.readLive(); err != nil || len(fresh.all) != 1 {
		t.Errorf("readLive = %d %v", len(fresh.all), err)
	}
}

// TestByUnitPrefixBrokenNeighbour — битый job.json соседа с тем же началом не мешает найти задание.
func TestByUnitPrefixBrokenNeighbour(t *testing.T) {
	dir := t.TempDir()
	base := "1112-0123-проба-длинное"
	writeUnitJob(t, dir, base, time.Now(), "alpha-master")
	if err := os.MkdirAll(filepath.Join(dir, base+"-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+"-x", "job.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := (&Jobs{Dir: dir}).ByUnitPrefix(systemdName(t, "worker-"+base+".service")[0][:60]); err != nil || v == nil || v.ID != base {
		t.Errorf("got %+v %v", v, err)
	}
}

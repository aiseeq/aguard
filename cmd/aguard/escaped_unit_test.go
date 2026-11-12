package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/cpuwatch"
	"github.com/aiseeq/aguard/internal/sigguard"
	"github.com/aiseeq/akit/session"
)

// TestWorkerByUnitLiveEscaped — живой боец с кириллицей в id: tmp находит задание по экранированному unit из cgroup
// и считает его живым (иначе мастеру уходит совет удалить папку идущего задания, как до fd675f8).
func TestWorkerByUnitLiveEscaped(t *testing.T) {
	dir, units := t.TempDir(), t.TempDir()
	id := "1112-0123-проба"
	if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "job.json"), []byte(`{"id":"`+id+`","master":"alpha-master","unit":"worker-`+id+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("systemd-escape", "--mangle", "worker-"+id+".service").Output()
	if err != nil {
		t.Fatalf("systemd-escape: %v", err)
	}
	esc := strings.TrimSpace(string(out))
	if err := os.MkdirAll(filepath.Join(units, esc), 0o700); err != nil {
		t.Fatal(err)
	}
	l := &liveSessions{jobs: &cpuwatch.Jobs{Dir: dir, Units: units}}
	job, ok, err := l.WorkerByUnit(esc)
	if err != nil || !ok || job.ID != id || !job.Live {
		t.Fatalf("WorkerByUnit(%q) = %+v ok=%v err=%v", esc, job, ok, err)
	}
}

// TestRouteSigguardExactCutSibling — обрезок ровно по концу worker-<id> (127 байт), действует повтор <id>-2: письмо
// по живому повтору, а не по заданию, чей id совпал с обрезком.
func TestRouteSigguardExactCutSibling(t *testing.T) {
	esc := func(name string) string {
		out, err := exec.Command("systemd-escape", "--mangle", name).Output()
		if err != nil {
			t.Fatalf("systemd-escape: %v", err)
		}
		return strings.TrimSuffix(strings.TrimSpace(string(out)), ".service")
	}
	base := "1112-0123-проба-"
	for len(esc("worker-"+base+"x.service")) <= 127 {
		base += "x"
	}
	if n := len(esc("worker-" + base + ".service")); n != 127 {
		t.Fatalf("длина %d", n)
	}
	dir, units := t.TempDir(), t.TempDir()
	now := time.Now()
	for i, id := range []string{base, base + "-2"} {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"id":"` + id + `","master":"m` + strconv.Itoa(i) + `","unit":"worker-` + id + `","created":"` + now.Add(time.Duration(i)*time.Second).Format(time.RFC3339) + `"}`
		if err := os.WriteFile(filepath.Join(dir, id, "job.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// unit запуска — worker-<id>-<n>.service: жив первый запуск задания <id>-2, а не повтор №2 задания <id>
	full := esc("worker-"+base+"-2-1.service") + ".service"
	if err := os.MkdirAll(filepath.Join(units, full), 0o700); err != nil {
		t.Fatal(err)
	}
	alive := map[string]session.Info{"m0": {Pid: 1, Name: "m0"}, "m1": {Pid: 2, Name: "m1"}}
	got, job := sigguardRoute(sigguard.Deny{Unit: full[:127]}, "leader", alive, &cpuwatch.Jobs{Dir: dir, Units: units})
	if got != "m1" || job != base+"-2" {
		t.Fatalf("route = %s %s, want m1 %s-2", got, job, base)
	}
}

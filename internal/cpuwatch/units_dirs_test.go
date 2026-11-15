package cpuwatch

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLiveUnitsBothSlices — worker переносит тела бойцов из app.slice в agents.slice: живой unit в любой из папок
// живой; папки agents.slice нет — app.slice читается; нет ни одной — ошибка.
func TestLiveUnitsBothSlices(t *testing.T) {
	root, jobs := t.TempDir(), t.TempDir()
	app, agents := filepath.Join(root, "app.slice"), filepath.Join(root, "agents.slice")
	for _, p := range []string{filepath.Join(app, "worker-job-1115-0001-old.service"), filepath.Join(agents, "worker-round-1115-0002-new-1.service")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(jobs, "1115-0001-old", "job.json"),
		`{"id":"1115-0001-old","dir":"/w/a/old","task":"old","master":"a-master","unit":"worker-job-1115-0001-old"}`)
	write(t, filepath.Join(jobs, "1115-0002-new", "job.json"),
		`{"id":"1115-0002-new","dir":"/w/b/new","task":"new","master":"b-master","unit":"worker-round-1115-0002-new"}`)
	j := &Jobs{Dir: jobs, Units: []string{app, agents}}
	for _, unit := range []string{"worker-job-1115-0001-old", "worker-round-1115-0002-new"} {
		if live, err := j.UnitLive(unit); err != nil || !live {
			t.Errorf("UnitLive(%q) = %v %v, want live", unit, live, err)
		}
	}
	if err := j.readLive(); err != nil || len(j.all) != 2 {
		t.Errorf("readLive: %d живых, err %v; want 2", len(j.all), err)
	}
	onlyApp := &Jobs{Dir: jobs, Units: []string{app, filepath.Join(root, "missing.slice")}}
	if live, err := onlyApp.UnitLive("worker-job-1115-0001-old"); err != nil || !live {
		t.Errorf("без второй папки: UnitLive = %v %v, want live", live, err)
	}
	onlyAgents := &Jobs{Dir: jobs, Units: []string{filepath.Join(root, "missing.slice"), agents}}
	if live, err := onlyAgents.UnitLive("worker-round-1115-0002-new"); err != nil || !live {
		t.Errorf("без первой папки: UnitLive = %v %v, want live", live, err)
	}
	notDir := filepath.Join(root, "file.slice")
	write(t, notDir, "")
	broken := &Jobs{Dir: jobs, Units: []string{app, notDir}}
	if _, err := broken.UnitLive("worker-job-1115-0001-old"); err == nil {
		t.Error("папка не читается (не ENOENT): want error, не неполный список")
	}
	none := &Jobs{Dir: jobs, Units: []string{filepath.Join(root, "a"), filepath.Join(root, "b")}}
	if _, err := none.UnitLive("worker-job-1115-0001-old"); err == nil {
		t.Error("нет ни одной папки: want error")
	}
}

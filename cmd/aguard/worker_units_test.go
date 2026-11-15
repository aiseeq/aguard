package main

import (
	"strings"
	"testing"

	"github.com/aiseeq/aguard/internal/cpuwatch"
)

// TestWorkerUnitDirsBothSlices — tmp, cpu и io ищут живые unit бойцов в app.slice и agents.slice своего user@.
func TestWorkerUnitDirsBothSlices(t *testing.T) {
	a := &app{uid: 1000}
	dirs := a.workerUnitDirs()
	if len(dirs) != 2 || !strings.HasSuffix(dirs[0], "/user@1000.service/app.slice") || !strings.HasSuffix(dirs[1], "/user@1000.service/agents.slice") {
		t.Fatalf("workerUnitDirs = %v", dirs)
	}
	cfg := cpuwatch.Default("")
	if got := a.cpuResolver(&cfg).Jobs.Units; len(got) != 2 {
		t.Errorf("cpu: Units = %v", got)
	}
	a.workerJobsDir = t.TempDir()
	if got := a.workerJobs().Units; len(got) != 2 {
		t.Errorf("tmp: Units = %v", got)
	}
}

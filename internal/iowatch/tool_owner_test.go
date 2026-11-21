package iowatch

import (
	"testing"
	"time"
)

func TestLeafProcFromMainCgroup(t *testing.T) {
	main, other := "/agents/worker.service", "/agents/other.service"
	leaf := func(podman int64, podmanCgroup string, rootComm string) string {
		c := SumCulprits([]Step{{Processes: []Process{
			{PID: 1, Comm: rootComm, WriteBytes: 1000, Cgroup: main},
			{PID: 2, Comm: "podman", WriteBytes: podman, Cgroup: podmanCgroup},
		}}})
		return c.LeafProc(main, []string{"codex", "claude", "bash", "sh", "node", "worker", "python3", "timeout"})
	}
	if got := leaf(250, main, "codex"); got != "podman(2)" {
		t.Fatalf("порог 1/4: лист %q", got)
	}
	if got := leaf(249, main, "codex"); got != "" {
		t.Fatalf("ниже порога: лист %q", got)
	}
	if got := leaf(500, other, "codex"); got != "" {
		t.Fatalf("лист из другой cgroup: %q", got)
	}
	if got := leaf(500, main, "2.1.328"); got != "podman(2)" {
		t.Fatalf("версия Claude Code не распознана обёрткой: %q", got)
	}
}

func TestLeafProcEmptyWhenMainIsNotWrapper(t *testing.T) {
	main := "/agents/worker.service"
	c := SumCulprits([]Step{{Processes: []Process{
		{PID: 1, Comm: "go", WriteBytes: 1000, Cgroup: main},
		{PID: 2, Comm: "podman", WriteBytes: 500, Cgroup: main},
	}}})
	if got := c.LeafProc(main, Default().Wrappers); got != "" {
		t.Fatalf("лист выбран при главном процессе go: %q", got)
	}
}

func TestLeafProcSumsInstancesByProcessName(t *testing.T) {
	main := "/agents/worker.service"
	wrappers := Default().Wrappers
	makeCulprits := func(processes ...Process) *Culprits {
		return new(SumCulprits([]Step{{Processes: processes}}))
	}
	if got := makeCulprits(
		Process{PID: 1, Comm: "codex", WriteBytes: 1000, Cgroup: main},
		Process{PID: 2, Comm: "podman", WriteBytes: 100, Cgroup: main},
		Process{PID: 3, Comm: "podman", WriteBytes: 100, Cgroup: main},
		Process{PID: 4, Comm: "podman", WriteBytes: 100, Cgroup: main},
	).LeafProc(main, wrappers); got != "podman(2)" {
		t.Fatalf("три экземпляра должны сложиться в лист, got %q", got)
	}
	if got := makeCulprits(
		Process{PID: 1, Comm: "codex", WriteBytes: 1200, Cgroup: main},
		Process{PID: 101, Comm: "podman", WriteBytes: 50, Cgroup: main},
		Process{PID: 102, Comm: "podman", WriteBytes: 100, Cgroup: main},
		Process{PID: 103, Comm: "podman", WriteBytes: 150, Cgroup: main},
	).LeafProc(main, wrappers); got != "podman(103)" {
		t.Fatalf("должен вернуться крупнейший экземпляр podman (150): %q", got)
	}
	if got := makeCulprits(
		Process{PID: 1, Comm: "codex", WriteBytes: 1000, Cgroup: main},
		Process{PID: 2, Comm: "podman", WriteBytes: 100, Cgroup: main},
	).LeafProc(main, wrappers); got != "" {
		t.Fatalf("одного экземпляра на 10%% недостаточно: %q", got)
	}
	if got := makeCulprits(
		Process{PID: 1, Comm: "codex", WriteBytes: 1200, Cgroup: main},
		Process{PID: 2, Comm: "podman", WriteBytes: 110, Cgroup: main},
		Process{PID: 3, Comm: "podman", WriteBytes: 110, Cgroup: main},
		Process{PID: 4, Comm: "podman", WriteBytes: 110, Cgroup: main},
		Process{PID: 5, Comm: "other", WriteBytes: 250, Cgroup: main},
	).LeafProc(main, wrappers); got != "podman(2)" {
		t.Fatalf("победитель по сумме должен быть podman, хотя крупнейший экземпляр — other: %q", got)
	}
	if got := makeCulprits(
		Process{PID: 1, Comm: "codex", WriteBytes: 1200, Cgroup: main},
		Process{PID: 11, Comm: "alpha", WriteBytes: 100, Cgroup: main},
		Process{PID: 12, Comm: "alpha", WriteBytes: 200, Cgroup: main},
		Process{PID: 21, Comm: "beta", WriteBytes: 150, Cgroup: main},
		Process{PID: 22, Comm: "beta", WriteBytes: 150, Cgroup: main},
	).LeafProc(main, wrappers); got != "alpha(12)" {
		t.Fatalf("при равной сумме имён должен победить alpha: %q", got)
	}
}

func TestRecorderFinishStoresDefaultLeafProcess(t *testing.T) {
	cfg := Default()
	r := NewRecorder(cfg)
	main := "/agents/worker.service"
	at := time.Now()
	r.Detail(Step{At: at, Processes: []Process{
		{PID: 1, Start: 1, Comm: "codex", WriteBytes: 1000, Cgroup: main},
		{PID: 2, Start: 2, Comm: "conmon", WriteBytes: 800, Cgroup: main},
		{PID: 3, Start: 3, Comm: "podman", WriteBytes: 250, Cgroup: main},
	}})
	c := Case{Start: at, End: at.Add(time.Second), DurMS: 1000}
	r.finish(&c)
	if c.Culprits == nil || c.Culprits.MainProc != "codex(1)" || c.Culprits.MainLeafProc != "podman(3)" {
		t.Fatalf("finish не сохранил лист главной cgroup: %+v", c.Culprits)
	}
}

func TestClaudeVersionProcBaseIsClaude(t *testing.T) {
	if got := ProcBase("2.1.328(123)"); got != "claude" {
		t.Fatalf("ProcBase версии Claude Code = %q, want claude", got)
	}
}

func TestConfigRejectsEmptyToolOwner(t *testing.T) {
	cfg := Default()
	cfg.Socket = "/tmp/io.sock"
	cfg.Tools = map[string]string{"tl": ""}
	if err := cfg.Validate(); err == nil {
		t.Fatal("пустой хозяин инструмента прошёл Validate")
	}
}

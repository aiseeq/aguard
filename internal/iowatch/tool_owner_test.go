package iowatch

import "testing"

func TestLeafProcFromMainCgroup(t *testing.T) {
	main, other := "/agents/worker.service", "/agents/other.service"
	leaf := func(podman int64, podmanCgroup string, rootComm string) string {
		c := SumCulprits([]Step{{Processes: []Process{
			{PID: 1, Comm: rootComm, WriteBytes: 1000, Cgroup: main},
			{PID: 2, Comm: "podman", WriteBytes: podman, Cgroup: podmanCgroup},
		}}})
		return c.LeafProc(main, []string{"codex", "claude", "bash", "sh", "node", "worker", "python3", "timeout"})
	}
	if got := leaf(500, main, "codex"); got != "podman(2)" {
		t.Fatalf("порог 1/4: лист %q", got)
	}
	if got := leaf(200, main, "codex"); got != "" {
		t.Fatalf("ниже порога: лист %q", got)
	}
	if got := leaf(500, other, "codex"); got != "" {
		t.Fatalf("лист из другой cgroup: %q", got)
	}
	if got := leaf(500, main, "2.1.328"); got != "podman(2)" {
		t.Fatalf("версия Claude Code не распознана обёрткой: %q", got)
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

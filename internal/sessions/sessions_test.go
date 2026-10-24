package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseStat(t *testing.T) {
	// comm со скобками и пробелом
	s := "123 (my (odd) proc) S 45 123 123 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 26733327 1000 10\n"
	st, err := ParseStat(s)
	if err != nil || st.Comm != "my (odd) proc" || st.Ppid != 45 || st.Start != "26733327" {
		t.Errorf("ParseStat: %+v %v", st, err)
	}
	if _, err := ParseStat("garbage"); err == nil {
		t.Error("мусор разобран")
	}
}

func writeProc(t *testing.T, root string, pid, start string) {
	t.Helper()
	d := filepath.Join(root, pid)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := pid + " (claude) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 " + start + " 0 0\n"
	if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryAlive(t *testing.T) {
	root := t.TempDir()
	reg := Registry{Dir: filepath.Join(root, "sessions"), ProcRoot: filepath.Join(root, "proc")}
	if err := os.MkdirAll(reg.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"10.json":     `{"pid":10,"procStart":"500","name":"lead","startedAt":1,"messagingSocketPath":"/s/10.sock"}`,
		"11.json":     `{"pid":11,"procStart":"600","name":"lead","startedAt":2,"messagingSocketPath":"/s/11.sock"}`, // pid переиспользован
		"12.json":     `{"pid":12,"procStart":"700","name":"work-admin","startedAt":3}`,
		"13.json":     `{"pid":13,"procStart":"800","name":"dead"}`,
		"13.abcd.key": `{"peerToken":"tok13"}`,
		"x.json":      `{}`,
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(reg.Dir, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProc(t, reg.ProcRoot, "10", "500")
	writeProc(t, reg.ProcRoot, "11", "999")
	writeProc(t, reg.ProcRoot, "12", "700")
	alive, err := reg.Alive()
	if err != nil {
		t.Fatal(err)
	}
	if alive["lead"].Pid != 10 || alive["work-admin"].Pid != 12 || len(alive) != 2 {
		t.Errorf("живые: %+v", alive)
	}
	if reg.IsAlive(13, "800") {
		t.Error("мёртвая сессия жива")
	}
	if tok, err := reg.Token(13); err != nil || tok != "tok13" {
		t.Errorf("токен 13: %q %v", tok, err)
	}
	if tok, err := reg.Token(10); err != nil || tok != "" {
		t.Errorf("нет файла — пусто без ошибки: %q %v", tok, err)
	}
	if in, err := reg.Get(99); in != nil || err != nil {
		t.Errorf("нет записи — nil без ошибки: %v %v", in, err)
	}
}

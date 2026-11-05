package statefile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Мера ревью тикета small-writers: восстановление после сбоя, точность чисел, цена прохода.

// Сбой между заменой снимка и усечением журнала: запись, которую журнал правил и потом удалил, в новом снимке
// уже отсутствует. Повтор журнала не должен падать навсегда, итог — состояние снимка.
func TestMergeOfRemovedKeyAfterInterruptedCompactionAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"files":{"a":{"offset":1},"b":{"offset":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"files":{"a":{"offset":2},"b":{"offset":1}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"files":{"b":{"offset":1}}}`)); err != nil {
		t.Fatal(err)
	}
	// Снимок заменён, журнал не усечён.
	if err := os.WriteFile(path, []byte(`{"files":{"b":{"offset":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("повтор журнала над новым снимком: %v", err)
	}
	if !jsonEqual(got, []byte(`{"files":{"b":{"offset":1}}}`)) {
		t.Fatalf("итог %s", got)
	}
}

// Папки ещё нет (ручной tick до первого запуска демона): чтение даёт пустое состояние, как раньше без файла.
func TestLoadWithoutDirAccept(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "нет", "state.json"))
	if err != nil || string(got) != "{}" {
		t.Fatalf("чтение без папки: %s, %v", got, err)
	}
}

// Уплотнение работает и сохраняет итог.
func TestCompactionKeepsStateAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"v":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	final := []byte(`{"v":"` + strings.Repeat("x", compactAt+1) + `"}`)
	if err := Save(path, final); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + ".journal"); err != nil || info.Size() != 0 {
		t.Fatalf("журнал после уплотнения: %v, %v", info, err)
	}
	got, err := Load(path)
	if err != nil || string(got) != string(final) {
		t.Fatalf("итог после уплотнения: %d байт, %v", len(got), err)
	}
}

// Снимок убрали руками ради сброса состояния, журнал остался: старый журнал не применяется к новому состоянию.
func TestJournalWithoutSnapshotAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"values":{"a":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"values":{"a":2}}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"values":{"a":7}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, []byte(`{"values":{"a":7}}`)) {
		t.Fatalf("старый журнал применился: %s", got)
	}
}

// Большие целые (время в наносекундах) не проходят через float64 ни на чтении, ни при повторе журнала.
func TestBigIntegersExactAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const big, other = "1793784452761234567", "1793784452761234999"
	if err := os.WriteFile(path, []byte(`{"seen":{"a":`+big+`,"b":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []byte(`{"seen":{"a":`+big+`,"b":`+other+`}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), big) || !strings.Contains(string(got), other) {
		t.Fatalf("большие целые испорчены: %s", got)
	}
	// Отличие только в младших разрядах большого целого — изменение, а не равенство.
	next := `{"seen":{"a":` + big + `,"b":1793784452761234998}}`
	if err := Save(path, []byte(next)); err != nil {
		t.Fatal(err)
	}
	if got, err = Load(path); err != nil || !strings.Contains(string(got), "1793784452761234998") {
		t.Fatalf("изменение в младших разрядах потеряно: %s, %v", got, err)
	}
}

// Цена прохода: чтение и сохранение большого состояния с журналом не разбирают все записи целиком. Счёт — выделенные
// байты, а не время: от нагрузки машины не зависят.
func TestCycleAllocationAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	type file struct {
		Offset int64  `json:"offset"`
		From   int64  `json:"from"`
		Name   string `json:"name,omitempty"`
	}
	type state struct {
		Started bool             `json:"started"`
		Files   map[string]*file `json:"files"`
	}
	st := state{Started: true, Files: map[string]*file{}}
	keys := make([]string, 0, 5000)
	for i := range 5000 {
		k := fmt.Sprintf("-home-aisee-git-project-%04d/01e2b979-a4d0-4a8b-9f6e-%012d.jsonl", i%40, i)
		st.Files[k] = &file{Offset: int64(i) * 100, Name: "master"}
		keys = append(keys, k)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	const cycles = 30
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for c := range cycles {
		if _, err := Load(path); err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			st.Files[keys[(c*7+i)%len(keys)]].Offset += 1000
		}
		next, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if err := Save(path, next); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	perCycle := (after.TotalAlloc - before.TotalAlloc) / cycles
	// Состояние ~0,5 МБ. Сырые карты RawMessage на уровне files укладываются; разбор каждой записи в any — нет.
	if limit := uint64(16 * len(b)); perCycle > limit {
		t.Fatalf("выделено на цикл %d байт, предел %d (состояние %d)", perCycle, limit, len(b))
	}
}

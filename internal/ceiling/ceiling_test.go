package ceiling

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestTargetAndLowering(t *testing.T) {
	if got := Target(100<<20, 1); got != 128<<20 {
		t.Fatalf("Target = %d", got)
	}
	if got := Target(128<<20, 0); got != 128<<20 {
		t.Fatalf("Target exact = %d", got)
	}
	if !MayLower(500, 400, 10, 120) || MayLower(500, 300, 10, 120) {
		t.Fatal("lowering safety predicate")
	}
}

func TestUpdateSerializesIndependentFields(t *testing.T) {
	path := StatePath(t.TempDir(), "agent-test-1.scope")
	if err := Write(path, State{Unit: "agent-test-1.scope", Base: 1, OOM: 1}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := int64(1); n <= 100; n++ {
			_, err := Update(path, func(state *State) error { state.Base = n; return nil })
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for n := int64(1); n <= 100; n++ {
			_, err := Update(path, func(state *State) error { state.OOM = n; return nil })
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	state, err := Read(path)
	if err != nil || state.Base != 100 || state.OOM != 100 {
		t.Fatalf("lost update: %+v err=%v", state, err)
	}
}

func TestUpdateMissingStateRefusesEmptyState(t *testing.T) {
	path := StatePath(t.TempDir(), "agent-missing-1.scope")
	if _, err := Update(path, func(state *State) error { state.Base = 1; return nil }); err == nil {
		t.Fatal("Update created an ownerless state")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected state file: %v", err)
	}
}

func TestSharedSizeStateAndMemoryMax(t *testing.T) {
	if got, err := ParseSize("34%", 1000); err != nil || got != 340 {
		t.Fatalf("percentage %d %v", got, err)
	}
	if _, err := ParseSize("bad", 1000); err == nil {
		t.Fatal("accepted invalid size")
	}
	home := t.TempDir()
	state := State{Unit: "agent-x-1.scope", Base: 10, Swap: 20, By: "cl"}
	path := StatePath(home, state.Unit)
	if err := Write(path, state); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil || !reflect.DeepEqual(got, state) {
		t.Fatalf("state %+v %v", got, err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := CurrentMax(dir); err != nil || n != Unlimited {
		t.Fatalf("unlimited max %d %v", n, err)
	}
	if err = os.WriteFile(filepath.Join(dir, "memory.max"), []byte("oops\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CurrentMax(dir); err == nil {
		t.Fatal("invalid memory.max accepted")
	}
}

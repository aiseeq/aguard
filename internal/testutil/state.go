// Package testutil содержит общие вспомогательные функции тестов.
package testutil

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Wchar возвращает число байт, записанных процессом по данным Linux /proc.
func Wchar(t testing.TB) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "wchar: "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("нет wchar")
	return 0
}

// WriteBudget выполняет измеряемые проходы и проверяет средний объём записи.
func WriteBudget(t testing.TB, cycles int, budget int64, run func(int) error) {
	t.Helper()
	start := Wchar(t)
	for cycle := range cycles {
		if err := run(cycle); err != nil {
			t.Fatal(err)
		}
	}
	spent := Wchar(t) - start
	if perCycle := spent / int64(cycles); perCycle > budget {
		t.Fatalf("запись на цикл %d байт, бюджет %d", perCycle, budget)
	}
}

// StateBudget измеряет проходы, которые загружают, проверяют, меняют и сохраняют состояние.
func StateBudget[T any](t testing.TB, cycles int, budget int64, path string, want func() int,
	load func(string) (*T, error), started func(*T) bool, count func(*T) int,
	update func(*T, int) error,
) {
	WriteBudget(t, cycles, budget, func(cycle int) error {
		state, err := load(path)
		if err != nil {
			return err
		}
		if !started(state) || count(state) != want() {
			return fmt.Errorf("цикл %d: started %v, записей %d, ждали %d", cycle, started(state), count(state), want())
		}
		return update(state, cycle)
	})
}

// ThreeKeys выбирает три циклически сменяемых ключа для тестов смещений.
func ThreeKeys(cycle int, keys []string, update func(string)) {
	for i := range 3 {
		update(keys[(cycle*7+i)%len(keys)])
	}
}

// CheckCount загружает состояние и проверяет ожидаемое число записей.
func CheckCount[T any](t testing.TB, path string, load func(string) (*T, error), count func(*T) int, want int) *T {
	t.Helper()
	state, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := count(state); got != want {
		t.Fatalf("записей %d, ждали %d", got, want)
	}
	return state
}

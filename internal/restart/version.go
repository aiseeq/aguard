package restart

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// parseVersion — «2.1.310» в числа; не версия — ошибка.
func parseVersion(s string) ([]int, error) {
	parts := strings.Split(s, ".")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("не версия Claude Code: %q", s)
		}
		nums[i] = n
	}
	return nums, nil
}

// older — версия a старше (меньше) b. Нечитаемая версия — ошибка: сравнивать нечего.
func older(a, b string) (bool, error) {
	va, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	for i := 0; i < max(len(va), len(vb)); i++ {
		x, y := at(va, i), at(vb, i)
		if x != y {
			return x < y, nil
		}
	}
	return false, nil
}

func at(v []int, i int) int {
	if i < len(v) {
		return v[i]
	}
	return 0
}

// installedVersion — версия, которую запустит claude из PATH: ~/.local/bin/claude — ссылка на
// ~/.local/share/claude/versions/<версия>, автообновление переставляет её на новую.
func installedVersion(claude string) (string, error) {
	target, err := filepath.EvalSymlinks(claude)
	if err != nil {
		return "", fmt.Errorf("установленная версия: %w", err)
	}
	v := filepath.Base(target)
	if _, err := parseVersion(v); err != nil {
		return "", err
	}
	return v, nil
}

// runningVersion — версия бинаря живого процесса по /proc/<pid>/exe; хвост в скобках, который ядро дописывает к
// ссылке на файл, исчезнувший с диска после обновления, отрезается.
func runningVersion(proc string, pid int) (string, error) {
	exe, err := os.Readlink(filepath.Join(proc, strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", fmt.Errorf("версия процесса %d: %w", pid, err)
	}
	v := strings.TrimSuffix(filepath.Base(exe), " (deleted)")
	if _, err := parseVersion(v); err != nil {
		return "", err
	}
	return v, nil
}

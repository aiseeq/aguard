// Package names — имя сессии по папке запуска: таблица ~/.config/cl/names.tsv (строка «папка<TAB>имя», ~ раскрывается),
// иначе <папка>-master для папок прямо в ~/git и ~/work. По ней cl называет сессию, а aguard disk — хозяина папки
// проекта, когда его сессия не запущена.
package names

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File — таблица имён относительно домашней папки.
var File = filepath.Join(".config", "cl", "names.tsv")

// ReadTable читает «папка<TAB>имя»; пустые строки и # — пропуск. Нет файла — пустая таблица.
func ReadTable(path, home string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("таблица имён: %w", err)
	}
	defer f.Close()
	table := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		dir, name, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s:%d: нужно «папка<TAB>имя»", path, n)
		}
		table[Expand(strings.TrimSpace(dir), home)] = strings.TrimSpace(name)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("таблица имён: %w", err)
	}
	return table, nil
}

// Expand раскрывает ~ и чистит путь.
func Expand(dir, home string) string {
	if dir == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return filepath.Clean(dir)
}

// For — имя из таблицы, иначе <проект>-master для ~/git/<проект> и ~/work/<проект>; иначе пусто.
func For(cwd, home string, table map[string]string) string {
	cwd = filepath.Clean(cwd)
	if name, ok := table[cwd]; ok {
		return name
	}
	parent := filepath.Dir(cwd)
	if parent == filepath.Join(home, "git") || parent == filepath.Join(home, "work") {
		return strings.ToLower(filepath.Base(cwd)) + "-master"
	}
	return ""
}

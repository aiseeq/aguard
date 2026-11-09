package queue

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

const frozenFields = 4 // путь, кто, дата, почему

// ReadFrozen читает список проектов, где работа допускается только при живом мастере.
// Строка разбирается как в tk: плохая касается только себя, а неполная держит проект замороженным.
// Ошибка — только когда сам файл не прочитан; жалобы на строки — во втором значении.
func ReadFrozen(path, home string) (map[string]bool, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("список замороженных %s: %w", path, err)
	}
	defer f.Close()
	result := map[string]bool{}
	var problems []string
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		dir, problem := frozenLine(text, home)
		if problem != "" {
			problems = append(problems, fmt.Sprintf("%s:%d: %s", path, line, problem))
		}
		if dir != "" {
			result[dir] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("список замороженных %s: %w", path, err)
	}
	return result, problems, nil
}

// frozenLine возвращает папку проекта ("" — строка никого не называет) и жалобу на строку.
func frozenLine(text, home string) (string, string) {
	fields := strings.Split(text, "\t")
	listed, problem := strings.TrimSpace(fields[0]), ""
	if len(fields) < frozenFields {
		listed = strings.Fields(text)[0]
		problem = "неполная строка, проект остаётся замороженным"
	} else if _, err := time.Parse("2006-01-02", strings.TrimSpace(fields[2])); err != nil {
		problem = fmt.Sprintf("дата не в виде ГГГГ-ММ-ДД (%v), проект остаётся замороженным", err)
	} else if strings.TrimSpace(fields[1]) == "" || strings.TrimSpace(fields[3]) == "" {
		problem = "пустое поле, проект остаётся замороженным"
	}
	dir := filepath.Clean(policy.ExpandHome(listed, home))
	if !filepath.IsAbs(dir) {
		return "", "нужен полный путь или ~/, строка не учтена"
	}
	return dir, problem
}

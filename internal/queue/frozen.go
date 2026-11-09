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

// ReadFrozen читает список проектов, где работа допускается только при живом мастере.
func ReadFrozen(path, home string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("список замороженных %s: %w", path, err)
	}
	defer f.Close()
	result := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("список замороженных %s:%d: нужно четыре поля", path, line)
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
			if fields[i] == "" {
				return nil, fmt.Errorf("список замороженных %s:%d: пустое поле", path, line)
			}
		}
		dir := filepath.Clean(policy.ExpandHome(fields[0], home))
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("список замороженных %s:%d: нужен полный путь", path, line)
		}
		if _, err := time.Parse("2006-01-02", fields[2]); err != nil {
			return nil, fmt.Errorf("список замороженных %s:%d: дата: %w", path, line, err)
		}
		result[dir] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("список замороженных %s: %w", path, err)
	}
	return result, nil
}

// Package cgroupstats разбирает общие форматы счётчиков cgroup v2.
package cgroupstats

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
)

// EachKeyed вызывает fn для каждой строки «ключ число» (memory.stat, memory.events) без копий:
// key живёт до возврата fn. Строка без пробела пропускается, нечисловое значение — ошибка.
func EachKeyed(data []byte, fn func(key []byte, value int64)) error {
	for len(data) > 0 {
		line := data
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			line, data = data[:end], data[end+1:]
		} else {
			data = nil
		}
		key, value, ok := bytes.Cut(line, []byte{' '})
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil {
			return fmt.Errorf("cgroup: %q: %w", line, err)
		}
		fn(key, n)
	}
	return nil
}

// ParseKeyed читает «ключ число» в переиспользуемую карту.
func ParseKeyed(r io.Reader, dst map[string]int64) (map[string]int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("cgroup: %w", err)
	}
	if dst == nil {
		dst = make(map[string]int64)
	}
	clear(dst)
	if err := EachKeyed(data, func(key []byte, value int64) { dst[string(key)] = value }); err != nil {
		return nil, err
	}
	return dst, nil
}

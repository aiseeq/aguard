package meminfo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Parse returns selected /proc/meminfo fields in bytes; missing keys and malformed values are errors.
func Parse(data []byte, keys ...string) (map[string]int64, error) {
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key] = true
	}
	values := make(map[string]int64, len(keys))
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !wanted[fields[0]] {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s: invalid value %q", fields[0], fields[1])
		}
		if len(fields) > 2 && fields[2] == "kB" {
			n *= 1024
		}
		values[fields[0]] = n
	}
	missing := 0
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			missing++
		}
	}
	if missing > 0 {
		return nil, fmt.Errorf("meminfo: из %d полей найдено %d", len(keys), len(keys)-missing)
	}
	return values, nil
}

func Read(keys ...string) (map[string]int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return Parse(data, keys...)
}

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// journalRecord is the common subset used by sudo attribution and kernel OOM parsing.
type journalRecord struct {
	Message   string `json:"MESSAGE"`
	Timestamp string `json:"__REALTIME_TIMESTAMP"`
	PID       string `json:"_PID"`
	Session   string `json:"_SYSTEMD_SESSION"`
	CGroup    string `json:"_SYSTEMD_CGROUP"`
	BootID    string `json:"_BOOT_ID"`
}

func parseJournalJSON(raw string) ([]journalRecord, error) {
	var records []journalRecord
	s := bufio.NewScanner(bytes.NewBufferString(raw))
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "-- cursor: ") {
			continue
		}
		var row journalRecord
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("journal JSON: %w", err)
		}
		records = append(records, row)
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("scan journal JSON: %w", err)
	}
	return records, nil
}

func journalTimestamp(value string) (time.Time, error) {
	micros, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("journal timestamp %q: %w", value, err)
	}
	return time.UnixMicro(micros), nil
}

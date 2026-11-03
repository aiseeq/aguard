package iowatch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Виды строк снимка.
const (
	KindStep   = "s" // быстрый шаг машины
	KindDetail = "d" // детальная запись: cgroup и процессы
)

// Step — быстрый шаг машины (дельты к прошлому шагу) или детальная запись (Dirty, Writeback,
// MemAvail, SwapUsed, Processes, Cgroups). Нулевые поля в снимок не пишутся.
type Step struct {
	At         time.Time `json:"at"`
	IOFullUS   int64     `json:"io_full_us,omitempty"`
	IOSomeUS   int64     `json:"io_some_us,omitempty"`
	MemFullUS  int64     `json:"mem_full_us,omitempty"`
	MemSomeUS  int64     `json:"mem_some_us,omitempty"`
	CPUSomeUS  int64     `json:"cpu_some_us,omitempty"` // PSI some cpu: время, когда кто-то ждал процессор
	Runnable   int64     `json:"runnable,omitempty"`    // готовые к работе в момент шага (loadavg)
	SwapIn     int64     `json:"swapin,omitempty"`
	SwapOut    int64     `json:"swapout,omitempty"`
	Majflt     int64     `json:"majflt,omitempty"`
	CleanerMS  int64     `json:"cleaner_ms,omitempty"` // CPU btrfs-cleaner за детальный интервал
	ReadBytes  int64     `json:"rd_b,omitempty"`
	WriteBytes int64     `json:"wr_b,omitempty"`
	IOTicks    int64     `json:"io_ticks,omitempty"` // мс занятости устройства
	RIOS       int64     `json:"r_ios,omitempty"`
	RMS        int64     `json:"r_ms,omitempty"`
	WIOS       int64     `json:"w_ios,omitempty"`
	WMS        int64     `json:"w_ms,omitempty"`
	InFlight   int64     `json:"inflight,omitempty"` // очередь устройства в момент шага
	WTimeMS    int64     `json:"wtime_ms,omitempty"` // взвешенное время очереди
	DIOS       int64     `json:"d_ios,omitempty"`
	DMS        int64     `json:"d_ms,omitempty"`
	FIOS       int64     `json:"f_ios,omitempty"`
	FMS        int64     `json:"f_ms,omitempty"`
	Dirty      int64     `json:"dirty,omitempty"`
	Writeback  int64     `json:"writeback,omitempty"`
	MemAvail   int64     `json:"mem_avail,omitempty"` // MemAvailable, байт
	SwapUsed   int64     `json:"swap_used,omitempty"` // SwapTotal − SwapFree, байт
	Processes  []Process `json:"processes,omitempty"`
	Cgroups    []Cgroup  `json:"cgroups,omitempty"`
}

// ProcessDetails — команда и рабочая папка процесса из /proc.
type ProcessDetails struct {
	Cmdline string `json:"cmdline,omitempty"`
	CWD     string `json:"cwd,omitempty"`
}

// Process — дельты процесса за детальный интервал.
type Process struct {
	PID        int    `json:"pid"`
	PPID       int    `json:"ppid,omitempty"`
	Comm       string `json:"comm"`
	Start      uint64 `json:"start"` // starttime в тиках с загрузки: отличает переиспользованный pid
	ReadBytes  int64  `json:"rd_b,omitempty"`
	WriteBytes int64  `json:"wr_b,omitempty"`
	Majflt     int64  `json:"majflt,omitempty"`
	Cgroup     string `json:"cgroup,omitempty"`
	Cmdline    string `json:"cmdline,omitempty"`
	CWD        string `json:"cwd,omitempty"`
	// Children — за интервал у процесса ушли дети: ядро при wait() добавляет их I/O родителю, уже
	// учтённое у детей вычтено, остаток может быть их.
	Children bool `json:"kids,omitempty"`
}

// Cgroup — дельты листа cgroup v2 за детальный интервал; AnonB — моментальное.
type Cgroup struct {
	Path       string `json:"path"`
	ReadBytes  int64  `json:"rd_b,omitempty"`
	WriteBytes int64  `json:"wr_b,omitempty"`
	SwapIn     int64  `json:"swpin,omitempty"`
	SwapOut    int64  `json:"swpout,omitempty"`
	Majflt     int64  `json:"majflt,omitempty"`
	ReadOps    int64  `json:"rios,omitempty"`
	WriteOps   int64  `json:"wios,omitempty"`
	IOFullUS   int64  `json:"io_full_us,omitempty"`
	MemFullUS  int64  `json:"mem_full_us,omitempty"`
	CPUUS      int64  `json:"cpu_us,omitempty"` // usage_usec из cpu.stat
	AnonB      int64  `json:"anon,omitempty"`   // anon из memory.stat на момент прохода, байт
}

// Record — строка снимка: {"k":"s"|"d", поля Step}.
type Record struct {
	Kind string `json:"k"`
	Step
}

// WriteRecord пишет запись строкой JSONL.
func WriteRecord(w io.Writer, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("сериализовать запись %s: %w", r.Kind, err)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ParseSnapshot читает строки снимка. Битая строка — warn и пропуск; оборванная последняя
// строка (файл ещё пишется) пропускается молча.
func ParseSnapshot(r io.Reader, add func(Record), warn func(error)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var long []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if long != nil {
			line = append(long, chunk...)
			long = nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("прочитать снимок: %w", err)
		}
		var rec Record
		e := json.Unmarshal(line, &rec)
		if e == nil && (rec.At.IsZero() || rec.Kind != KindStep && rec.Kind != KindDetail) {
			e = errors.New("нет времени или вида записи")
		}
		if e != nil {
			if warn != nil {
				warn(fmt.Errorf("строка %.60q: %w", line, e))
			}
			continue
		}
		add(rec)
	}
}

// nameLayout — время в имени снимка: начало события или момент ручного сброса.
const nameLayout = "2006-01-02T150405.000"

const manualPrefix = "manual-"

// ManualName — имя ручного снимка по моменту сброса.
func ManualName(at time.Time) string { return manualPrefix + at.Format(nameLayout) + ".jsonl" }

// IsManual — файл ручного снимка.
func IsManual(name string) bool { return strings.HasPrefix(name, manualPrefix) }

// nameTime — время из имени снимка (события или ручного).
func nameTime(name string) (time.Time, bool) {
	base, ok := strings.CutSuffix(strings.TrimPrefix(name, manualPrefix), ".jsonl")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(nameLayout, base, time.Local)
	return t, err == nil
}

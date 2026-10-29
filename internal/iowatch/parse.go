package iowatch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Разборщики файлов /proc и cgroup — без копий строк: замер идёт 10 раз в секунду.

// nextLine отрезает первую строку data.
func nextLine(data []byte) (line, rest []byte) {
	if before, after, ok := bytes.Cut(data, []byte{'\n'}); ok {
		return before, after
	}
	return data, nil
}

// nextField отрезает первое поле строки, разделители — пробелы и табуляции.
func nextField(line []byte) (field, rest []byte) {
	start := 0
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	end := start
	for end < len(line) && line[end] != ' ' && line[end] != '\t' {
		end++
	}
	return line[start:end], line[end:]
}

func parseIntBytes(value []byte) (int64, error) {
	n, err := parseUintBytes(value)
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt64 {
		return 0, fmt.Errorf("число %q не помещается в int64", value)
	}
	return int64(n), nil
}

func parseUintBytes(value []byte) (uint64, error) {
	if len(value) == 0 || len(value) > 20 {
		return 0, fmt.Errorf("неверное число %q", value)
	}
	var n uint64
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("неверное число %q", value)
		}
		next := n*10 + uint64(digit-'0')
		if next < n {
			return 0, fmt.Errorf("число %q не помещается в uint64", value)
		}
		n = next
	}
	return n, nil
}

// delta — прирост счётчика; сброшенный счётчик (новое меньше старого) даёт 0.
func delta(cur, prev int64) int64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// parsePressureTotals — total строк some и full файла PSI (/proc/pressure/*, cgroup *.pressure), мкс.
// Строки full может не быть (/proc/pressure/cpu до 5.13) — тогда full 0; без total в some — ошибка.
func parsePressureTotals(data []byte) (some, full int64, err error) {
	seenSome := false
	for len(data) > 0 {
		var line []byte
		line, data = nextLine(data)
		kind, rest := nextField(line)
		isSome, isFull := bytes.Equal(kind, []byte("some")), bytes.Equal(kind, []byte("full"))
		if !isSome && !isFull {
			continue
		}
		for len(rest) > 0 {
			var field []byte
			field, rest = nextField(rest)
			value, ok := bytes.CutPrefix(field, []byte("total="))
			if !ok {
				continue
			}
			n, err := parseIntBytes(value)
			if err != nil {
				return 0, 0, fmt.Errorf("%s total: %w", kind, err)
			}
			if isSome {
				some, seenSome = n, true
			} else {
				full = n
			}
		}
	}
	if !seenSome {
		return 0, 0, errors.New("нет строки some с total")
	}
	return some, full, nil
}

// parseLoadavgRunnable — готовые к работе сущности планировщика из /proc/loadavg: четвёртое поле
// «выполняются/всего» до косой черты (`8/5201` → 8). Читающий поток в числе: ядро считает и его.
func parseLoadavgRunnable(data []byte) (int64, error) {
	var field []byte
	rest := data
	for range 4 {
		field, rest = nextField(rest)
	}
	running, _, ok := bytes.Cut(field, []byte{'/'})
	if !ok {
		return 0, fmt.Errorf("loadavg: нет поля выполняются/всего в %.60q", data)
	}
	n, err := parseIntBytes(running)
	if err != nil {
		return 0, fmt.Errorf("loadavg выполняются: %w", err)
	}
	return n, nil
}

// parseVMStat — pswpin, pswpout, pgmajfault из /proc/vmstat.
func parseVMStat(data []byte) (in, out, maj int64, err error) {
	for len(data) > 0 {
		var line []byte
		line, data = nextLine(data)
		key, rest := nextField(line)
		var dst *int64
		switch {
		case bytes.Equal(key, []byte("pswpin")):
			dst = &in
		case bytes.Equal(key, []byte("pswpout")):
			dst = &out
		case bytes.Equal(key, []byte("pgmajfault")):
			dst = &maj
		default:
			continue
		}
		value, _ := nextField(rest)
		if *dst, err = parseIntBytes(value); err != nil {
			return 0, 0, 0, fmt.Errorf("vmstat %s: %w", key, err)
		}
	}
	return in, out, maj, nil
}

// diskCounters — накопительные счётчики строки /proc/diskstats.
type diskCounters struct{ readBytes, writeBytes, rios, rms, wios, wms, inflight, busyMS, weighted, dios, dms, fios, fms int64 }

// Номера полей строки diskstats после major, minor и имени (Documentation/admin-guide/iostats.rst).
const (
	diskReadOps = iota
	_
	diskReadSectors
	diskReadMS
	diskWriteOps
	_
	diskWriteSectors
	diskWriteMS
	diskInFlight
	diskBusyMS
	diskWeightedMS
	diskDiscardOps
	_
	_
	diskDiscardMS
	diskFlushOps
	diskFlushMS
	diskFields
)

// parseDiskstats — счётчики устройства dev; старые ядра без discard и flush дают там нули.
func parseDiskstats(data []byte, dev string) (diskCounters, error) {
	for len(data) > 0 {
		var line []byte
		line, data = nextLine(data)
		_, rest := nextField(line)
		_, rest = nextField(rest)
		name, rest := nextField(rest)
		if string(name) != dev {
			continue
		}
		var v [diskFields]int64
		for i := range v {
			var field []byte
			if field, rest = nextField(rest); len(field) == 0 {
				break
			}
			n, err := parseIntBytes(field)
			if err != nil {
				return diskCounters{}, fmt.Errorf("diskstats %s поле %d: %w", dev, i+4, err)
			}
			v[i] = n
		}
		return diskCounters{
			readBytes: v[diskReadSectors] * 512, writeBytes: v[diskWriteSectors] * 512,
			rios: v[diskReadOps], rms: v[diskReadMS], wios: v[diskWriteOps], wms: v[diskWriteMS],
			inflight: v[diskInFlight], busyMS: v[diskBusyMS], weighted: v[diskWeightedMS],
			dios: v[diskDiscardOps], dms: v[diskDiscardMS], fios: v[diskFlushOps], fms: v[diskFlushMS],
		}, nil
	}
	return diskCounters{}, fmt.Errorf("%w: %s нет в diskstats", ErrNoDevice, dev)
}

// Поля io.stat, которые берём: байты и операции чтения и записи.
const (
	ioReadBytes = iota
	ioWriteBytes
	ioReadOps
	ioWriteOps
	ioFields
)

// parseIOStat раскладывает io.stat по устройствам devices (major:minor); строки других устройств
// пропускаются, отсутствующее устройство даёт нули.
func parseIOStat(data []byte, devices []string, dst [][ioFields]int64) error {
	clear(dst)
	for len(data) > 0 {
		var line []byte
		line, data = nextLine(data)
		id, rest := nextField(line)
		index := -1
		for i, device := range devices {
			if string(id) == device {
				index = i
				break
			}
		}
		if index < 0 {
			continue
		}
		for len(rest) > 0 {
			var field []byte
			field, rest = nextField(rest)
			key, value, ok := bytes.Cut(field, []byte{'='})
			if !ok {
				continue
			}
			var slot int
			switch {
			case bytes.Equal(key, []byte("rbytes")):
				slot = ioReadBytes
			case bytes.Equal(key, []byte("wbytes")):
				slot = ioWriteBytes
			case bytes.Equal(key, []byte("rios")):
				slot = ioReadOps
			case bytes.Equal(key, []byte("wios")):
				slot = ioWriteOps
			default:
				continue
			}
			n, err := parseIntBytes(value)
			if err != nil {
				return fmt.Errorf("io.stat %s %s: %w", id, key, err)
			}
			dst[index][slot] = n
		}
	}
	return nil
}

// parseProcIO — read_bytes и write_bytes из /proc/<pid>/io.
//
// Запись — write_bytes минус cancelled_write_bytes: грязные страницы, отброшенные усечением или
// удалением файла до сброса, на диск не попали.
func parseProcIO(data []byte) (readBytes, writeBytes int64, err error) {
	var cancelled int64
	for len(data) > 0 {
		var line []byte
		line, data = nextLine(data)
		key, value, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			continue
		}
		var dst *int64
		switch {
		case bytes.Equal(key, []byte("read_bytes")):
			dst = &readBytes
		case bytes.Equal(key, []byte("write_bytes")):
			dst = &writeBytes
		case bytes.Equal(key, []byte("cancelled_write_bytes")):
			dst = &cancelled
		default:
			continue
		}
		if *dst, err = parseIntBytes(bytes.TrimSpace(value)); err != nil {
			return 0, 0, fmt.Errorf("io процесса %s: %w", key, err)
		}
	}
	return readBytes, max(0, writeBytes-cancelled), nil
}

// procStat — нужные поля /proc/<pid>/stat.
type procStat struct {
	comm  []byte // ссылается на буфер чтения
	ppid  int
	start uint64
	maj   int64
}

// Номера полей stat после «pid (comm)»: state — 0.
const (
	statPPID      = 1
	statMajflt    = 9
	statStartTime = 19
)

// parseProcStat разбирает /proc/<pid>/stat; comm может содержать пробелы и скобки, режем по последней ')'.
// akit proc.ParseStat majflt не отдаёт и копирует строки.
func parseProcStat(s []byte) (procStat, error) {
	l, r := bytes.IndexByte(s, '('), bytes.LastIndexByte(s, ')')
	if l < 0 || r <= l {
		return procStat{}, errors.New("stat без comm")
	}
	st := procStat{comm: s[l+1 : r]}
	rest := s[r+1:]
	for i := 0; i <= statStartTime; i++ {
		var field []byte
		if field, rest = nextField(rest); len(field) == 0 {
			return procStat{}, fmt.Errorf("короткий stat: %d полей", i)
		}
		var err error
		switch i {
		case statPPID:
			var n int64
			n, err = parseIntBytes(field)
			st.ppid = int(n)
		case statMajflt:
			st.maj, err = parseIntBytes(field)
		case statStartTime:
			st.start, err = parseUintBytes(field)
		}
		if err != nil {
			return procStat{}, fmt.Errorf("stat поле %d: %w", i+3, err)
		}
	}
	return st, nil
}

// memInfo — нужное из /proc/meminfo, байты.
type memInfo struct{ dirty, writeback, available, swapTotal, swapFree int64 }

// swapUsed — занятый своп, байты.
func (m memInfo) swapUsed() int64 { return m.swapTotal - m.swapFree }

// memInfoKeys — поля meminfo в порядке memInfo; нет любого — ошибка (ядро без них — не наше).
var memInfoKeys = [...][]byte{[]byte("Dirty:"), []byte("Writeback:"), []byte("MemAvailable:"), []byte("SwapTotal:"), []byte("SwapFree:")}

// parseMemInfo — Dirty, Writeback, MemAvailable, SwapTotal и SwapFree из /proc/meminfo, байты.
func parseMemInfo(data []byte) (memInfo, error) {
	var m memInfo
	dst := [len(memInfoKeys)]*int64{&m.dirty, &m.writeback, &m.available, &m.swapTotal, &m.swapFree}
	seen := 0
	for len(data) > 0 && seen < len(memInfoKeys) {
		var line []byte
		line, data = nextLine(data)
		key, rest := nextField(line)
		for i, want := range memInfoKeys {
			if !bytes.Equal(key, want) {
				continue
			}
			value, _ := nextField(rest)
			n, err := parseIntBytes(value)
			if err != nil {
				return memInfo{}, fmt.Errorf("meminfo %s: %w", key, err)
			}
			*dst[i] = n * 1024
			seen++
			break
		}
	}
	if seen < len(memInfoKeys) {
		return memInfo{}, fmt.Errorf("meminfo: из %d полей (Dirty, Writeback, MemAvailable, SwapTotal, SwapFree) найдено %d", len(memInfoKeys), seen)
	}
	return m, nil
}

// cachedFile — открытый файл счётчика: ядро пересчитывает содержимое на каждое чтение с нуля,
// так что pread без повторного open дешевле в три системных вызова.
type cachedFile struct {
	path string
	f    *os.File
}

// read читает файл целиком в buf, при нехватке места буфер растёт; возвращает буфер для повторного
// использования и прочитанное.
func (c *cachedFile) read(buf []byte) ([]byte, []byte, error) {
	if c.f == nil {
		f, err := os.Open(c.path)
		if err != nil {
			return buf, nil, fmt.Errorf("прочитать %s: %w", c.path, err)
		}
		c.f = f
	}
	for {
		n, err := c.f.ReadAt(buf, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return buf, nil, fmt.Errorf("прочитать %s: %w", c.path, err)
		}
		if n < len(buf) {
			return buf, buf[:n], nil
		}
		buf = make([]byte, 2*len(buf))
	}
}

func (c *cachedFile) close() {
	if c.f != nil {
		_ = c.f.Close()
		c.f = nil
	}
}

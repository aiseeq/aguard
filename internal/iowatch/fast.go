package iowatch

import (
	"fmt"
	"path/filepath"
	"time"
)

// FastReader — быстрый шаг машины целиком: PSI io, memory и cpu, vmstat, строка устройства в
// diskstats, готовые к работе из loadavg. Принадлежит одной горутине.
type FastReader struct {
	device                                           string
	ioPSI, memPSI, cpuPSI, vmstat, diskstat, loadavg cachedFile
	buf                                              []byte
	prev                                             fastCounters
	primed                                           bool
}

type fastCounters struct {
	ioSome, ioFull, memSome, memFull, cpuSome, swapIn, swapOut, majflt int64
	disk                                                               diskCounters
}

func NewFastReader(procRoot, device string) *FastReader {
	path := func(name string) cachedFile { return cachedFile{path: filepath.Join(procRoot, name)} }
	return &FastReader{device: device, ioPSI: path("pressure/io"), memPSI: path("pressure/memory"), cpuPSI: path("pressure/cpu"),
		vmstat: path("vmstat"), diskstat: path("diskstats"), loadavg: path("loadavg"), buf: make([]byte, 8192)}
}

func (r *FastReader) Close() {
	for _, f := range []*cachedFile{&r.ioPSI, &r.memPSI, &r.cpuPSI, &r.vmstat, &r.diskstat, &r.loadavg} {
		f.close()
	}
}

// Read — шаг: дельты к прошлому чтению. Первое чтение только запоминает счётчики и даёт пустой шаг.
func (r *FastReader) Read(now time.Time) (Step, error) {
	var c fastCounters
	var data []byte
	var err error
	if r.buf, data, err = r.ioPSI.read(r.buf); err != nil {
		return Step{}, err
	}
	if c.ioSome, c.ioFull, err = parsePressureTotals(data); err != nil {
		return Step{}, fmt.Errorf("разобрать %s: %w", r.ioPSI.path, err)
	}
	if r.buf, data, err = r.memPSI.read(r.buf); err != nil {
		return Step{}, err
	}
	if c.memSome, c.memFull, err = parsePressureTotals(data); err != nil {
		return Step{}, fmt.Errorf("разобрать %s: %w", r.memPSI.path, err)
	}
	if r.buf, data, err = r.cpuPSI.read(r.buf); err != nil {
		return Step{}, err
	}
	if c.cpuSome, _, err = parsePressureTotals(data); err != nil {
		return Step{}, fmt.Errorf("разобрать %s: %w", r.cpuPSI.path, err)
	}
	if r.buf, data, err = r.loadavg.read(r.buf); err != nil {
		return Step{}, err
	}
	runnable, err := parseLoadavgRunnable(data)
	if err != nil {
		return Step{}, err
	}
	if r.buf, data, err = r.vmstat.read(r.buf); err != nil {
		return Step{}, err
	}
	if c.swapIn, c.swapOut, c.majflt, err = parseVMStat(data); err != nil {
		return Step{}, err
	}
	if r.buf, data, err = r.diskstat.read(r.buf); err != nil {
		return Step{}, err
	}
	if c.disk, err = parseDiskstats(data, r.device); err != nil {
		return Step{}, err
	}
	p := r.prev
	r.prev = c
	s := Step{At: now}
	if !r.primed {
		r.primed = true
		return s, nil
	}
	s.Runnable = runnable // моментальное, не дельта
	s.IOSomeUS, s.IOFullUS = delta(c.ioSome, p.ioSome), delta(c.ioFull, p.ioFull)
	s.MemSomeUS, s.MemFullUS = delta(c.memSome, p.memSome), delta(c.memFull, p.memFull)
	s.CPUSomeUS = delta(c.cpuSome, p.cpuSome)
	s.SwapIn, s.SwapOut, s.Majflt = delta(c.swapIn, p.swapIn), delta(c.swapOut, p.swapOut), delta(c.majflt, p.majflt)
	d, pd := c.disk, p.disk
	s.ReadBytes, s.WriteBytes = delta(d.readBytes, pd.readBytes), delta(d.writeBytes, pd.writeBytes)
	s.IOTicks, s.WTimeMS = delta(d.busyMS, pd.busyMS), delta(d.weighted, pd.weighted)
	s.RIOS, s.RMS = delta(d.rios, pd.rios), delta(d.rms, pd.rms)
	s.WIOS, s.WMS = delta(d.wios, pd.wios), delta(d.wms, pd.wms)
	s.DIOS, s.DMS = delta(d.dios, pd.dios), delta(d.dms, pd.dms)
	s.FIOS, s.FMS = delta(d.fios, pd.fios), delta(d.fms, pd.fms)
	s.InFlight = d.inflight
	return s, nil
}

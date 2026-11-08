package iowatch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// Cap — потолок io.max ближайшей к листу cgroup (сам лист или предок), где для нашего диска или dm над ним
// задано число вместо max: путь этой cgroup и её пределы, 0 — без предела. Пусто — листа потолок не держит.
//
// Задача, которую держит потолок, ждёт придержанный blk-throttle bio в iowait — PSI считает это full, и на
// простаивающей машине системный io full под потолком 60–70 % без всякого фриза (случай 08.11 13:12:52: проба
// под IOReadBandwidthMax=30M, 8,1 с простоя, диск занят 5 %, интерфейс ни разу не застыл).
type Cap struct {
	Path  string `json:"path,omitempty"`
	RBps  int64  `json:"rbps,omitempty"`
	WBps  int64  `json:"wbps,omitempty"`
	RIOps int64  `json:"riops,omitempty"`
	WIOps int64  `json:"wiops,omitempty"`
}

// parseIOMax — пределы io.max для устройств devices: по каждому полю наименьший из заданных; max — без предела.
func parseIOMax(data []byte, devices []string) (Cap, error) {
	var c Cap
	for len(data) > 0 {
		var line []byte
		line, data = nextLine(data)
		id, rest := nextField(line)
		if len(id) == 0 || !slices.Contains(devices, string(id)) {
			continue
		}
		for len(rest) > 0 {
			var field []byte
			field, rest = nextField(rest)
			key, value, ok := bytes.Cut(field, []byte{'='})
			if !ok {
				return Cap{}, fmt.Errorf("поле без '=': %q", field)
			}
			var slot *int64
			switch string(key) {
			case "rbps":
				slot = &c.RBps
			case "wbps":
				slot = &c.WBps
			case "riops":
				slot = &c.RIOps
			case "wiops":
				slot = &c.WIOps
			default:
				return Cap{}, fmt.Errorf("неизвестное поле %q", key)
			}
			if string(value) == "max" {
				continue
			}
			n, err := strconv.ParseInt(string(value), 10, 64)
			if err != nil || n <= 0 {
				return Cap{}, fmt.Errorf("предел %s=%q", key, value)
			}
			if *slot == 0 || n < *slot {
				*slot = n
			}
		}
	}
	return c, nil
}

func (c Cap) limited() bool { return c.RBps|c.WBps|c.RIOps|c.WIOps != 0 }

// capOf — потолок листа rel: ближайшая cgroup с пределом от листа к корню (корень io.max не имеет). io.max
// читается каждый проход: потолок ставят и снимают на живой cgroup (systemctl set-property), а время папки
// от этого не меняется. Предок общий у многих листьев — за проход читается один раз (r.capPass).
func (r *cgroupReader) capOf(rel string) (Cap, error) {
	var chain []string
	var found Cap
	for dir := rel; dir != "." && dir != "" && dir != "/"; dir = filepath.Dir(dir) {
		if c, ok := r.capPass[dir]; ok {
			found = c
			break
		}
		chain = append(chain, dir)
		limits, err := r.readIOMax(dir)
		if err != nil {
			return Cap{}, err
		}
		if limits.limited() {
			limits.Path = "/" + dir
			found = limits
			break
		}
	}
	for _, dir := range chain {
		r.capPass[dir] = found
	}
	return found, nil
}

// readIOMax — пределы io.max папки rel; файла нет (контроллер io не включён у родителя) — без пределов.
func (r *cgroupReader) readIOMax(rel string) (Cap, error) {
	f := r.capFiles[rel]
	if f == nil {
		f = &cachedFile{path: filepath.Join(r.root, rel, "io.max")}
		r.capFiles[rel] = f
	}
	var data []byte
	var err error
	r.buf, data, err = f.read(r.buf)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODEV) {
		f.close()
		return Cap{}, nil
	}
	if err != nil {
		return Cap{}, err
	}
	c, err := parseIOMax(data, r.devices)
	if err != nil {
		return Cap{}, fmt.Errorf("разобрать %s: %w", f.path, err)
	}
	return c, nil
}

// dropCapFiles закрывает io.max папок, которых больше нет в дереве (keep — живые пути от root).
func (r *cgroupReader) dropCapFiles(keep map[string]bool) {
	for rel, f := range r.capFiles {
		if !keep[rel] {
			f.close()
			delete(r.capFiles, rel)
		}
	}
}

const (
	// capRateShare — доля предела, с которой cgroup под потолком упирается в него: проба 08.11 шла 76 МиБ/с при
	// 80M (99,6 %) и 28–30 МиБ/с при 30M (98–105 %). Ниже — потолок не держит, и простой не его.
	capRateShare = 0.8
	// capOutsideMS — простой любой cgroup вне потолка за случай меньше: остальные не ждали (13:12:52 — до 51 мс).
	capOutsideMS = 200
)

// cappedWait — случай [start, end] — ожидание под потолком, а не фриз машины: по детальным записям самого
// случая (без окна контекста: простой до случая не его) cgroup под потолками, упиравшиеся в них, дали больше
// половины простоя листьев, ни одна cgroup вне них не ждала capOutsideMS, диск занят меньше половины случая.
// outside — наибольший простой cgroup вне потолка, мс; who — простой по потолкам, мс.
//
// Упор — по каждой записи отдельно: сумма листьев под одним потолком за запись не ниже capRateShare предела
// в ограниченном направлении. Простой записи без упора (предел записи при простое чтения, последняя секунда
// чтения, снятый потолок) потолком не оправдан и считается простоем вне потолка.
//
// Не видит (известные пределы): потолок на самом интерфейсе (его ожидание — тоже «под потолком»; на 08.11 число в
// io.max есть только у agents-evals.slice) и ожидание блокировки ФС, которую держит придержанный писатель, — оно не
// в PSI io full ждущего. Срезы интерфейса (guiwatch) детектор не сверяет.
func cappedWait(details []Step, start, end time.Time, detail time.Duration, durMS, busyMS int64) (capped bool, outside int64, who []Ranked) {
	type owner struct{ rd, wr, rios, wios, idleUS int64 }
	outsideUS := map[string]int64{}
	byName := map[string]int64{}
	var totalUS, cappedUS int64
	owners := map[Cap]*owner{}
	for i := range details {
		d := &details[i]
		// Запись — дельта с прошлого прохода: медленный проход (тикер пропустил такты) покрывает несколько секунд,
		// и скорость — байты за весь этот промежуток. Первая запись окна — за номинальный detail.
		interval := detail
		if i > 0 {
			interval = max(detail, d.At.Sub(details[i-1].At))
		}
		if !d.At.After(start) || !d.At.Add(-interval).Before(end) {
			continue // запись покрывает (At − interval, At]: с случаем не пересекается
		}
		sec := interval.Seconds()
		atCap := func(sum, limit int64) bool { return limit > 0 && float64(sum)/sec >= capRateShare*float64(limit) }
		clear(owners)
		for j := range d.Cgroups {
			g := &d.Cgroups[j]
			totalUS += g.IOFullUS
			if !g.Cap.limited() {
				outsideUS[CgroupName(g.Path)] += g.IOFullUS
				continue
			}
			o := owners[g.Cap]
			if o == nil {
				o = &owner{}
				owners[g.Cap] = o
			}
			o.rd += g.ReadBytes
			o.wr += g.WriteBytes
			o.rios += g.ReadOps
			o.wios += g.WriteOps
			o.idleUS += g.IOFullUS
		}
		for c, o := range owners {
			if atCap(o.rd, c.RBps) || atCap(o.wr, c.WBps) || atCap(o.rios, c.RIOps) || atCap(o.wios, c.WIOps) {
				cappedUS += o.idleUS
				byName[CgroupName(c.Path)] += o.idleUS
				continue
			}
			outsideUS[CgroupName(c.Path)] += o.idleUS
		}
	}
	for _, us := range outsideUS {
		outside = max(outside, us/1000)
	}
	for name, us := range byName {
		byName[name] = us / 1000
	}
	capped = cappedUS > 0 && 2*cappedUS > totalUS && outside < capOutsideMS && 2*busyMS < durMS
	return capped, outside, Rank(byName, caseTop)
}

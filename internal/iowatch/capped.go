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

// capPart — простой листа или потолка в одной детальной записи: cgroup вне потолка (по имени) или потолок (по
// имени его cgroup) с пометкой, упирались ли в него за запись.
type capPart struct {
	name   string
	idleUS int64
	capped bool
}

// capOwner — листья под одним потолком в записи.
type capOwner struct {
	cap                        Cap
	rd, wr, rios, wios, idleUS int64
}

// capParts сводит запись за interval в простой по потолкам и вне их, дописывая в dst. Упор — по сумме листьев
// под одним потолком за запись: не ниже capRateShare предела в ограниченном направлении. Простой под потолком
// без упора (предел записи при простое чтения, последняя секунда чтения, снятый потолок) потолком не оправдан и
// идёт как простой вне потолка. owners — буфер для повторного использования.
func capParts(d *Step, interval time.Duration, dst []capPart, owners []capOwner) ([]capPart, []capOwner) {
	owners = owners[:0]
	for j := range d.Cgroups {
		g := &d.Cgroups[j]
		if !g.Cap.limited() {
			if g.IOFullUS > 0 {
				dst = append(dst, capPart{name: CgroupName(g.Path), idleUS: g.IOFullUS})
			}
			continue
		}
		k := slices.IndexFunc(owners, func(o capOwner) bool { return o.cap == g.Cap })
		if k < 0 {
			owners = append(owners, capOwner{cap: g.Cap})
			k = len(owners) - 1
		}
		o := &owners[k]
		o.rd += g.ReadBytes
		o.wr += g.WriteBytes
		o.rios += g.ReadOps
		o.wios += g.WriteOps
		o.idleUS += g.IOFullUS
	}
	sec := interval.Seconds()
	atCap := func(sum, limit int64) bool { return limit > 0 && float64(sum)/sec >= capRateShare*float64(limit) }
	for k := range owners {
		o, c := &owners[k], owners[k].cap
		if o.idleUS == 0 {
			continue
		}
		dst = append(dst, capPart{name: CgroupName(c.Path), idleUS: o.idleUS,
			capped: atCap(o.rd, c.RBps) || atCap(o.wr, c.WBps) || atCap(o.rios, c.RIOps) || atCap(o.wios, c.WIOps)})
	}
	return dst, owners
}

// capAcc — простой записей, взвешенный долей записи внутри окна (случая или окна now).
type capAcc struct {
	totalUS, cappedUS float64
	outside, byName   map[string]float64
}

func newCapAcc() capAcc { return capAcc{outside: map[string]float64{}, byName: map[string]float64{}} }

// add — простой записи: под потолком — с весом capped, вне потолка — с весом outside.
func (a *capAcc) add(parts []capPart, capped, outside float64) {
	for _, p := range parts {
		if p.capped {
			v := float64(p.idleUS) * capped
			a.totalUS += v
			a.cappedUS += v
			a.byName[p.name] += v
			continue
		}
		v := float64(p.idleUS) * outside
		a.totalUS += v
		a.outside[p.name] += v
	}
}

// capVerdict — признание окна: ожидание под потолком (Capped), наибольший простой cgroup вне потолка и простой
// упиравшихся в потолки, мс; Who — простой по потолкам.
type capVerdict struct {
	Capped              bool
	OutsideMS, CappedMS int64
	Who                 []Ranked
}

// verdict — под потолком: упиравшиеся в потолки дали больше половины простоя листьев, ни одна cgroup вне них не
// ждала capOutsideMS, диск занят меньше половины окна длиной durMS.
func (a *capAcc) verdict(durMS, busyMS int64) capVerdict {
	var v capVerdict
	for _, us := range a.outside {
		v.OutsideMS = max(v.OutsideMS, int64(us)/1000)
	}
	who := make(map[string]int64, len(a.byName))
	for name, us := range a.byName {
		who[name] = int64(us) / 1000
	}
	v.CappedMS = int64(a.cappedUS) / 1000
	v.Who = Rank(who, caseTop)
	v.Capped = a.cappedUS > 0 && 2*a.cappedUS > a.totalUS && v.OutsideMS < capOutsideMS && 2*busyMS < durMS
	return v
}

// overlapShare — доля записи (from, at] внутри [start, end], UnixNano; 0 — не пересекаются.
func overlapShare(from, at, start, end int64) float64 {
	lo, hi := max(from, start), min(at, end)
	if hi <= lo || at <= from {
		return 0
	}
	return float64(hi-lo) / float64(at-from)
}

// cappedWait — случай [start, end] — ожидание под потолком, а не фриз машины (capAcc.verdict) по детальным
// записям, пересекающим случай: простой под потолком — пропорционально доле записи внутри случая (простой до
// случая не его), простой вне потолка — целиком: фриз и есть этот простой, равномерным внутри записи он не бывает
// (проверка 08.11 14:27: plasmashell 330 мс в записи с долей 0,6 иначе дал бы 198 мс — ниже порога, без письма).
// Запись — дельта с прошлого прохода: медленный проход (тикер пропустил такты) покрывает несколько секунд, и
// скорость упора — байты за весь этот промежуток. Интервал первой записи передаётся отдельно:
// он может начинаться до окна, если предыдущая запись была раньше.
//
// Не видит (известные пределы): потолок на самом интерфейсе (его ожидание — тоже «под потолком»; на 08.11 число в
// io.max есть только у agents-evals.slice) и ожидание блокировки ФС, которую держит придержанный писатель, — оно не
// в PSI io full ждущего. Срезы интерфейса (guiwatch) детектор не сверяет.
func cappedWait(details []Step, start, end time.Time, detail time.Duration, durMS, busyMS int64) capVerdict {
	return cappedWaitFirstInterval(details, start, end, detail, durMS, busyMS)
}

func cappedWaitFirstInterval(details []Step, start, end time.Time, firstInterval time.Duration, durMS, busyMS int64) capVerdict {
	acc := newCapAcc()
	var parts []capPart
	var owners []capOwner
	for i := range details {
		d := &details[i]
		interval := firstInterval
		if i > 0 {
			interval = d.At.Sub(details[i-1].At)
			if interval <= 0 {
				interval = firstInterval
			}
		}
		share := overlapShare(d.At.Add(-interval).UnixNano(), d.At.UnixNano(), start.UnixNano(), end.UnixNano())
		if share == 0 {
			continue
		}
		parts, owners = capParts(d, interval, parts[:0], owners)
		acc.add(parts, share, 1)
	}
	return acc.verdict(durMS, busyMS)
}

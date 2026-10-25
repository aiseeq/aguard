package policy

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pressure — давление памяти: PSI some avg60 и доля занятого свопа.
type Pressure struct {
	PSISomeAvg60 float64
	SwapUsedPct  float64
}

// ParsePSI достаёт «some avg60» из /proc/pressure/memory.
func ParsePSI(s string) (float64, error) {
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for f := range strings.FieldsSeq(line) {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				x, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return 0, fmt.Errorf("PSI avg60: %w", err)
				}
				return x, nil
			}
		}
	}
	return 0, errors.New("PSI: нет строки some с avg60")
}

// ParseSwapUsedPct считает занятый своп в процентах по /proc/meminfo; свопа нет — 0.
func ParseSwapUsedPct(meminfo string) (float64, error) {
	var total, free float64
	sc := bufio.NewScanner(strings.NewReader(meminfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || (f[0] != "SwapTotal:" && f[0] != "SwapFree:") {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			return 0, fmt.Errorf("meminfo %s: %w", f[0], err)
		}
		if f[0] == "SwapTotal:" {
			total = v
		} else {
			free = v
		}
	}
	if total <= 0 {
		return 0, nil
	}
	return (total - free) / total * 100, nil
}

// Factor — множитель порога: 1 в норме, pressure.factor под давлением.
func (c *Config) Factor(p Pressure) float64 {
	if (c.Pressure.PSISomeAvg60 > 0 && p.PSISomeAvg60 >= c.Pressure.PSISomeAvg60) ||
		(c.Pressure.SwapUsedPct > 0 && p.SwapUsedPct >= c.Pressure.SwapUsedPct) {
		return c.Pressure.Factor
	}
	return 1
}

// ReadPressure читает давление памяти из procRoot (/proc). При ошибке отдаёт
// то, что прочиталось (непрочитанное — 0, то есть «давления нет»), и ошибку.
func ReadPressure(procRoot string) (Pressure, error) {
	var p Pressure
	psi, err := os.ReadFile(filepath.Join(procRoot, "pressure", "memory"))
	if err != nil {
		return p, fmt.Errorf("PSI: %w", err)
	}
	if p.PSISomeAvg60, err = ParsePSI(string(psi)); err != nil {
		return p, err
	}
	mi, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return p, fmt.Errorf("meminfo: %w", err)
	}
	p.SwapUsedPct, err = ParseSwapUsedPct(string(mi))
	return p, err
}

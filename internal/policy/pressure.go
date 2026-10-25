package policy

import (
	"bufio"
	"strconv"
	"strings"
)

// Pressure — давление памяти: PSI some avg60 и доля занятого свопа.
type Pressure struct {
	PSISomeAvg60 float64
	SwapUsedPct  float64
}

// ParsePSI достаёт «some avg60» из /proc/pressure/memory.
func ParsePSI(s string) float64 {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				x, _ := strconv.ParseFloat(v, 64)
				return x
			}
		}
	}
	return 0
}

// ParseSwapUsedPct считает занятый своп в процентах по /proc/meminfo.
func ParseSwapUsedPct(meminfo string) float64 {
	var total, free float64
	sc := bufio.NewScanner(strings.NewReader(meminfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "SwapTotal:":
			total = v
		case "SwapFree:":
			free = v
		}
	}
	if total <= 0 {
		return 0
	}
	return (total - free) / total * 100
}

// Factor — множитель порога: 1 в норме, pressure.factor под давлением.
func (c *Config) Factor(p Pressure) float64 {
	if (c.Pressure.PSISomeAvg60 > 0 && p.PSISomeAvg60 >= c.Pressure.PSISomeAvg60) ||
		(c.Pressure.SwapUsedPct > 0 && p.SwapUsedPct >= c.Pressure.SwapUsedPct) {
		return c.Pressure.Factor
	}
	return 1
}

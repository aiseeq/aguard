package policy

import "github.com/aiseeq/akit/pressure"

// Factor — множитель порога: 1 в норме, pressure.factor под давлением.
func (c *Config) Factor(p pressure.Memory) float64 {
	if (c.Pressure.PSISomeAvg60 > 0 && p.PSISomeAvg60 >= c.Pressure.PSISomeAvg60) ||
		(c.Pressure.SwapUsedPct > 0 && p.SwapUsedPct >= c.Pressure.SwapUsedPct) {
		return c.Pressure.Factor
	}
	return 1
}

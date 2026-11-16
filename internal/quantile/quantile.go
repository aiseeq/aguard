// Package quantile provides quantiles for values sorted in ascending order.
package quantile

// Of returns the floor-index quantile of sorted values, or zero for an empty slice.
func Of(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(q*float64(len(sorted)-1))]
}

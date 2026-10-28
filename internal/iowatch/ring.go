package iowatch

import "time"

// Ring — кольцевой буфер записей за последние window. Ёмкость считается от шагов, так что при
// штатном темпе вытеснение идёт по времени; при переполнении уходит самая старая запись.
type Ring struct {
	records     []Record
	head, count int
	window      time.Duration
}

// NewRing — буфер на cfg.Ring: быстрые шаги и детальные записи с запасом на неровный тикер.
func NewRing(cfg Config) *Ring {
	window := cfg.Ring.D()
	capacity := int(window/cfg.Step.D()+window/cfg.Detail.D())*11/10 + 16
	return &Ring{records: make([]Record, capacity), window: window}
}

// Add кладёт запись и вытесняет записи старше window от неё.
func (r *Ring) Add(rec Record) {
	if r.count == len(r.records) {
		r.head = (r.head + 1) % len(r.records)
		r.count--
	}
	r.records[(r.head+r.count)%len(r.records)] = rec
	r.count++
	cutoff := rec.At.Add(-r.window)
	for r.count > 0 && r.records[r.head].At.Before(cutoff) {
		r.records[r.head] = Record{} // не держать срезы процессов вытесненной записи
		r.head = (r.head + 1) % len(r.records)
		r.count--
	}
}

// Records — копия записей по порядку добавления.
func (r *Ring) Records() []Record {
	out := make([]Record, r.count)
	for i := range out {
		out[i] = r.records[(r.head+i)%len(r.records)]
	}
	return out
}

// Len — сколько записей в буфере.
func (r *Ring) Len() int { return r.count }

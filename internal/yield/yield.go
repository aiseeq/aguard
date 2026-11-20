// Package yield — уступка долгого обхода занятой машине: обход disk (~/.cache — миллионы stat и FIEMAP) и полный
// замер слежения (internal/track: du каждой записи и листинг контейнеров) при холодном кэше метаданных читают диск.
// 30.10 12:47–12:51: 22 МБ/с метаданных btrfs, обход disk 4+ мин вместо ~20 с, его чтение держало очередь graft heavy
// (замер akit-master); 09.11–10.11: полный замер слежения на старте демона после загрузки — сотни МБ чтения за секунды
// и фризы диска 1–2,6 с (тикет io-freeze-aguard). Машину меряет правило akit (как `busy`), не чаще раза в Check;
// занята — пауза Pause: обход не встаёт насовсем, а идёт урывками. Уступает только сам aguard, агентов не ограничивает.
package yield

import (
	"context"
	"fmt"
	"time"

	"github.com/aiseeq/akit/pressure"
)

const (
	Check = time.Second     // замер машины не чаще
	Pause = 2 * time.Second // пауза на занятой машине
)

// Yielder — уступка одного обходчика; не для одновременных вызовов из разных горутин.
type Yielder struct {
	meter  *pressure.Meter
	warn   func(error)
	status func(error)
	clear  func()
	check  time.Duration
	pause  time.Duration
	last   time.Time
	warned bool
	Pauses int           // пауз с создания
	Paused time.Duration // их время
}

// New — уступка по машине procRoot; warn получает первую ошибку замера.
func New(procRoot string, warn func(error)) *Yielder {
	return newWith(pressure.NewMeter(procRoot), warn)
}

// NewObserved reports every pressure fault and recovery to a long-lived owner. This keeps
// alert timing stable when callers create a fresh Yielder for each observation round.
func NewObserved(procRoot string, warn func(error), fault func(error), recovered func()) *Yielder {
	y := newWith(pressure.NewMeter(procRoot), warn)
	y.status, y.clear = fault, recovered
	return y
}

func newWith(meter *pressure.Meter, warn func(error)) *Yielder {
	return &Yielder{meter: meter, warn: warn, check: Check, pause: Pause}
}

// Wait pauses when pressure is high or cannot be measured; after a measurement
// error it returns as a busy pause so disk and track can continue in portions.
func (y *Yielder) Wait(ctx context.Context) (bool, error) {
	if time.Since(y.last) < y.check {
		return false, ctx.Err()
	}
	y.last = time.Now()
	busy, err := y.busy(ctx)
	if err != nil {
		y.warnOnce(err)
		y.reportFault(err)
		y.pauseFor(ctx)
		return true, ctx.Err()
	}
	y.reportRecovery()
	if !busy {
		return false, ctx.Err()
	}
	y.pauseFor(ctx)
	return true, ctx.Err()
}

// WaitSafe applies the same pressure rule as Wait, but returns meter errors after
// pausing. Destructive/background work must not continue when pressure is unknown.
func (y *Yielder) WaitSafe(ctx context.Context) error {
	if time.Since(y.last) < y.check {
		return nil
	}
	y.last = time.Now()
	busy, err := y.busy(ctx)
	if err != nil {
		y.warnOnce(err)
		y.reportFault(err)
		y.pauseFor(ctx)
		return err
	}
	y.reportRecovery()
	if busy {
		y.pauseFor(ctx)
	}
	return ctx.Err()
}

func (y *Yielder) pauseFor(ctx context.Context) {
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(y.pause):
	}
	y.Pauses++
	y.Paused += time.Since(start)
	y.last = time.Now()
}

// busy — занята ли машина по правилу akit. Ошибка возвращается, чтобы оба режима уступили машину.
func (y *Yielder) busy(ctx context.Context) (bool, error) {
	// Пороги пользователя — те же, что у busy и очереди graft ($XDG_CONFIG_HOME/akit/pressure.yaml поверх умолчаний);
	// файл битый — ошибка, умолчаниями не подменяем.
	limits, err := pressure.UserLimits()
	if err != nil {
		return false, fmt.Errorf("пороги pressure: %w", err)
	}
	var v pressure.Verdict
	v, err = y.meter.Check(ctx, limits, "", false)
	if err != nil {
		return false, fmt.Errorf("замер pressure: %w", err)
	}
	return v.Busy(), nil
}

func (y *Yielder) warnOnce(err error) {
	if !y.warned && y.warn != nil {
		y.warned = true
		y.warn(err)
	}
}

func (y *Yielder) reportFault(err error) {
	if y.status != nil {
		y.status(err)
	}
}

func (y *Yielder) reportRecovery() {
	if y.clear != nil {
		y.clear()
	}
}

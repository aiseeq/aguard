// Package yield — уступка долгого обхода занятой машине: обход disk (~/.cache — миллионы stat и FIEMAP) и полный
// замер слежения (internal/track: du каждой записи и листинг контейнеров) при холодном кэше метаданных читают диск.
// 30.10 12:47–12:51: 22 МБ/с метаданных btrfs, обход disk 4+ мин вместо ~20 с, его чтение держало очередь graft heavy
// (замер akit-master); 09.11–10.11: полный замер слежения на старте демона после загрузки — сотни МБ чтения за секунды
// и фризы диска 1–2,6 с (тикет io-freeze-aguard). Машину меряет правило akit (как `busy`), не чаще раза в Check;
// занята — пауза Pause: обход не встаёт насовсем, а идёт урывками. Уступает только сам aguard, агентов не ограничивает.
package yield

import (
	"context"
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
	check  time.Duration
	pause  time.Duration
	last   time.Time
	warned bool
	Pauses int           // пауз с создания
	Paused time.Duration // их время
}

// New — уступка по машине procRoot; warn получает первую ошибку замера (дальше молчит, обход идёт без пауз).
func New(procRoot string, warn func(error)) *Yielder {
	return newWith(pressure.NewMeter(procRoot), warn)
}

func newWith(meter *pressure.Meter, warn func(error)) *Yielder {
	return &Yielder{meter: meter, warn: warn, check: Check, pause: Pause}
}

// Wait — не раньше Check с прошлого замера (или конца паузы) меряет машину; занята — ждёт Pause или отмены ctx.
// true — пауза была. Следующий замер — через Check после паузы: на постоянно занятой машине обход идёт урывками
// (Check работы на Pause паузы) и кончается.
func (y *Yielder) Wait(ctx context.Context) bool {
	if time.Since(y.last) < y.check {
		return false
	}
	y.last = time.Now()
	if !y.busy(ctx) {
		return false
	}
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(y.pause):
	}
	y.Pauses++
	y.Paused += time.Since(start)
	y.last = time.Now()
	return true
}

// busy — занята ли машина по правилу akit. Ошибка замера — первая в warn, а обход идёт без пауз: уступка — забота
// о машине, а не условие работы, и встать из-за битого файла порогов обход не должен.
func (y *Yielder) busy(ctx context.Context) bool {
	// Пороги пользователя — те же, что у busy и очереди graft ($XDG_CONFIG_HOME/akit/pressure.yaml поверх умолчаний);
	// файл битый — ошибка, умолчаниями не подменяем.
	limits, err := pressure.UserLimits()
	var v pressure.Verdict
	if err == nil {
		v, err = y.meter.Check(ctx, limits, "", false)
	}
	if err != nil && !y.warned {
		y.warned = true
		y.warn(err)
	}
	return err == nil && v.Busy()
}

package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/names"
)

// queueNamesRepeat — повтор письма лиду о той же поломке таблицы имён.
const queueNamesRepeat = 24 * time.Hour

// queueNamesMemory — прошлая удачная таблица имён в памяти демона и отметка о письме лиду про битую.
type queueNamesMemory struct {
	table   map[string]string
	at      time.Time // когда прочитана прошлая удачная
	loaded  bool
	err     string // последняя ошибка чтения; "" — таблица читается
	sentErr string // ошибка, о которой написали лиду
	sentAt  time.Time
}

// queueNamesTable — таблица имён для очереди (tk ls, внешние, будильник проектов). Строка без табуляции не останавливает
// письма: при прогретой таблице — WARN и прошлая (письма уходят по её именам); прошлой нет (холодный старт) — ошибка:
// выдумывать хозяев нельзя. Причину лиду пишет queueNamesAlert.
func (a *app) queueNamesTable() (map[string]string, error) {
	m := &a.queueNames
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	if err == nil {
		m.table, m.at, m.loaded = table, time.Now(), true
		m.err, m.sentErr, m.sentAt = "", "", time.Time{}
		return table, nil
	}
	m.err = err.Error()
	if !m.loaded {
		return nil, err
	}
	a.warn("queue", "таблица имён битая, иду на прошлой", "err", err)
	return m.table, nil
}

// queueNamesAlert — одно письмо лиду о битой таблице имён; та же ошибка — не чаще раза в сутки, починка снимает отметку.
// Лида нет в живых — письмо ждёт следующего прохода. Отметка в памяти: рестарт демона пишет снова (очередь могла встать).
func (a *app) queueNamesAlert(c *config.Config, dry bool, now time.Time) {
	m := &a.queueNames
	if m.err == "" || m.err == m.sentErr && now.Sub(m.sentAt) < queueNamesRepeat {
		return
	}
	path := filepath.Join(a.home, names.File)
	msg := fmt.Sprintf("aguard queue: таблица имён %s не читается: %s.\n", path, m.err)
	if m.loaded {
		msg += fmt.Sprintf("Очередь идёт на прошлой таблице (прочитана %s): письма по тикетам уходят по её именам; "+
			"если таблицу сменили вместе с поломкой (переименовали мастера), письма идут по старым именам. Поправь строку — следующий проход возьмёт новую.",
			m.at.Format("02.01 15:04 MST"))
	} else {
		msg += "Прошлой таблицы нет (демон запущен при битой): письма по тикетам стоят, пока таблицу не поправят."
	}
	if dry {
		fmt.Fprintf(a.out, "names %s: %s\n", c.Lead, msg)
		return
	}
	alive, err := a.reg.Alive()
	if err != nil {
		a.warn("queue", "реестр сессий", "err", err)
	}
	if _, ok := alive[c.Lead]; !ok {
		return
	}
	if err := a.sendAlive(alive, c.Lead, c.SendAuth, msg); err != nil {
		if !a.quietFailure("queue", err) {
			a.warn("queue", "письмо не отправлено", "to", c.Lead, "err", err)
		}
		return
	}
	m.sentErr, m.sentAt = m.err, now
	a.note("queue", "sent", "names", c.Lead, 1, "notice")
}

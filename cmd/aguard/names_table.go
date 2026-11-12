package main

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/aiseeq/aguard/internal/config"
	"github.com/aiseeq/aguard/internal/names"
)

const (
	namesAlertRepeat = 24 * time.Hour // повтор письма лиду о той же поломке таблицы имён
	namesWarnRepeat  = time.Hour      // повтор WARN «иду на прошлой» одного наблюдателя о той же поломке
)

// namesMu охраняет app.namesMem: наблюдатели читают таблицу из своих горутин (queue, agentwaits, disk, io, said, тикеты
// io и disk). Замок вне app: тесты копируют app как «новый процесс», копия замка — ошибка vet copylocks.
var namesMu sync.Mutex

// namesMemory — прошлая удачная таблица имён в памяти демона, отметки о WARN и письме лиду про битую; под namesMu.
type namesMemory struct {
	table   map[string]string // не меняется после чтения: отдаётся всем без копии
	at      time.Time         // когда прочитана прошлая удачная
	loaded  bool
	err     string                 // последняя ошибка чтения; "" — таблица читается
	warned  map[string]namesWarned // наблюдатель → последний WARN «иду на прошлой»
	sentErr string                 // ошибка, о которой написали лиду
	sentAt  time.Time
}

type namesWarned struct {
	err string
	at  time.Time
}

// namesTable — таблица имён cl (~/.config/cl/names.tsv) для наблюдателя watcher; единственное чтение её демоном.
// Файл читается каждый раз: поправленную таблицу следующий проход берёт сразу. Строка без табуляции не останавливает
// наблюдателей: при прогретой таблице — WARN (раз в namesWarnRepeat на наблюдателя и текст ошибки) и прошлая; прошлой нет
// (холодный старт) — ошибка: выдумывать хозяев нельзя, письмо не тому хуже неотправленного. Лиду пишет namesAlert.
func (a *app) namesTable(watcher string) (map[string]string, error) {
	m := &a.namesMem
	namesMu.Lock()
	table, err := names.ReadTable(filepath.Join(a.home, names.File), a.home)
	now := time.Now()
	if err == nil {
		m.table, m.at, m.loaded = table, now, true
		m.err, m.sentErr, m.sentAt = "", "", time.Time{}
		clear(m.warned)
		namesMu.Unlock()
		return table, nil
	}
	m.err = err.Error()
	if !m.loaded {
		namesMu.Unlock()
		return nil, err
	}
	table, at := m.table, m.at
	w := m.warned[watcher] // нет отметки — пустой текст, не равный ошибке
	due := w.err != m.err || now.Sub(w.at) >= namesWarnRepeat
	if due {
		if m.warned == nil {
			m.warned = map[string]namesWarned{}
		}
		m.warned[watcher] = namesWarned{err: m.err, at: now}
	}
	namesMu.Unlock()
	if due {
		a.warn(watcher, "таблица имён битая, иду на прошлой", "err", err, "read_at", at.Format(time.DateTime))
	}
	return table, nil
}

// namesAlert — одно письмо лиду о битой таблице имён на все места демона; та же ошибка — не чаще раза в сутки, починка
// снимает отметку. Шлёт только проход очереди. Лида нет в живых — письмо ждёт следующего прохода. Отметка в памяти:
// рестарт демона пишет снова (наблюдатели могли встать).
func (a *app) namesAlert(c *config.Config, dry bool, now time.Time) {
	m := &a.namesMem
	namesMu.Lock()
	cur, loaded, at := m.err, m.loaded, m.at
	skip := cur == "" || cur == m.sentErr && now.Sub(m.sentAt) < namesAlertRepeat
	namesMu.Unlock()
	if skip {
		return
	}
	path := filepath.Join(a.home, names.File)
	msg := fmt.Sprintf("aguard: таблица имён %s не читается: %s.\n", path, cur)
	if loaded {
		msg += fmt.Sprintf("Наблюдатели aguard идут на прошлой таблице (прочитана %s): письма очереди по тикетам, хозяева "+
			"находок disk и случаев io, папки тикетов io и disk, имена сессий в agentwaits и said — по её именам; если таблицу "+
			"сменили вместе с поломкой (переименовали мастера), всё это идёт по старым именам. Поправь строку — следующий "+
			"проход каждого наблюдателя возьмёт новую.", at.Format("02.01 15:04 MST"))
	} else {
		msg += "Прошлой таблицы нет (демон запущен при битой): письма очереди по тикетам и проходы agentwaits, disk и said " +
			"стоят; случаи io, чей хозяин — по папке, идут лиду как без хозяина; тикеты io и disk — в папку лида. Так до " +
			"починки таблицы."
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
	if err := a.sendAlive(alive, c.Lead, c.SendAuth, "names.broken", msg); err != nil {
		if !a.quietFailure("queue", err) {
			a.warn("queue", "письмо не отправлено", "to", c.Lead, "err", err)
		}
		return
	}
	a.namesSent(cur, now)
	a.note("queue", "sent", "names", c.Lead, 1, "notice")
}

// namesSent — отметка о письме лиду про ошибку sent. Пока письмо шло, таблицу могли поправить (или сломать иначе) — тогда
// отметки нет: починка её снимает, иначе та же поломка после починки сутки шла бы без письма.
func (a *app) namesSent(sent string, now time.Time) {
	m := &a.namesMem
	namesMu.Lock()
	defer namesMu.Unlock()
	if m.err == sent {
		m.sentErr, m.sentAt = sent, now
	}
}

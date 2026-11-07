package track

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// Записи без события выхода (выход потерян, процесс в чужом пространстве
// имён) копились сотнями тысяч (тикет tmpguard-track-swap): старше keep и
// без процесса в /proc они получают выход и живут ещё keep, затем
// снимаются; живые, свежие и непроверенные остаются.
func TestTombsExpireOpenWithoutProc(t *testing.T) {
	var logs bytes.Buffer
	tb := newTombs(time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
	tb.gone = func(pid int) (bool, error) {
		switch pid {
		case 2:
			return false, nil
		case 6:
			return false, errors.New("stat: permission denied")
		}
		return true, nil
	}
	old := time.Now().Add(-2 * time.Minute)
	tb.m[1] = &tomb{seen: old}                                           // нет процесса, старая — выход сейчас
	tb.m[2] = &tomb{seen: old}                                           // процесс жив — без выхода
	tb.m[3] = &tomb{seen: time.Now()}                                    // свежая — без выхода
	tb.m[4] = &tomb{seen: old, exited: time.Now()}                       // вышел недавно — оставить
	tb.m[5] = &tomb{seen: old, exited: time.Now().Add(-2 * time.Minute)} // вышел давно — снять
	tb.m[6] = &tomb{seen: old}                                           // проверка не удалась — без выхода, WARN
	tb.expireOnce()
	for pid, want := range map[int]bool{1: true, 2: true, 3: true, 4: true, 5: false, 6: true} {
		if _, ok := tb.m[pid]; ok != want {
			t.Errorf("pid %d: запись есть=%v, ждали %v", pid, ok, want)
		}
	}
	if tb.m[1].exited.IsZero() {
		t.Errorf("pid 1: выход не поставлен — запоздалое событие не нашло бы создателя после снятия")
	}
	for _, pid := range []int{2, 3, 6} {
		if !tb.m[pid].exited.IsZero() {
			t.Errorf("pid %d: выход поставлен", pid)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "permission denied") {
		t.Errorf("нет WARN об ошибке проверки: %s", logs.String())
	}
	if total, open := tb.size(); total != 5 || open != 3 {
		t.Errorf("size = %d, %d; ждали 5, 3", total, open)
	}
	tb.m[1].exited = time.Now().Add(-2 * time.Minute)
	tb.expireOnce()
	if _, ok := tb.m[1]; ok {
		t.Errorf("pid 1: запись не снята через keep после выхода")
	}
}

// execMsg — cn_msg(20) + proc_event exec для pid (как шлёт proc connector).
func execMsg(pid int) []byte {
	b := make([]byte, 20+16+24)
	binary.LittleEndian.PutUint32(b[20:], procEvExec)
	binary.LittleEndian.PutUint32(b[20+16:], uint32(pid))
	binary.LittleEndian.PutUint32(b[20+16+4:], uint32(pid))
	return b
}

// Пока /proc проверяется без мьютекса, pid ушёл новому процессу (exec
// обновил запись, fork потерян): запись не помечается. Постоянная ошибка
// проверки даёт один WARN в час, не на каждом обходе.
func TestTombsExpireRaceAndWarnOnce(t *testing.T) {
	var logs bytes.Buffer
	tb := newTombs(time.Minute, slog.New(slog.NewTextHandler(&logs, nil)))
	self := os.Getpid() // exec читает /proc: нужен живой процесс
	old := time.Now().Add(-2 * time.Minute)
	tb.m[self] = &tomb{seen: old}
	tb.m[2] = &tomb{seen: old}
	tb.gone = func(pid int) (bool, error) {
		if pid == 2 {
			return false, errors.New("stat: permission denied")
		}
		tb.handle(execMsg(self)) // exec нового процесса с тем же pid во время проверки
		return true, nil
	}
	tb.expireOnce()
	if x := tb.m[self]; !x.exited.IsZero() || x.comm == "" {
		t.Errorf("pid %d: выход поставлен записи, которую за время проверки обновил exec (или exec не дошёл): %+v", self, *x)
	}
	tb.m[self].seen = old
	tb.gone = func(pid int) (bool, error) {
		if pid == 2 {
			return false, errors.New("stat: permission denied")
		}
		return false, nil
	}
	tb.expireOnce()
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("WARN об ошибке проверки: %d строк, ждали 1 за час:\n%s", n, logs.String())
	}
	tb.lastFailWarn = time.Now().Add(-time.Hour - time.Second)
	tb.expireOnce()
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Errorf("WARN через час: %d строк, ждали 2", n)
	}
}

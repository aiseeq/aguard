package track

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// Записи без события выхода (выход потерян, процесс в чужом пространстве
// имён) копились сотнями тысяч (тикет tmpguard-track-swap): старше keep и
// без процесса в /proc они снимаются, живые и свежие остаются.
func TestTombsExpireOpenWithoutProc(t *testing.T) {
	tb := newTombs(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	alive := map[int]bool{2: true}
	tb.alive = func(pid int) bool { return alive[pid] }
	old := time.Now().Add(-2 * time.Minute)
	tb.m[1] = &tomb{seen: old}                                           // нет процесса, старая — снять
	tb.m[2] = &tomb{seen: old}                                           // процесс жив — оставить
	tb.m[3] = &tomb{seen: time.Now()}                                    // свежая — оставить
	tb.m[4] = &tomb{seen: old, exited: time.Now()}                       // вышел недавно — оставить
	tb.m[5] = &tomb{seen: old, exited: time.Now().Add(-2 * time.Minute)} // вышел давно — снять
	tb.expireOnce()
	for pid, want := range map[int]bool{1: false, 2: true, 3: true, 4: true, 5: false} {
		if _, ok := tb.m[pid]; ok != want {
			t.Errorf("pid %d: запись есть=%v, ждали %v", pid, ok, want)
		}
	}
	if total, open := tb.size(); total != 3 || open != 2 {
		t.Errorf("size = %d, %d; ждали 3, 2", total, open)
	}
}

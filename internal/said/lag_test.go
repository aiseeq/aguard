package said

import (
	"strings"
	"testing"
	"time"
)

// Отставание указателя: старейшая реплика очереди, сколько ждёт, простой модели и его причина.
func TestStateLag(t *testing.T) {
	now := time.Date(2026, 11, 23, 2, 15, 0, 0, time.UTC)
	empty := (&State{Pass: now.Add(-time.Minute), Mode: ModeWrite}).Lag(now)
	if empty.Queue != 0 || empty.Behind != 0 || empty.Since != time.Minute || !strings.Contains(empty.String(), "очередь пуста") ||
		len(empty.Stale(30*time.Minute)) != 0 {
		t.Fatalf("%+v %q", empty, empty.String())
	}
	if why := empty.Stale(30 * time.Second); len(why) != 1 || !strings.Contains(why[0], "последний проход") {
		t.Fatalf("проход минуту назад при max 30s: %q", why)
	}
	if why := empty.Stale(time.Minute); len(why) != 0 {
		t.Fatalf("ровно max — не отставание: %q", why)
	}
	if why := (&State{}).Lag(now).Stale(time.Hour); len(why) != 1 || !strings.Contains(why[0], "не было") {
		t.Fatalf("нет прохода: %q", why)
	}
	zero := (&State{Pass: now, Mode: ModeWrite, Pending: []Pending{{Event: Event{At: now.Add(-time.Hour)}}, {}}}).Lag(now)
	if zero.Behind != time.Hour {
		t.Fatalf("реплика без времени последней в очереди спрятала отставание: %+v", zero)
	}
	st := &State{
		Pending: []Pending{
			{Event: Event{At: now.Add(-2 * time.Hour)}},
			{Event: Event{At: now.Add(-12 * time.Hour)}},
			{Event: Event{At: now.Add(-time.Hour)}},
		},
		DownSince: now.Add(-11 * time.Hour),
		Skip:      "work: работа запрещена сервером; personal: used 97 ≥ 90",
	}
	st.Pass, st.Mode = now.Add(-2*time.Hour), ModeWrite
	l := st.Lag(now)
	if why := l.Stale(time.Hour); len(why) != 2 {
		t.Fatalf("проход 2 ч назад и реплика 12 ч — две причины: %q", why)
	}
	if why := l.Stale(0); why != nil {
		t.Fatalf("без max — без причин: %q", why)
	}
	if l.Queue != 3 || !l.Oldest.Equal(now.Add(-12*time.Hour)) || l.Behind != 12*time.Hour || !l.DownSince.Equal(st.DownSince) {
		t.Fatalf("%+v", l)
	}
	s := l.String()
	for _, want := range []string{"в очереди 3", "ждёт 12h0m0s", "модель не отвечает", "personal: used 97"} {
		if !strings.Contains(s, want) {
			t.Fatalf("нет %q в %q", want, s)
		}
	}
	future := (&State{Pass: now.Add(time.Minute), Pending: []Pending{{Event: Event{At: now.Add(time.Minute)}}}}).Lag(now)
	if future.Behind != 0 || future.Since != 0 {
		t.Fatalf("реплика из будущего (сдвиг часов) — не отставание: %+v", future)
	}
}

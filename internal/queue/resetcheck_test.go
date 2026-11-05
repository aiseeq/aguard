package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	waited = "waited@example.com"
	active = "active@example.com"
)

// Строки живой limits-history.jsonl той ночи: у 28.10 01:00 и 31.10 07:28 один сброс в формах Z и +01:00.
const nightHistory = `{"at":"2026-10-24T16:39:11+02:00","sevenDayUsedPercentage":99,"sevenDayResetsAt":"2026-10-28T01:00:00+01:00","account":"waited@example.com"}
{"at":"2026-10-28T01:00:00+01:00","sevenDayUsedPercentage":0,"sevenDayResetsAt":"2026-11-04T00:00:00Z","account":"waited@example.com"}
{"at":"2026-10-31T07:28:26+01:00","sevenDayUsedPercentage":100,"sevenDayResetsAt":"2026-11-04T01:00:00+01:00","account":"waited@example.com"}
{"at":"2026-11-03T20:18:44+01:00","sevenDayUsedPercentage":97,"sevenDayResetsAt":"2026-11-09T11:00:00+01:00","account":"active@example.com"}
{"at":"2026-11-03T23:53:40+01:00","sevenDayUsedPercentage":99,"sevenDayResetsAt":"2026-11-09T11:00:00+01:00","account":"active@example.com"}
{"at":"2026-11-04T01:00:01+01:00","sevenDayUsedPercentage":0,"sevenDayResetsAt":"2026-11-11T00:00:00Z","account":"waited@example.com"}
`

func clock(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// history — строки файла на момент now: будущих строк в файле тогда ещё не было.
func history(t *testing.T, body string, now time.Time) []Row {
	t.Helper()
	p := filepath.Join(t.TempDir(), "limits-history.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []Row
	for _, r := range rows {
		if !r.At.After(now) {
			out = append(out, r)
		}
	}
	return out
}

func check(t *testing.T, body, account, since, at string) ResetCheck {
	t.Helper()
	now := clock(t, at)
	got, err := CheckReset(history(t, body, now), account, clock(t, since), now)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func hasAll(t *testing.T, got ResetCheck, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got.Reason, w) {
			t.Errorf("в причине нет %q:\n%s", w, got.Reason)
		}
	}
}

const pausedAt = "2026-11-03T20:22:11+01:00"

// Случай той ночи: письма 00:03 — за 57 минут до сброса named-учётки, таймер 01:04 — уже после.
func TestCheckResetThatNight(t *testing.T) {
	early := check(t, nightHistory, waited, pausedAt, "2026-11-04T00:03:09+01:00")
	if early.Confirmed {
		t.Fatalf("00:03 до сброса: %s", early.Reason)
	}
	hasAll(t, early, "рано", waited, "сброс 2026-11-04T01:00:00+01:00", "через 57 мин", "расход 100 %")

	late := check(t, nightHistory, waited, pausedAt, "2026-11-04T01:04:00+01:00")
	if !late.Confirmed {
		t.Fatalf("01:04 после сброса: %s", late.Reason)
	}
	hasAll(t, late, "сброс подтверждён", "окно до 2026-11-04T01:00:00+01:00", "текущее окно до 2026-11-11T01:00:00+01:00", "расход 0 %")
}

// Сценарий 3: активная в ту ночь active (её и показывал верхний sevenDayResetsAt) сбрасывается 09.11 — ответ по названной
// учётке, и в причине видна другая учётка с более близким сбросом.
func TestCheckResetNamedAccountOnly(t *testing.T) {
	got := check(t, nightHistory, active, pausedAt, "2026-11-04T00:03:09+01:00")
	if got.Confirmed {
		t.Fatalf("active до сброса: %s", got.Reason)
	}
	hasAll(t, got, active+", сброс 2026-11-09T11:00:00+01:00", "другая учётка "+waited+": сброс 2026-11-04T01:00:00+01:00")

	// Сброс waited в 01:00 не снимает паузу, поставленную на active.
	if got = check(t, nightHistory, active, pausedAt, "2026-11-04T01:05:00+01:00"); got.Confirmed {
		t.Fatalf("active после сброса другой учётки: %s", got.Reason)
	}
}

// Сценарий 1: прошлая смена окна (28.10) до постановки паузы не считается; момент постановки раньше неё — буквальный
// ответ «сброс был», и причина называет какой.
func TestCheckResetBeforePauseDoesNotCount(t *testing.T) {
	if got := check(t, nightHistory, waited, pausedAt, "2026-11-03T20:22:30+01:00"); got.Confirmed {
		t.Fatalf("смена окна 28.10 до паузы: %s", got.Reason)
	}
	got := check(t, nightHistory, waited, "2026-10-27T12:00:00+01:00", "2026-11-03T20:22:30+01:00")
	if !got.Confirmed {
		t.Fatalf("смена окна 28.10 после такого момента: %s", got.Reason)
	}
	hasAll(t, got, "окно до 2026-10-28T01:00:00+01:00")
}

// Сценарий 2: одно время сброса в формах Z и +01:00 — то же окно: 31.10 после постановки паузы — не смена.
func TestCheckResetSameInstantOtherZone(t *testing.T) {
	if got := check(t, nightHistory, waited, "2026-10-30T00:00:00+01:00", "2026-11-04T00:03:09+01:00"); got.Confirmed {
		t.Fatalf("тот же сброс в другой форме: %s", got.Reason)
	}
}

// Сценарий 4: неиспользованное окно: 0 % → 0 %, сброс всё равно был.
func TestCheckResetUnusedWindow(t *testing.T) {
	const body = `{"at":"2026-10-31T09:00:00+01:00","sevenDayUsedPercentage":0,"sevenDayResetsAt":"2026-11-07T08:00:00Z","account":"g"}
{"at":"2026-11-07T09:00:05+01:00","sevenDayUsedPercentage":0,"sevenDayResetsAt":"2026-11-14T08:00:00Z","account":"g"}
`
	if got := check(t, body, "g", "2026-11-05T12:00:00+01:00", "2026-11-07T09:01:00+01:00"); !got.Confirmed {
		t.Fatalf("окно сменилось при 0 %%: %s", got.Reason)
	}
}

// Сценарий 5: время на часах — не факт: без новой строки отказ, со строкой проходит.
func TestCheckResetNoNewSample(t *testing.T) {
	cut := nightHistory[:strings.LastIndex(strings.TrimSuffix(nightHistory, "\n"), "\n")+1]
	got := check(t, cut, waited, pausedAt, "2026-11-04T06:00:00+01:00")
	if got.Confirmed {
		t.Fatalf("без строки нового окна: %s", got.Reason)
	}
	hasAll(t, got, "нового замера нет", "прошло 300 мин назад", "последний замер 2026-10-31T07:28:26+01:00")

	if got = check(t, nightHistory, waited, pausedAt, "2026-11-04T06:00:00+01:00"); !got.Confirmed {
		t.Fatalf("со строкой нового окна: %s", got.Reason)
	}
}

// Окно кончилось до постановки паузы и замерено после неё: лимит свободен, ждать ещё неделю незачем.
func TestCheckResetObservedAfterPause(t *testing.T) {
	const body = `{"at":"2026-10-20T10:00:00+02:00","sevenDayUsedPercentage":100,"sevenDayResetsAt":"2026-10-24T10:00:00+02:00","account":"g"}
{"at":"2026-10-26T10:00:00+01:00","sevenDayUsedPercentage":0,"sevenDayResetsAt":"2026-10-31T09:00:00+01:00","account":"g"}
`
	if got := check(t, body, "g", "2026-10-25T12:00:00+01:00", "2026-10-26T10:01:00+01:00"); !got.Confirmed {
		t.Fatalf("первая строка нового окна после паузы: %s", got.Reason)
	}
}

// Сценарий 7: короткое имя вместо почты — ошибка входа со списком учёток, а не «рано».
func TestCheckResetUnknownAccount(t *testing.T) {
	now := clock(t, "2026-11-04T01:04:00+01:00")
	_, err := CheckReset(history(t, nightHistory, now), "waited", clock(t, pausedAt), now)
	if err == nil || !strings.Contains(err.Error(), waited) || !strings.Contains(err.Error(), active) {
		t.Fatalf("нужна ошибка со списком учёток, получено %v", err)
	}
}

// Две смены окна после паузы: причина называет первую, а текущим окном — последнее.
func TestCheckResetTwoWindowChanges(t *testing.T) {
	got := check(t, nightHistory, waited, "2026-10-27T12:00:00+01:00", "2026-11-04T01:04:00+01:00")
	if !got.Confirmed {
		t.Fatalf("две смены окна: %s", got.Reason)
	}
	hasAll(t, got, "окно до 2026-10-28T01:00:00+01:00 сменилось", "текущее окно до 2026-11-11T01:00:00+01:00")
}

// Строка нового окна в ту же секунду, что и постановка паузы, замерена не после неё; секундой раньше паузы — тем более.
func TestCheckResetSinceBoundary(t *testing.T) {
	if got := check(t, nightHistory, waited, "2026-11-04T01:00:01+01:00", "2026-11-04T01:04:00+01:00"); got.Confirmed {
		t.Fatalf("строка в момент постановки: %s", got.Reason)
	}
	if got := check(t, nightHistory, waited, "2026-11-04T01:00:00+01:00", "2026-11-04T01:04:00+01:00"); !got.Confirmed {
		t.Fatalf("строка через секунду после постановки: %s", got.Reason)
	}
}

// Время сброса сдвинулось назад — это не следующее окно: факта сброса нет.
func TestCheckResetWindowMovedBack(t *testing.T) {
	const body = `{"at":"2026-11-01T10:00:00+01:00","sevenDayUsedPercentage":40,"sevenDayResetsAt":"2026-11-07T09:00:00+01:00","account":"g"}
{"at":"2026-11-02T10:00:00+01:00","sevenDayUsedPercentage":41,"sevenDayResetsAt":"2026-11-06T09:00:00+01:00","account":"g"}
`
	if got := check(t, body, "g", "2026-11-01T12:00:00+01:00", "2026-11-02T11:00:00+01:00"); got.Confirmed {
		t.Fatalf("сдвиг назад: %s", got.Reason)
	}
}

func TestCheckResetSinceInFuture(t *testing.T) {
	now := clock(t, "2026-11-04T00:03:00+01:00")
	if _, err := CheckReset(history(t, nightHistory, now), waited, now.Add(time.Minute), now); err == nil {
		t.Fatal("момент постановки паузы в будущем: нужна ошибка")
	}
}

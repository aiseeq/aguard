package queue

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// ResetCheck — итог сверки: сбросился ли недельный лимит учётки после постановки паузы.
type ResetCheck struct {
	Confirmed bool
	Reason    string // одна-две строки для лида: что подтвердилось или почему снимать рано
}

// CheckReset сверяет снятие паузы «до сброса лимита» с историей: факт сброса — строка следующего окна названной
// учётки, замеренная после since (момент постановки паузы). Строка пишется из ответа API провайдера через секунды
// после сброса; время на часах фактом не считается (04.11 00:03: письма по таймеру за 57 минут до сброса, tl d808eda8:266).
// Закреп времени сброса здесь не годится: старое время даёт ложное «можно» посреди следующего окна, а время из слов
// повторяет ошибку той ночи. rows — из ReadHistory, по возрастанию At.
func CheckReset(rows []Row, account string, since, now time.Time) (ResetCheck, error) {
	if since.After(now) {
		return ResetCheck{}, fmt.Errorf("момент постановки паузы %s позже текущего %s", stamp(since, now), stamp(now, now))
	}
	var last, passed *Row
	for i := range rows {
		r := &rows[i]
		if r.Account != account {
			continue
		}
		if passed == nil && last != nil && r.At.After(since) && r.ResetsAt.After(last.ResetsAt) {
			passed = last
		}
		last = r
	}
	if last == nil {
		return ResetCheck{}, fmt.Errorf("нет строк учётки %q; известные: %s", account, strings.Join(Accounts(rows, now, 0), ", "))
	}
	if passed != nil {
		return ResetCheck{Confirmed: true, Reason: fmt.Sprintf("сброс подтверждён: %s, окно до %s сменилось; текущее окно до %s, расход %.0f %% (замер %s)",
			account, stamp(passed.ResetsAt, now), stamp(last.ResetsAt, now), last.Used, stamp(last.At, now))}, nil
	}
	reason := fmt.Sprintf("рано: %s, сброс %s, через %s, расход %.0f %% (замер %s)",
		account, stamp(last.ResetsAt, now), minutes(last.ResetsAt.Sub(now)), last.Used, stamp(last.At, now))
	if !last.ResetsAt.After(now) {
		reason = fmt.Sprintf("нового замера нет: %s, время сброса %s прошло %s назад, последний замер %s, расход %.0f %%; факта сброса в истории нет",
			account, stamp(last.ResetsAt, now), minutes(now.Sub(last.ResetsAt)), stamp(last.At, now), last.Used)
	}
	return ResetCheck{Reason: reason + otherAccounts(rows, account, now)}, nil
}

// otherAccounts — последние окна остальных учёток: при постановке паузы видно, чей сброс ближе (той ночью активная учётка
// сбрасывалась 09.11, а ждали другую, 04.11).
func otherAccounts(rows []Row, account string, now time.Time) string {
	latest := map[string]Row{}
	for _, r := range rows { // по возрастанию At: остаётся последняя строка учётки
		if r.Account != account {
			latest[r.Account] = r
		}
	}
	var out strings.Builder
	for _, name := range slices.Sorted(maps.Keys(latest)) {
		r := latest[name]
		fmt.Fprintf(&out, "\nдругая учётка %s: сброс %s, расход %.0f %% (замер %s)", name, stamp(r.ResetsAt, now), r.Used, stamp(r.At, now))
	}
	return out.String()
}

func stamp(t, now time.Time) string { return t.In(now.Location()).Format(time.RFC3339) }

func minutes(d time.Duration) string {
	return fmt.Sprintf("%d мин", int(d.Round(time.Minute)/time.Minute))
}

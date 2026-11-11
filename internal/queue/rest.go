package queue

import (
	"fmt"
	"slices"
	"time"

	"github.com/rickar/cal/v2"
	"github.com/rickar/cal/v2/cy"
)

// Rest — дни отдыха папок Roots: суббота, воскресенье и праздники Holidays, день — по дате в поясе Zone. В такой день
// в этих папках из новой работы идёт только CRIT: ворота закрывают HIGH, будильник и «HIGH не взят» ждут первого
// рабочего дня (пользователь 10.11: «Праздники и выходные я имел в виду»). Нулевое значение — дней отдыха нет.
type Rest struct {
	Roots    []string
	Zone     *time.Location
	Holidays []*cal.Holiday
}

// restCalendars — календари праздников queue.rest_holidays.
var restCalendars = map[string][]*cal.Holiday{"CY": cyHolidays}

// cyHolidays — календарь Кипра rickar/cal (14 дней) и два официальных дня, которых в нём нет: пасхальный вторник —
// в списке банковских выходных Центробанка Кипра, сочельник — в списке госслужбы (CySEC); сверка 11.11.2026, тикет
// cy-holidays-official-coverage.
var cyHolidays = append(slices.Clone(cy.Holidays),
	&cal.Holiday{Name: "Τρίτη του Πάσχα", Type: cal.ObservancePublic, Func: cal.CalcEasterOffset, Julian: true, Offset: 2},
	&cal.Holiday{Name: "Παραμονή Χριστουγέννων", Type: cal.ObservancePublic, Month: time.December, Day: 24, Func: cal.CalcDayOfMonth},
)

// Day — причина отдыха дня now в поясе Zone; пусто — рабочий день или папок отдыха нет.
func (r Rest) Day(now time.Time) string {
	if len(r.Roots) == 0 {
		return ""
	}
	local := now.In(r.Zone)
	for _, h := range r.Holidays {
		if _, observed := h.Calc(local.Year()); sameDate(observed, local) {
			return "праздник: " + h.Name
		}
	}
	switch local.Weekday() {
	case time.Saturday:
		return "суббота"
	case time.Sunday:
		return "воскресенье"
	}
	return ""
}

// Covers — день now — день отдыха, и папка dir лежит в одном из Roots.
func (r Rest) Covers(dir string, now time.Time) bool {
	return slices.ContainsFunc(r.Roots, func(root string) bool { return underRoot(dir, root) }) && r.Day(now) != ""
}

// End — 00:00 в поясе Zone первого рабочего дня после now: одно значение на всю цепочку дней отдыха (суббота,
// воскресенье и праздник следом), поэтому state.json за эти дни не переписывается.
func (r Rest) End(now time.Time) time.Time {
	local := now.In(r.Zone)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, r.Zone)
	for r.Day(day) != "" {
		day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, r.Zone)
	}
	return day
}

func sameDate(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

// NewRest — дни отдыха из конфига: пояс и календарь обязательны при непустых корнях, неизвестное имя — ошибка.
func NewRest(roots []string, zone, holidays string) (Rest, error) {
	if len(roots) == 0 {
		if zone != "" || holidays != "" {
			return Rest{}, fmt.Errorf("queue.rest_zone и rest_holidays без rest_crit ничего не значат")
		}
		return Rest{}, nil
	}
	if zone == "" {
		return Rest{}, fmt.Errorf("queue.rest_zone: пояс дней отдыха обязателен при rest_crit (например Europe/Nicosia)")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return Rest{}, fmt.Errorf("queue.rest_zone %q: %w", zone, err)
	}
	days, ok := restCalendars[holidays]
	if !ok {
		return Rest{}, fmt.Errorf("queue.rest_holidays %q: известен только CY", holidays)
	}
	return Rest{Roots: roots, Zone: loc, Holidays: days}, nil
}

package disk

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/record"
)

// Прогноз запаса (тикет disk-alert-forecast, замысел docs/design.md: «диск кончится через 9 дней при нынешнем росте»).
// Скорость — по недавнему окну forecast_window, не за сутки: ручная очистка в сутках прячет нынешнюю убыль (сценарий 6
// противника 10.11). Прогноз есть, только если убыль идёт и за последний forecast_recent: разовое скачивание, после
// которого место стоит, — не темп (сценарий 7).

// seriesStep — точка ряда не чаще: свободное место меряется раз в минуту, а прогнозу по окну в часы хватает пяти минут;
// файл ряда пишется не чаще точки.
const seriesStep = 5 * time.Minute

// forecastNoise — убыль за последний forecast_recent меньше этого — не «убыль идёт»: свободное место btrfs гуляет на
// десятки мегабайт и без записи (метаданные, отложенное освобождение). 256 МиБ/ч — около 6 ГБ/сут, медленнее прогноз
// всё равно дальше forecast_within.
const forecastNoise = 256 << 20

// ForecastGrew — grew-находки в письме прогноза — за столько: при walk = 24h это два последних обхода.
const ForecastGrew = 48 * time.Hour

// ForecastState — forecast.json: ряд свободного места за forecast_window, последний вывод (строка журнала — при его
// смене) и отметки писем. Свой файл: ряд должен пережить перезапуск демона, а обход может быть брошен остановкой.
type ForecastState struct {
	Series []Point  `json:"series"`
	Last   string   `json:"last,omitempty"` // ключ вывода: "" — прогноза нет, иначе целые сутки до free_min
	Told   FreeTold `json:"told,omitempty"` // адресат -> когда пора следующее письмо
}

// LoadForecast — состояние прогноза; файла нет — пусто.
func LoadForecast(path string) (*ForecastState, error) {
	f := &ForecastState{}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("прогноз disk: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(b, f); err != nil {
			return nil, fmt.Errorf("прогноз disk %s: %w", path, err)
		}
	}
	if f.Told == nil {
		f.Told = FreeTold{}
	}
	return f, nil
}

// Save пишет состояние атомарно.
func (f *ForecastState) Save(path string) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("прогноз disk: %w", err)
	}
	if err := record.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("прогноз disk: %w", err)
	}
	return nil
}

// Add — точка ряда, если прошлой нет или она старше seriesStep; точки старше окна — прочь. true — ряд изменился.
func (f *ForecastState) Add(c *Config, now time.Time, free int64) bool {
	n := len(f.Series)
	f.Series = slices.DeleteFunc(f.Series, func(p Point) bool { return now.Sub(p.At) > c.ForecastWindow.D() })
	changed := len(f.Series) != n
	if len(f.Series) == 0 || now.Sub(f.Series[len(f.Series)-1].At) >= seriesStep {
		f.Series = append(f.Series, Point{At: now, Bytes: free})
		changed = true
	}
	return changed
}

// Forecast — вывод прогноза: темп убыли (байт в час), по какому отрезку, сколько до free_min и до нуля.
type Forecast struct {
	Rate          float64
	Span          time.Duration
	Free          int64
	ToMin, ToZero time.Duration
}

// Predict — прогноз по ряду и нынешнему free: скорость — от самой ранней точки окна до сейчас; прогноз есть, только если
// место за окно убыло и за последний forecast_recent убыло не меньше forecastNoise.
func (c *Config) Predict(series []Point, now time.Time, free int64) (Forecast, bool) {
	var start, recent *Point
	for i := range series {
		p := &series[i]
		age := now.Sub(p.At)
		if age > c.ForecastWindow.D() || age <= 0 {
			continue
		}
		if start == nil || p.At.Before(start.At) {
			start = p
		}
		if age >= c.ForecastRecent.D() && (recent == nil || p.At.After(recent.At)) {
			recent = p
		}
	}
	if start == nil || recent == nil {
		return Forecast{}, false
	}
	drop := start.Bytes - free
	if drop <= 0 || recent.Bytes-free < forecastNoise {
		return Forecast{}, false
	}
	span := now.Sub(start.At)
	rate := float64(drop) / span.Hours()
	hours := func(b int64) time.Duration {
		return time.Duration(float64(max(b, 0)) / rate * float64(time.Hour))
	}
	return Forecast{Rate: rate, Span: span, Free: free, ToMin: hours(free - int64(c.FreeMin)), ToZero: hours(free)}, true
}

// ForecastKey — ключ вывода для строки журнала: "" — прогноза нет, иначе целые сутки до free_min.
func ForecastKey(fc Forecast, ok bool) string {
	if !ok {
		return ""
	}
	return strconv.Itoa(int(fc.ToMin / (24 * time.Hour)))
}

// Warn — прогноз зовёт письмо: до free_min меньше forecast_within, а порог ещё не пересечён (там — письмо free_min).
func (c *Config) Warn(fc Forecast, ok bool) bool {
	return ok && fc.Free >= int64(c.FreeMin) && fc.ToMin < c.ForecastWithin.D()
}

// ForecastLine — вывод прогноза строкой журнала.
func (c *Config) ForecastLine(fc Forecast, ok bool) string {
	if !ok {
		return fmt.Sprintf("прогноза нет: за %s место не убывает или за последний %s убыль меньше %s", policy.HumanAge(c.ForecastWindow.D()),
			policy.HumanAge(c.ForecastRecent.D()), policy.HumanBytes(forecastNoise))
	}
	return fmt.Sprintf("прогноз: убыль %s (за %s), %s до %s (free_min), до нуля %s", rateText(fc.Rate), policy.HumanAge(fc.Span),
		about(fc.ToMin), policy.HumanBytes(int64(c.FreeMin)), about(fc.ToZero))
}

// ForecastMessage — письмо прогноза: темп, часы или сутки до порога и до нуля, grew-находки последних обходов с временем.
func (c *Config) ForecastMessage(fc Forecast, grew []Record, home string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "aguard disk: на /home свободно %s и убывает %s (за %s, убыль идёт и в последний %s): %s до %s (free_min), "+
		"кончится примерно через %s.", policy.HumanBytes(fc.Free), rateText(fc.Rate), policy.HumanAge(fc.Span),
		policy.HumanAge(c.ForecastRecent.D()), about(fc.ToMin), policy.HumanBytes(int64(c.FreeMin)), about(fc.ToZero))
	grew = slices.Clone(grew)
	sort.Slice(grew, func(i, j int) bool {
		return cmp.Or(cmp.Compare(grew[j].Delta, grew[i].Delta), grew[j].At.Compare(grew[i].At)) < 0
	})
	if len(grew) == 0 {
		fmt.Fprintf(&b, "\nРоста записей на grow за %s обходы не нашли: место ест не одна запись (или обхода не было).",
			policy.HumanAge(ForecastGrew))
	} else {
		fmt.Fprintf(&b, "\nРост записей по обходам за %s:", policy.HumanAge(ForecastGrew))
		for i := range grew[:min(5, len(grew))] {
			r := &grew[i]
			fmt.Fprintf(&b, "\n- %s %s (%s)", r.At.Local().Format("02.01 15:04"), r.What(home), r.Owner)
		}
		if len(grew) > 5 {
			fmt.Fprintf(&b, "\n- и ещё %d", len(grew)-5)
		}
	}
	fmt.Fprintf(&b, "\nПовтор — раз в сутки, пока до порога меньше %s.", about(c.ForecastWithin.D()))
	return b.String()
}

// rateText — темп: от 1 ГБ в час — в час, медленнее — в сутки.
func rateText(perHour float64) string {
	if perHour >= 1<<30 {
		return policy.HumanBytes(int64(perHour)) + "/ч"
	}
	return policy.HumanBytes(int64(perHour*24)) + "/сут"
}

// about — срок: меньше двух суток — часами, дальше — сутками.
func about(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("~%d ч", int(math.Round(d.Hours())))
	}
	return fmt.Sprintf("~%d сут", int(math.Round(d.Hours()/24)))
}

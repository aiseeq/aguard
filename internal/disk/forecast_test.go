package disk

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/policy"
)

// series — ряд за сутки до now через 5 мин: f(возраст) — свободное место.
func series(now time.Time, f func(age time.Duration) int64) []Point {
	var out []Point
	for age := 24 * time.Hour; age > 0; age -= seriesStep {
		out = append(out, Point{At: now.Add(-age), Bytes: f(age)})
	}
	return out
}

const gib = float64(1 << 30)

// Сценарий 6 противника 10.11: очистка подняла свободное 200→260 ГБ за сутки, но последние 6 ч убыль 10 ГБ/ч —
// прогноз по окну 6 ч: «~11 ч до 150 ГБ», письмо.
func TestForecastRecentWindowAfterCleanup(t *testing.T) {
	c := Default()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	pts := series(now, func(age time.Duration) int64 {
		if age > 6*time.Hour {
			return 200 << 30
		}
		return int64((320 - 10*(6-age.Hours())) * gib)
	})
	fc, ok := c.Predict(pts, now, 260<<30)
	if !ok || fc.Rate < 9.99*gib || fc.Rate > 10.01*gib {
		t.Fatalf("прогноз: %+v %v, ждал 10 ГБ/ч", fc, ok)
	}
	if line := c.ForecastLine(fc, ok); !strings.Contains(line, "~11 ч до 150 ГБ") || !strings.Contains(line, "10 ГБ/ч") {
		t.Fatalf("строка: %s", line)
	}
	if !c.Warn(fc, ok) || ForecastKey(fc, ok) != "0" {
		t.Fatalf("до порога 11 ч — письмо и ключ 0 суток: warn %v key %q", c.Warn(fc, ok), ForecastKey(fc, ok))
	}
	msg := c.ForecastMessage(fc, []Record{{Kind: KindGrew, At: now.Add(-6 * time.Hour), Path: "/home/u/.cache/go-build",
		Bytes: 165 << 30, Delta: 44 << 30, Since: now.Add(-30 * time.Hour), Owner: "lead"}}, "/home/u")
	if !strings.Contains(msg, "~11 ч до 150 ГБ") || !strings.Contains(msg, "10.11 04:00 ~/.cache/go-build выросла на 44 ГБ") {
		t.Fatalf("письмо:\n%s", msg)
	}
}

// Сценарий 7: разовое −100 ГБ, потом ровно — прогноза нет; то же, когда ступенька внутри окна 6 ч: в последний час
// убыли нет.
func TestForecastStepThenFlat(t *testing.T) {
	c := Default()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	for _, step := range []time.Duration{23*time.Hour + 55*time.Minute, 3 * time.Hour} {
		pts := series(now, func(age time.Duration) int64 {
			if age > step {
				return 350 << 30
			}
			return 250 << 30
		})
		if fc, ok := c.Predict(pts, now, 250<<30); ok {
			t.Fatalf("ступенька %s назад: прогноз %+v", step, fc)
		}
	}
}

// Очистка внутри окна: за 6 ч место выросло, хотя последний час убывает, — прогноза нет (скорость — по окну).
func TestForecastWindowRiseNoForecast(t *testing.T) {
	c := Default()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	pts := series(now, func(age time.Duration) int64 {
		if age > 2*time.Hour {
			return 200 << 30
		}
		return 260 << 30
	})
	if fc, ok := c.Predict(pts, now, 250<<30); ok {
		t.Fatalf("за окно место выросло на 50 ГБ, а прогноз есть: %+v", fc)
	}
}

// Убыль за последний час — от forecastNoise; меньше — не «убыль идёт».
func TestForecastRecentNoise(t *testing.T) {
	c := Default()
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		drop int64
		want bool
	}{{forecastNoise, true}, {forecastNoise - 1, false}} {
		pts := series(now, func(age time.Duration) int64 {
			if age >= time.Hour {
				return 300<<30 + tc.drop
			}
			return 300 << 30
		})
		if _, ok := c.Predict(pts, now, 300<<30); ok != tc.want {
			t.Errorf("убыль за час %d: прогноз %v, ждал %v", tc.drop, ok, tc.want)
		}
	}
}

// Отклонённый сценарий 1 (замысел): равномерная убыль 15,3 ГБ/сут при 275 ГБ — прогноз ~8 сут, письма нет.
func TestForecastSlowNoWarn(t *testing.T) {
	c := Default()
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.Local)
	pts := series(now, func(age time.Duration) int64 { return int64((275 + 15.3*age.Hours()/24) * gib) })
	fc, ok := c.Predict(pts, now, 275<<30)
	if !ok || ForecastKey(fc, ok) != "8" || c.Warn(fc, ok) {
		t.Fatalf("прогноз %+v %v ключ %q warn %v — ждал 8 суток без письма", fc, ok, ForecastKey(fc, ok), c.Warn(fc, ok))
	}
}

// Приёмка (2) тикета: убыль 68 ГБ/сут при 290 ГБ — «кончится примерно через 4 суток, до free_min — около 2»; письмо —
// когда до порога меньше forecast_within (2,06 сут: при 48 ч нет, при 72 ч есть).
func TestForecastAcceptanceTwo(t *testing.T) {
	c := Default()
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.Local)
	pts := series(now, func(age time.Duration) int64 { return int64((290 + 68*age.Hours()/24) * gib) })
	fc, ok := c.Predict(pts, now, 290<<30)
	msg := c.ForecastMessage(fc, nil, "/home/u")
	if !ok || !strings.Contains(msg, "~2 сут до 150 ГБ") || !strings.Contains(msg, "кончится примерно через ~4 сут") {
		t.Fatalf("письмо:\n%s", msg)
	}
	if c.Warn(fc, ok) {
		t.Fatal("2,06 сут не меньше 48 ч — письма нет")
	}
	c.ForecastWithin = policy.Duration(72 * time.Hour)
	if !c.Warn(fc, ok) {
		t.Fatal("при forecast_within 72 ч — письмо")
	}
	fc.Free = 149 << 30
	if c.Warn(fc, ok) {
		t.Fatal("ниже free_min — письмо free_min, не прогноз")
	}
}

// Ряд: точка не чаще 5 мин, старше окна — прочь, переживает перезапуск (файл).
func TestForecastSeriesPersists(t *testing.T) {
	c := Default()
	path := filepath.Join(t.TempDir(), "forecast.json")
	f, err := LoadForecast(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)
	if !f.Add(&c, t0, 300<<30) || f.Add(&c, t0.Add(seriesStep-time.Second), 299<<30) || !f.Add(&c, t0.Add(seriesStep), 298<<30) {
		t.Fatalf("шаг ряда: %v", f.Series)
	}
	f.Last = "3"
	f.Told.Mark("lead", t0, true)
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	g, err := LoadForecast(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Series) != 2 || g.Last != "3" || !g.Told["lead"].Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("после перезапуска: %+v", g)
	}
	g.Add(&c, t0.Add(6*time.Hour+time.Second), 290<<30)
	if len(g.Series) != 2 || !g.Series[0].At.Equal(t0.Add(seriesStep)) {
		t.Fatalf("точка старше окна осталась: %v", g.Series)
	}
}

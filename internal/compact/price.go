package compact

import (
	"math"
	"strings"

	"github.com/aiseeq/akit/price"
)

// cost — во что обойдётся контекст tokens модели model, в миллионных долях доллара: rewrite — следующий запрос после
// смерти кэша пишет его заново (запись на час, её пишет Claude Code), read — каждый запрос при живом кэше читает его.
// Цены — общая таблица akit/price, это оценка, не счёт. Модель не из таблицы — нули.
func cost(model string, tokens int64) (rewrite, read int64) {
	model = strings.TrimSuffix(model, "[1m]")
	w, okW := price.Cost(model, price.Usage{CacheWrite: tokens, Write1h: tokens})
	r, okR := price.Cost(model, price.Usage{CacheRead: tokens})
	if !okW || !okR {
		return 0, 0
	}
	return int64(math.Round(w * 1e6)), int64(math.Round(r * 1e6))
}

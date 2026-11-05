package iowatch

import (
	"fmt"
	"time"
)

// Протокол сокета моментального состояния диска (Config.Socket): строка JSON запроса → строка JSON
// ответа, одно соединение — один запрос. Описание для клиентов — docs/design.md, абзац `io`.

// ProtoVersion — версия протокола; запрос другой версии получает ответ с Error.
const ProtoVersion = 1

// Запросы.
const (
	QueryNow      = "now"      // моментальное состояние (Now); kind — ещё свод профилей вида
	QuerySeries   = "series"   // посекундный ряд с since
	QueryProfiles = "profiles" // профили вида kind (пусто — все) с концом не раньше since
	QueryCases    = "cases"    // сводки случаев с концом не раньше since
)

// Request — запрос: `now`, `series`, `profiles` или `cases`; для последних трёх поддерживается `since` в RFC3339.
type Request struct {
	V     int    `json:"v"`
	Q     string `json:"q"`
	Since string `json:"since,omitempty"` // RFC3339; пусто — всё, что есть
	Kind  string `json:"kind,omitempty"`
}

// Answer — ответ: версия, Error или данные запроса. Поля Now — в корне ответа; нет поля series или
// profiles — пусто.
type Answer struct {
	V     int    `json:"v"`
	Error string `json:"error,omitempty"`
	*Now
	Series   []MachineSecond `json:"series,omitempty"`
	Profiles []Profile       `json:"profiles,omitempty"`
	Cases    []Case          `json:"cases,omitempty"`
}

// CheckRequest — версия и вид запроса; since — разобранное время (ноль — не задано).
func CheckRequest(req Request) (since time.Time, err error) {
	if req.V != ProtoVersion {
		return since, fmt.Errorf("версия протокола %d, служба знает %d", req.V, ProtoVersion)
	}
	switch req.Q {
	case QueryNow, QuerySeries, QueryProfiles, QueryCases:
	default:
		return since, fmt.Errorf("неизвестный запрос %q: now, series, profiles или cases", req.Q)
	}
	if req.Since != "" {
		if since, err = time.Parse(time.RFC3339, req.Since); err != nil {
			return since, fmt.Errorf("since %q: нужно RFC3339", req.Since)
		}
	}
	return since, nil
}

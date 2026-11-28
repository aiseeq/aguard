package iowatch

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Протокол сокета моментального состояния диска (Config.Socket): строка JSON запроса → строка JSON
// ответа, одно соединение — один запрос. Описание для клиентов — docs/design.md, абзац `io`.

// ProtoVersion — версия протокола; запрос другой версии получает ответ с Error.
const ProtoVersion = 1

// Запросы.
const (
	QueryNow      = "now"      // моментальное состояние (Now); kind от старых клиентов не читается
	QuerySeries   = "series"   // посекундный ряд с since
	QueryProfiles = "profiles" // профили вида kind (пусто — все) с концом не раньше since
	QueryCases    = "cases"    // сводки случаев с концом не раньше since и учёт покрытия замера
)

// Request — запрос: `now`, `series`, `profiles` или `cases`; для последних трёх поддерживается `since` в RFC3339.
type Request struct {
	V     int    `json:"v"`
	Q     string `json:"q"`
	Since string `json:"since,omitempty"` // RFC3339; пусто — всё, что есть
	Kind  string `json:"kind,omitempty"`
}

// Answer — ответ: версия, Error или данные запроса. Поля Now — в корне ответа; нет поля series, profiles или
// cases — пусто.
type Answer struct {
	V     int    `json:"v"`
	Error string `json:"error,omitempty"`
	*Now
	Series   []MachineSecondFileRow `json:"series,omitempty"`
	Profiles []Profile              `json:"profiles,omitempty"`
	Cases    []Case                 `json:"cases,omitempty"`
	// Coverage — учёт покрытия замера на момент ответа cases (всегда в ответе cases; нет — служба старее учёта).
	Coverage *CoverRecord `json:"coverage,omitempty"`
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

// CasesAnswer — ответ cases, который пишет себя по одному случаю (linesock зовёт StreamJSON): сводки за неделю —
// десятки МБ JSON, а json.Marshal всего ответа собирал их одним буфером — ступень пика памяти демона 99 МБ
// (rss-growth-1118). Байты те же, что у json.Marshal(Answer{V: ProtoVersion, Cases: …, Coverage: …}).
type CasesAnswer struct {
	Cases    []Case
	Coverage *CoverRecord
}

// StreamJSON пишет ответ в w без перевода строки.
func (a CasesAnswer) StreamJSON(w io.Writer) error {
	sw := &stickyWriter{w: w}
	sw.write(fmt.Appendf(nil, `{"v":%d`, ProtoVersion))
	if len(a.Cases) > 0 {
		sw.write([]byte(`,"cases":[`))
		for i := range a.Cases {
			b, err := json.Marshal(&a.Cases[i])
			if err != nil {
				return fmt.Errorf("случай %d: %w", i, err)
			}
			if i > 0 {
				sw.write([]byte(","))
			}
			sw.write(b)
			if sw.err != nil {
				break
			}
		}
		sw.write([]byte("]"))
	}
	if a.Coverage != nil && sw.err == nil {
		b, err := json.Marshal(a.Coverage)
		if err != nil {
			return fmt.Errorf("учёт покрытия: %w", err)
		}
		sw.write([]byte(`,"coverage":`))
		sw.write(b)
	}
	sw.write([]byte("}"))
	if sw.err != nil {
		return fmt.Errorf("запись ответа cases: %w", sw.err)
	}
	return nil
}

// stickyWriter запоминает первую ошибку записи; следующие записи после неё не идут.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) write(b []byte) {
	if s.err == nil {
		_, s.err = s.w.Write(b)
	}
}

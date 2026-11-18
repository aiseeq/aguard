package record

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// decodeState читает записи по одной, чтобы не буферизовать снимок целиком.
func decodeState(r io.Reader) (State, error) {
	var s State
	d := json.NewDecoder(r)
	tok, err := d.Token()
	if err != nil {
		return s, fmt.Errorf("начало снимка: %w", err)
	}
	if tok == nil {
		return s, stateEnd(d)
	}
	if tok != json.Delim('{') {
		return s, fmt.Errorf("снимок должен быть объектом")
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return s, fmt.Errorf("ключ снимка: %w", err)
		}
		name, ok := key.(string)
		if !ok {
			return s, fmt.Errorf("ключ снимка должен быть строкой")
		}
		if err := decodeStateField(d, &s, name); err != nil {
			return s, err
		}
	}
	if _, err := d.Token(); err != nil {
		return s, fmt.Errorf("конец снимка: %w", err)
	}
	return s, stateEnd(d)
}

func stateEnd(d *json.Decoder) error {
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		if err != nil {
			return fmt.Errorf("после снимка: %w", err)
		}
		return fmt.Errorf("лишние данные после снимка")
	}
	return nil
}

func decodeStateField(d *json.Decoder, s *State, key string) error {
	switch {
	case strings.EqualFold(key, "updated"):
		return d.Decode(&s.Updated)
	case strings.EqualFold(key, "started"):
		return d.Decode(&s.Started)
	case strings.EqualFold(key, "last_full"):
		return d.Decode(&s.LastFull)
	case strings.EqualFold(key, "roots"):
		return d.Decode(&s.Roots)
	case strings.EqualFold(key, "containers"):
		return d.Decode(&s.Containers)
	case strings.EqualFold(key, "entries"):
		return decodeEntries(d, s)
	default:
		var ignored json.RawMessage
		return d.Decode(&ignored)
	}
}

func decodeEntries(d *json.Decoder, s *State) error {
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("начало entries: %w", err)
	}
	if tok == nil {
		s.Entries = nil
		return nil
	}
	if tok != json.Delim('[') {
		return fmt.Errorf("entries должен быть массивом")
	}
	if !d.More() {
		s.Entries = []Entry{}
		_, err = d.Token()
		return err
	}
	if s.Entries == nil {
		s.Entries = []Entry{}
	} else {
		s.Entries = s.Entries[:0]
	}
	for d.More() {
		if len(s.Entries) < cap(s.Entries) {
			s.Entries = s.Entries[:len(s.Entries)+1]
		} else {
			s.Entries = append(s.Entries, Entry{})
		}
		if err := d.Decode(&s.Entries[len(s.Entries)-1]); err != nil {
			return fmt.Errorf("запись entries: %w", err)
		}
	}
	_, err = d.Token()
	return err
}

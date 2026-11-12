package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aiseeq/aguard/internal/record"
)

// quietHelp — справка aguard quiet: группы писем совпадают с разделом «Тихий режим» docs/spec.md.
const quietHelp = `aguard quiet — тишина писем сторожа сессиям на срок.

  aguard quiet                                         состояние
  aguard quiet on --until ВРЕМЯ --why ТЕКСТ --by ИМЯ   включить до ВРЕМЯ (RFC3339 или ГГГГ-ММ-ДД ЧЧ:ММ местного), не дальше 7 суток
  aguard quiet off                                     снять

В тишине:
  идут сквозь неё (аварии): sigguard — отказ сигнала и неразобранные строки; ceiling — снятие процессов потолком памяти
  ждут её конца и уходят обычным повтором: queue, night, cpu, tmp, codex, disk, said
  пропадают: io, bgtask, remote, orphans

busy --quiet — другое: придерживает тяжёлую работу, писем не глушит.
`

func (a *app) quietCmd(args []string) error {
	if len(args) == 0 {
		return a.quietStatus()
	}
	switch args[0] {
	case "on":
		return a.quietOn(args[1:])
	case "off":
		return a.quietOff(args[1:])
	case "-h", "--help", "help":
		_, err := fmt.Fprint(a.out, quietHelp)
		return err
	default:
		return errors.New("aguard quiet: ожидается on или off")
	}
}

func (a *app) quietStatus() error {
	state, active, err := a.readQuiet()
	if err != nil {
		return err
	}
	if !active {
		fmt.Fprintln(a.out, "громко")
		return nil
	}
	left := time.Until(state.Until).Round(time.Second)
	fmt.Fprintf(a.out, "тихо до %s (осталось %s): %s\n", state.Until.Local().Format("2006-01-02 15:04:05 MST"), left, state.Why)
	return nil
}

func (a *app) quietOn(args []string) error {
	fl := flag.NewFlagSet("quiet on", flag.ContinueOnError)
	fl.SetOutput(os.Stderr)
	untilArg := fl.String("until", "", "срок RFC3339 или локальное время ГГГГ-ММ-ДД ЧЧ:ММ")
	why := fl.String("why", "", "причина")
	by := fl.String("by", "", "имя сессии или человек, включивший режим")
	if err := fl.Parse(args); err != nil {
		return fmt.Errorf("aguard quiet on: %w", err)
	}
	if *untilArg == "" || strings.TrimSpace(*why) == "" || strings.TrimSpace(*by) == "" || fl.NArg() != 0 {
		return errors.New("aguard quiet on: нужны --until, --why и --by")
	}
	until, err := parseQuietUntil(*untilArg)
	if err != nil {
		return err
	}
	now := time.Now()
	if !until.After(now) {
		return errors.New("aguard quiet on: срок должен быть в будущем")
	}
	if until.After(now.Add(7 * 24 * time.Hour)) {
		return errors.New("aguard quiet on: срок не может быть дальше 7 суток")
	}
	state := quietState{Until: until, Why: *why, By: *by, At: now}
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("кодирование тихого режима: %w", err)
	}
	if err := os.MkdirAll(a.dir("quiet"), 0o700); err != nil {
		return fmt.Errorf("создание папки тихого режима: %w", err)
	}
	if err := record.WriteFileAtomic(a.quietPath(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "тихо до %s: %s\n", until.Local().Format("2006-01-02 15:04:05 MST"), *why)
	return nil
}

func parseQuietUntil(value string) (time.Time, error) {
	until, rfcErr := time.Parse(time.RFC3339, value)
	if rfcErr == nil {
		return until, nil
	}
	until, localErr := time.ParseInLocation("2006-01-02 15:04", value, time.Local)
	if localErr != nil {
		return time.Time{}, fmt.Errorf("aguard quiet on: неверный --until: %w", localErr)
	}
	return until, nil
}

func (a *app) quietOff(args []string) error {
	if len(args) != 0 {
		return errors.New("aguard quiet off: лишние аргументы")
	}
	if err := os.Remove(a.quietPath()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(a.out, "не был включён")
			return nil
		}
		return fmt.Errorf("удаление %s: %w", a.quietPath(), err)
	}
	fmt.Fprintln(a.out, "громко")
	return nil
}

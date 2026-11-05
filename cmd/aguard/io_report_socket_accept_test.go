package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/iowatch"
)

// Мера тикета io-report-rewrite: отчёт берёт случаи из памяти службы по сокету и ничего не заставляет писать.

var _ = iowatch.QueryCases

func acceptWaitSocket(t *testing.T, socket string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("сокет не открылся")
		}
	}
}

func acceptSummary(t *testing.T, raw []byte) iowatch.Summary {
	t.Helper()
	var sum iowatch.Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatalf("итог не JSON: %v: %s", err, raw)
	}
	return sum
}

func acceptCase(now time.Time, ago time.Duration, idle int64) iowatch.Case {
	start := now.Add(-ago)
	return iowatch.Case{Start: start, End: start.Add(time.Duration(idle) * time.Millisecond), DurMS: idle, IdleMS: idle, PeakMS: idle}
}

// Служба работает: случаи — из её памяти, а не из устаревшего файла; файлы не тронуты и не созданы.
func TestIOReportSocketAccept(t *testing.T) {
	s, out, socket := ioLiveService(t, t.Context())
	now := time.Now()
	s.cases = []iowatch.Case{acceptCase(now, time.Hour, 2500)}
	if err := iowatch.SaveCases(s.path, []iowatch.Case{acceptCase(now, 2*time.Hour, 9999)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	go s.serve()
	go s.wait(time.Minute) // горутина цикла: исполняет extra
	acceptWaitSocket(t, socket)

	if err := s.a.ioReport(s.path, true, now.Add(-24*time.Hour), now, 0, true); err != nil {
		t.Fatal(err)
	}
	if sum := acceptSummary(t, out.Bytes()); sum.Total != 1 || sum.IdleMS != 2500 {
		t.Fatalf("итог не из памяти службы: total %d, idle %d", sum.Total, sum.IdleMS)
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("отчёт заставил службу переписать файл сводок")
	}
	for _, path := range []string{s.seriesPath, s.profilesPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("отчёт заставил службу записать %s: %v", filepath.Base(path), err)
		}
	}
}

// Срок отчёта режет случаи и на стороне службы: старые в ответ не идут.
func TestIOReportSocketSinceAccept(t *testing.T) {
	s, out, socket := ioLiveService(t, t.Context())
	now := time.Now()
	s.cases = []iowatch.Case{acceptCase(now, 3*time.Hour, 4000), acceptCase(now, time.Hour, 2500)}
	go s.serve()
	go s.wait(time.Minute)
	acceptWaitSocket(t, socket)
	if err := s.a.ioLive(iowatch.Request{Q: iowatch.QueryCases, Since: now.Add(-2 * time.Hour).Format(time.RFC3339)}, true); err != nil {
		t.Fatal(err)
	}
	var ans iowatch.Answer
	if err := json.Unmarshal(out.Bytes(), &ans); err != nil {
		t.Fatal(err)
	}
	if len(ans.Cases) != 1 || ans.Cases[0].IdleMS != 2500 {
		t.Fatalf("ответ службы по since: %+v", ans.Cases)
	}
}

// Службы нет: файл как есть, без ошибки.
func TestIOReportSocketNoServiceAccept(t *testing.T) {
	s, out, _ := ioLiveService(t, t.Context())
	now := time.Now()
	if err := iowatch.SaveCases(s.path, []iowatch.Case{acceptCase(now, time.Hour, 3000)}); err != nil {
		t.Fatal(err)
	}
	if err := s.a.ioReport(s.path, true, now.Add(-24*time.Hour), now, 0, true); err != nil {
		t.Fatal(err)
	}
	if sum := acceptSummary(t, out.Bytes()); sum.Total != 1 || sum.IdleMS != 3000 {
		t.Fatalf("итог из файла: total %d, idle %d", sum.Total, sum.IdleMS)
	}
}

// Служба есть, но занята: ошибка, а не молчаливый откат к устаревшему файлу.
func TestIOReportSocketBusyAccept(t *testing.T) {
	s, _, socket := ioLiveService(t, t.Context())
	now := time.Now()
	if err := iowatch.SaveCases(s.path, []iowatch.Case{acceptCase(now, time.Hour, 3000)}); err != nil {
		t.Fatal(err)
	}
	go s.serve() // цикла нет: extra никто не исполняет
	acceptWaitSocket(t, socket)
	err := s.a.ioReport(s.path, true, now.Add(-24*time.Hour), now, 0, true)
	if err == nil || errors.Is(err, errIONotRunning) {
		t.Fatalf("занятая служба: %v", err)
	}
}

package main

import (
	"testing"

	"github.com/aiseeq/aguard/internal/config"
)

// Каждый наблюдатель демона есть в отчёте: иначе его сбои не видны (disk и said до 26.10).
func TestReportSpecsCoverWatchers(t *testing.T) {
	a := &app{}
	var cfg config.Config
	inReport := map[string]bool{}
	for _, s := range a.reportSpecs(&cfg) {
		inReport[s.Name] = true
	}
	for _, w := range a.watchers(nil, nil) {
		if !inReport[w.name] {
			t.Errorf("наблюдателя %s нет в reportSpecs", w.name)
		}
	}
}

func TestReportShowsClaudeTmpOutsideMode(t *testing.T) {
	a := &app{}
	cfg := config.Default("/home/u")
	cfg.ClaudeTmp.OutsideMode = "act"
	for _, spec := range a.reportSpecs(&cfg) {
		if spec.Name == "claude-tmp outside" {
			if spec.Mode != "act" {
				t.Fatalf("outside mode in report=%q", spec.Mode)
			}
			return
		}
	}
	t.Fatal("outside mode is missing from reportSpecs")
}

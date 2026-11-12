package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestGuideUnknownReturnsUsageStatus(t *testing.T) {
	a := &app{out: new(bytes.Buffer)}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	guideErr := a.guideCmd([]string{"queue.no_such_kind"})
	os.Stderr = old
	_ = w.Close()
	got, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	code, ok := exitStatus(guideErr)
	if !ok || code != 2 {
		t.Fatalf("exit status = %d, recognized=%v, err=%v", code, ok, guideErr)
	}
	for _, want := range []string{"неизвестный вид", "queue.crit", "sigguard.denied", "queue", "night"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("stderr не содержит %q: %s", want, got)
		}
	}
}

func TestGuideMultipleArgumentsPrintUsageToStderr(t *testing.T) {
	a := &app{out: new(bytes.Buffer)}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	guideErr := a.guideCmd([]string{"queue", "queue.crit"})
	os.Stderr = old
	_ = w.Close()
	got, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	code, ok := exitStatus(guideErr)
	if !ok || code != 2 || !strings.Contains(string(got), "использование: aguard guide") || a.out.(*bytes.Buffer).Len() != 0 {
		t.Fatalf("guide a b: code=%d recognized=%v stderr=%q stdout=%q", code, ok, got, a.out)
	}
}

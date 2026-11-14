package track

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func benchmarkReadProc(b *testing.B, pid int, read func(int) (procData, error)) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := read(pid); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadProcSelf(b *testing.B) {
	benchmarkReadProc(b, os.Getpid(), readProc)
}

func BenchmarkLegacyReadProcSelf(b *testing.B) {
	benchmarkReadProc(b, os.Getpid(), legacyReadProc)
}

func BenchmarkReadProcPID1(b *testing.B) {
	benchmarkReadProc(b, 1, readProc)
}

func BenchmarkLegacyReadProcPID1(b *testing.B) {
	benchmarkReadProc(b, 1, legacyReadProc)
}

func benchmarkReadProcNULTail(b *testing.B, read func(int) (procData, error)) {
	args := []string{"n", strings.Repeat("x", 100)}
	for range 10000 {
		args = append(args, "")
	}
	child := startReadProcChild(b, "echo ready; read x", args...)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := read(child.pid); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadProcNULTail10k(b *testing.B) {
	benchmarkReadProcNULTail(b, readProc)
}

func BenchmarkLegacyReadProcNULTail10k(b *testing.B) {
	benchmarkReadProcNULTail(b, legacyReadProc)
}

func BenchmarkTombsHandleExecSelf(b *testing.B) {
	pid := os.Getpid()
	t := newTombs(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	msg := execMsg(pid)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		t.handle(msg)
	}
}

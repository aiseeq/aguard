package prune

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkScanFreshTranscript(b *testing.B) {
	path := filepath.Join(b.TempDir(), "transcript.jsonl")
	var data strings.Builder
	for range 2048 {
		data.WriteString(`{"message":"routine transcript content with claude-tmp/claude-1000/project/01234567-89ab-cdef-0123-456789abcdef"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(data.Len()))
	b.ReportAllocs()
	for b.Loop() {
		if err := scanFreshTranscript(context.Background(), path, nil, map[string]bool{}); err != nil {
			b.Fatal(err)
		}
	}
}

package prune

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestScanFreshTranscriptChunkBoundaries(t *testing.T) {
	const prefix = "claude-tmp/claude-1000/project/"
	const firstID = "01234567-89ab-cdef-0123-456789abcdef"
	const secondID = "fedcba98-7654-3210-fedc-ba9876543210"
	const readChunk = 64 * 1024

	// Put the first match five bytes before the reader's chunk boundary so the
	// identifier itself is split across ReadSlice fragments.
	padding := readChunk - 5 - len(prefix)
	data := strings.Repeat("x", padding) + prefix + strings.ToUpper(firstID) + "/tail\n"
	// A second oversized line ends at EOF without a newline.
	// The identifier is the last byte of the file: only the newline appended at EOF ends the match.
	data += strings.Repeat("y", readChunk+17) + prefix + secondID
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	if err := scanFreshTranscript(context.Background(), path, nil, got); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{firstID: true, secondID: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanFreshTranscript IDs = %#v, want %#v", got, want)
	}
}

package track

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkFullMeasurePass(b *testing.B) {
	root := filepath.Join(b.TempDir(), "tree")
	if err := os.Mkdir(root, 0700); err != nil {
		b.Fatal(err)
	}
	for i := range 20000 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("entry-%05d", i)), []byte("x"), 0600); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := measure(root, 0, &pacer{ctx: context.Background()}); err != nil {
			b.Fatal(err)
		}
	}
}

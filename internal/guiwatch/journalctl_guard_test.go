package guiwatch

import (
	"os"
	"testing"

	"github.com/aiseeq/aguard/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithCommandGuard(m, "internal/guiwatch", "journalctl"))
}

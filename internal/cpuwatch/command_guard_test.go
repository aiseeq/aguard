package cpuwatch

import (
	"os"
	"testing"

	"github.com/aiseeq/aguard/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithCommandGuard(m, "internal/cpuwatch", "ps"))
}

func TestCommandGuardInstalled(t *testing.T) {
	if err := testutil.CheckCommandGuardsInstalled("ps"); err != nil {
		t.Fatal(err)
	}
}

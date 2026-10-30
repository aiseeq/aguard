package perf

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const endpoint = "2000000000"

func runScript(t *testing.T, name string, env map[string]string, args ...string) (string, int) {
	t.Helper()
	cmdArgs := append([]string{filepath.Join(".", name)}, args...)
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Dir = "."
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.Output()
	if err == nil {
		return string(out), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	t.Fatalf("run %s: %v", name, err)
	return "", -1
}

func fixture(name string) string { return filepath.Join("testdata", name) }

func TestUnitCPUDayWeightsHistoryAndActiveRun(t *testing.T) {
	show := fixture("systemctl-active.txt")
	env := map[string]string{
		"AGUARD_PERF_JOURNAL": fixture("unit-journal.log"),
		"AGUARD_PERF_SHOW":    show,
		"AGUARD_PERF_NOW":     endpoint,
		"AGUARD_PERF_UPTIME":  "100000",
	}
	out, code := runScript(t, "unit-day.sh", env, "cpu", "--until", "@"+endpoint)
	if code != 0 || strings.TrimSpace(out) != "86400" {
		t.Fatalf("got (%q, %d), want (86400, 0)", out, code)
	}
	// The first 6h run overlaps the window for 4h, contributing 4 CPU hours;
	// the next 7h and active 1h contribute 7+1. Coverage is 12h, so daily
	// normalization is 12 CPU hours * 24h / 12h = 24 CPU hours = 86400s.
}

func TestUnitCPUDayInsufficientCoverage(t *testing.T) {
	env := map[string]string{
		"AGUARD_PERF_JOURNAL": fixture("unit-short-journal.log"),
		"AGUARD_PERF_NOW":     endpoint,
		"AGUARD_PERF_SHOW":    "",
	}
	out, code := runScript(t, "unit-day.sh", env, "cpu", "--until", "@"+endpoint)
	if code != 3 || out != "" {
		t.Fatalf("got (%q, %d), want empty output and code 3", out, code)
	}
}

func TestUnitCPUDayIncludesRunEndingAfterWindow(t *testing.T) {
	env := map[string]string{
		"AGUARD_PERF_JOURNAL": fixture("unit-cross-end.log"),
		"AGUARD_PERF_NOW":     "2000007200",
		"AGUARD_PERF_SHOW":    "",
	}
	out, code := runScript(t, "unit-day.sh", env, "cpu", "--until", "@"+endpoint)
	if code != 0 || strings.TrimSpace(out) != "27000" {
		t.Fatalf("got (%q, %d), want (27000, 0)", out, code)
	}
	// The three runs cover 6+12+6 hours in the window and contribute 1.5+3+3
	// CPU hours. The 8h run ending after T contributes 4 CPU hours over its
	// full duration, hence 3 CPU hours for its 6h overlap. 7.5h/day = 27000s.
}

func TestUnitCPUDayIncludesActiveRunInPastWindow(t *testing.T) {
	env := map[string]string{
		"AGUARD_PERF_JOURNAL": fixture("unit-journal.log"),
		"AGUARD_PERF_NOW":     endpoint,
		"AGUARD_PERF_UPTIME":  "100000",
	}
	// The active launch began three hours before now and has accumulated three
	// CPU hours. This window ends two hours before now, so one hour of that run
	// overlaps; 14 CPU hours over 14 covered hours normalizes to 24 CPU hours,
	// or 86400 seconds per day.
	env["AGUARD_PERF_SHOW"] = fixture("systemctl-past-window.txt")
	out, code := runScript(t, "unit-day.sh", env, "cpu", "--until", "@1999992800")
	if code != 0 || strings.TrimSpace(out) != "86400" {
		t.Fatalf("got (%q, %d), want (86400, 0)", out, code)
	}
}

func TestUnitDayDiskMeasuresUseDeviceMaximumAndActiveRun(t *testing.T) {
	for _, tc := range []struct{ measure, want string }{{"read", "31.5"}, {"write", "48.0"}} {
		t.Run(tc.measure, func(t *testing.T) {
			env := map[string]string{
				"AGUARD_PERF_JOURNAL": fixture("unit-summary.log"),
				"AGUARD_PERF_SHOW":    fixture("systemctl-day-active.txt"),
				"AGUARD_PERF_NOW":     endpoint,
				"AGUARD_PERF_UPTIME":  "100000",
				"AGUARD_PERF_IOSTAT":  fixture("io-stat-two-devices.txt"),
			}
			out, code := runScript(t, "unit-day.sh", env, tc.measure, "--until", "@"+endpoint)
			if code != 0 || strings.TrimSpace(out) != tc.want {
				t.Fatalf("got (%q, %d), want (%s, 0)", out, code, tc.want)
			}
		})
	}
}

func TestUnitDayCurrentIOStatMaximumMatchesWholeDayCounter(t *testing.T) {
	for _, tc := range []struct{ measure, want string }{{"read", "9.0"}, {"write", "8.0"}} {
		t.Run(tc.measure, func(t *testing.T) {
			env := map[string]string{
				"AGUARD_PERF_JOURNAL": "/dev/null",
				"AGUARD_PERF_SHOW":    fixture("systemctl-day-24h-active.txt"),
				"AGUARD_PERF_NOW":     endpoint,
				"AGUARD_PERF_UPTIME":  "100000",
				"AGUARD_PERF_IOSTAT":  fixture("io-stat-two-devices.txt"),
			}
			out, code := runScript(t, "unit-day.sh", env, tc.measure, "--until", "@"+endpoint)
			if code != 0 || strings.TrimSpace(out) != tc.want {
				t.Fatalf("got (%q, %d), want (%s, 0)", out, code, tc.want)
			}
		})
	}
}

func TestUnitDayRSSIncludesEndedAndCurrentPeak(t *testing.T) {
	env := map[string]string{
		"AGUARD_PERF_JOURNAL": fixture("unit-summary.log"),
		"AGUARD_PERF_SHOW":    fixture("systemctl-day-active.txt"),
		"AGUARD_PERF_NOW":     endpoint,
		"AGUARD_PERF_STATUS":  fixture("status-current.txt"),
	}
	out, code := runScript(t, "unit-day.sh", env, "rss", "--until", "@"+endpoint)
	if code != 0 || strings.TrimSpace(out) != "250" {
		t.Fatalf("got (%q, %d), want (250, 0)", out, code)
	}
}

func TestUnitDayDiskInsufficientCoverageAndRSSNoValues(t *testing.T) {
	for _, measure := range []string{"read", "write", "rss"} {
		env := map[string]string{"AGUARD_PERF_JOURNAL": fixture("unit-short-journal.log"), "AGUARD_PERF_SHOW": "", "AGUARD_PERF_NOW": endpoint}
		out, code := runScript(t, "unit-day.sh", env, measure, "--until", "@"+endpoint)
		if code != 3 || out != "" {
			t.Errorf("%s: got (%q, %d), want empty output and code 3", measure, out, code)
		}
	}
}

func TestDiskWalkMedianAndFiltering(t *testing.T) {
	out, code := runScript(t, "disk-walk.sh", map[string]string{"AGUARD_PERF_JOURNAL": fixture("unit-journal.log")}, "--until", "@"+endpoint)
	if code != 0 || strings.TrimSpace(out) != "90.0" {
		t.Fatalf("got (%q, %d), want (90.0, 0)", out, code)
	}
}

func TestDiskWalkNeedsThreeSamples(t *testing.T) {
	out, code := runScript(t, "disk-walk.sh", map[string]string{"AGUARD_PERF_JOURNAL": fixture("disk-two.log")}, "--until", "@"+endpoint)
	if code != 3 || out != "" {
		t.Fatalf("got (%q, %d), want empty output and code 3", out, code)
	}
}

func TestExecMSCommandAndExitCodes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{{[]string{"true"}, 0}, {[]string{"sh", "-c", "exit 5"}, 5}, {[]string{"sh", "-c", "exit 3"}, 1}} {
		out, code := runScript(t, "exec-ms.sh", map[string]string{"AGUARD_PERF_BUDGET_S": "0"}, tc.args...)
		if code != tc.want {
			t.Errorf("%v: code %d, want %d (stdout %q)", tc.args, code, tc.want, out)
		}
		if tc.want == 0 && !regexp.MustCompile(`^\d+\n$`).MatchString(out) {
			t.Errorf("successful command printed %q, want one integer", out)
		}
		if tc.want != 0 && out != "" {
			t.Errorf("failed command printed %q", out)
		}
	}
}

func TestExecMSZeroBudgetStillRunsOnce(t *testing.T) {
	count := filepath.Join(t.TempDir(), "runs")
	out, code := runScript(t, "exec-ms.sh", map[string]string{"AGUARD_PERF_BUDGET_S": "0"}, "sh", "-c", "echo run >> \"$1\"; true", "count", count)
	if code != 0 || !regexp.MustCompile(`^\d+\n$`).MatchString(out) {
		t.Fatalf("got (%q, %d), want integer and code 0", out, code)
	}
	data, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "run\n") != 1 {
		t.Fatalf("ran %d times, want once", strings.Count(string(data), "run\n"))
	}
}

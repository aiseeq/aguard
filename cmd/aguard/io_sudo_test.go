package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/aguard/internal/ioroute"
	"github.com/aiseeq/aguard/internal/iowatch"
)

func sudoFixture(t *testing.T) ([]iowatch.Case, []journalRecord, []journalRecord) {
	t.Helper()
	base := filepath.Join("testdata", "io-sudo")
	cases, err := iowatch.LoadCases(filepath.Join(base, "case-1109.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []journalRecord {
		b, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := parseJournalJSON(string(b))
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	return cases, read("sudo.jsonl"), read("logind.jsonl")
}

func appWithSudoFixture(sudo, logind []journalRecord) *app {
	return &app{sudoJournalSource: func(_ context.Context, _, _ string, filters ...string) ([]journalRecord, error) {
		if len(filters) == 0 {
			return nil, nil
		}
		if filters[0] == "_COMM=sudo" {
			return sudo, nil
		}
		if filters[0] == "_SYSTEMD_UNIT=systemd-logind.service" {
			return logind, nil
		}
		return nil, nil
	}}
}

func TestSudoInfoRoutesReal1109FixtureMainAndForeign(t *testing.T) {
	cases, sudo, logind := sudoFixture(t)
	if len(cases) != 1 {
		t.Fatalf("fixture cases=%d, want 1", len(cases))
	}
	a := appWithSudoFixture(sudo, logind)
	cache := newSudoLookupCache()
	if err := a.sudoInfo(context.Background(), &cases[0], cache, false); err != nil {
		t.Fatal(err)
	}
	main, foreign := cases[0].Sudo, cases[0].ForeignSudo
	if main == nil || main.Command != "rm -rf --one-file-system /home/aisee/.cache/claude-tmp.pre-subvolume" || main.TTY != "pts/5" {
		t.Fatalf("c497 attribution: %+v", main)
	}
	if foreign == nil || foreign.Command != "dnf upgrade --refresh" || foreign.TTY != "pts/17" {
		t.Fatalf("foreign c492 attribution: %+v", foreign)
	}
	route, err := (&ioOwnerFinder{}).routeCase(&cases[0])
	if err != nil {
		t.Fatal(err)
	}
	if route.Owner != "" || !route.Generic {
		t.Fatalf("root route should have no owner ticket: %+v", route)
	}
	line := ioCaseLine(&cases[0])
	for _, part := range []string{"rm -rf --one-file-system", "TTY pts/5", "dnf upgrade --refresh", "TTY pts/17"} {
		if !strings.Contains(line, part) {
			t.Fatalf("case line missing %q: %s", part, line)
		}
	}
}

func TestSudoInfoNoTTYCommandOwnerAndCgroupLabels(t *testing.T) {
	_, sudo, logind := sudoFixture(t)
	c := iowatch.Case{Start: time.UnixMicro(1794737837000000), Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c498.scope"}}
	a := appWithSudoFixture(sudo, logind)
	if err := a.sudoInfo(context.Background(), &c, newSudoLookupCache(), false); err != nil {
		t.Fatal(err)
	}
	if c.Sudo == nil || c.Sudo.TTY != "" || c.Sudo.Command != "btrfs subvolume show /home/aisee/.cache/claude-tmp" || c.Sudo.Owner != "machine-master" {
		t.Fatalf("c498 agent command attribution: %+v", c.Sudo)
	}
	route, err := (&ioOwnerFinder{}).routeCase(&c)
	if err != nil || route.Owner != "machine-master" || route.Class != "root" || !route.Generic {
		t.Fatalf("root agent attribution did not reach route: route=%+v err=%v", route, err)
	}
	if who, label := sudoOwnerFromCgroup("/user.slice/user-1000.slice/user@1000.service/app.slice/app-org.kde.konsole-7438.scope/tab(10951).scope"); who != "" || label != "терминал пользователя (konsole/far2l)" {
		t.Fatalf("konsole owner=%q label=%q", who, label)
	}
	if who, label := sudoOwnerFromCgroup("/user.slice/user-1000.slice/user@1000.service/app.slice/app-far2l@session.service"); who != "" || label != "терминал пользователя (konsole/far2l)" {
		t.Fatalf("far2l owner=%q label=%q", who, label)
	}
}

func TestStoredRootSessionAndBareUserSession(t *testing.T) {
	rows := []journalRecord{
		{Message: "aisee : TTY=pts/17 ; PWD=/home/aisee/Downloads ; USER=root ; COMMAND=dnf upgrade --refresh", PID: "492", Timestamp: "1000000", BootID: "boot"},
		{Message: "pam_unix(sudo:session): session opened for user root(uid=0) by aisee(uid=1000)", PID: "492", Timestamp: "1100000", Session: "c492", BootID: "boot"},
	}
	logind := []journalRecord{
		{Message: "New session 'c492' of user root.", Timestamp: "1200000", BootID: "boot"},
		{Message: "New session 'c53' of user aisee.", Timestamp: "1300000", BootID: "boot"},
	}
	a := appWithSudoFixture(rows, logind)
	rootCase := iowatch.Case{Start: time.UnixMicro(1500000), BootID: "boot", Culprits: &iowatch.Culprit{Main: "session-c492.scope"}}
	if err := a.sudoInfo(t.Context(), &rootCase, newSudoLookupCache(), false); err != nil {
		t.Fatal(err)
	}
	if rootCase.Sudo == nil || rootCase.Sudo.Command != "dnf upgrade --refresh" {
		t.Fatalf("stored root main session was not resolved: %+v", rootCase.Sudo)
	}
	route, err := (&ioOwnerFinder{}).routeCase(&rootCase)
	if err != nil || route.Class != "root" || !route.Generic {
		t.Fatalf("stored root session route=%+v err=%v", route, err)
	}
	userCase := iowatch.Case{Start: time.UnixMicro(1500000), BootID: "boot", Culprits: &iowatch.Culprit{Main: "session-c53.scope"}}
	if err := a.sudoInfo(t.Context(), &userCase, newSudoLookupCache(), false); err != nil {
		t.Fatal(err)
	}
	if userCase.Sudo != nil {
		t.Fatalf("bare user session was attributed as root: %+v", userCase.Sudo)
	}
	userRoute, err := (&ioOwnerFinder{}).routeCase(&userCase)
	if err != nil || userRoute.Class == "root" {
		t.Fatalf("bare user session route=%+v err=%v", userRoute, err)
	}
	foreignUserCase := iowatch.Case{Start: time.UnixMicro(1500000), BootID: "boot", Culprits: &iowatch.Culprit{Main: "ordinary.service", Foreign: iowatch.Ranked{Name: "session-c53.scope"}}}
	if err := a.sudoInfo(t.Context(), &foreignUserCase, newSudoLookupCache(), false); err != nil {
		t.Fatal(err)
	}
	if foreignUserCase.ForeignSudo != nil {
		t.Fatalf("bare foreign user session was assumed to be root: %+v", foreignUserCase.ForeignSudo)
	}
}

func TestSudoInfoCacheAndSavedAttribution(t *testing.T) {
	cases, sudo, logind := sudoFixture(t)
	cases[0].BootID = "boot-a"
	calls := 0
	a := appWithSudoFixture(sudo, logind)
	base := a.sudoJournalSource
	a.sudoJournalSource = func(ctx context.Context, since, until string, filters ...string) ([]journalRecord, error) {
		calls++
		return base(ctx, since, until, filters...)
	}
	cache := newSudoLookupCache()
	if err := a.sudoInfo(context.Background(), &cases[0], cache, false); err != nil {
		t.Fatal(err)
	}
	firstCalls := calls
	second := cases[0]
	second.Sudo = nil
	second.ForeignSudo = nil
	if err := a.sudoInfo(context.Background(), &second, cache, false); err != nil {
		t.Fatal(err)
	}
	if calls != firstCalls || second.Sudo == nil {
		t.Fatalf("cached lookup made more journal calls: %d -> %d", firstCalls, calls)
	}
	saved := &iowatch.SudoInfo{Command: "persisted", Owner: "saved-agent"}
	cases[0].Sudo = saved
	before := calls
	if err := a.sudoInfo(context.Background(), &cases[0], cache, false); err != nil {
		t.Fatal(err)
	}
	if cases[0].Sudo != saved || calls != before {
		t.Fatalf("stored sudo attribution was overwritten: %+v calls %d->%d", cases[0].Sudo, before, calls)
	}
}

func TestSudoLivePathUsesSessionCgroupPID(t *testing.T) {
	root := t.TempDir()
	cgroup := filepath.Join(root, "cg", "user.slice/user-0.slice/session-c497.scope")
	if err := os.MkdirAll(cgroup, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroup, "cgroup.procs"), []byte("1196378\n"), 0600); err != nil {
		t.Fatal(err)
	}
	procRoot := filepath.Join(root, "proc")
	bootIDFile := filepath.Join(procRoot, "sys/kernel/random/boot_id")
	if err := os.MkdirAll(filepath.Dir(bootIDFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootIDFile, []byte("boot-a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows := []journalRecord{{Message: "aisee : USER=root ; COMMAND=rm -rf --one-file-system /tmp/x", PID: "1196378", Timestamp: "1794737303591247", BootID: "boot-a"}}
	logindCalls := 0
	pidFiltered := false
	a := &app{cgroupDir: filepath.Join(root, "cg"), procDir: procRoot, bootID: func() (string, error) { return "boot-a", nil },
		sudoJournalSource: func(_ context.Context, _, _ string, filters ...string) ([]journalRecord, error) {
			if filters[0] == "_SYSTEMD_UNIT=systemd-logind.service" {
				logindCalls++
			}
			for _, filter := range filters {
				if filter == "_PID=1196378" {
					pidFiltered = true
				}
			}
			return rows, nil
		}}
	c := iowatch.Case{Start: time.UnixMicro(1794737303592000), Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	if err := a.sudoInfo(context.Background(), &c, newSudoLookupCache(), true); err != nil {
		t.Fatal(err)
	}
	if c.Sudo == nil || c.Sudo.Command != "rm -rf --one-file-system /tmp/x" || logindCalls != 0 || !pidFiltered {
		t.Fatalf("live PID association: sudo=%+v logind calls=%d pid-filtered=%v", c.Sudo, logindCalls, pidFiltered)
	}
}

func TestSudoPIDReuseAndBootID(t *testing.T) {
	rows := []journalRecord{
		{Message: "user : USER=root ; COMMAND=old command", PID: "4001962", Timestamp: "1000000", BootID: "boot-old"},
		{Message: "pam_unix(sudo:session): session opened for user root", PID: "4001962", Timestamp: "2000000", Session: "c54", BootID: "boot-old"},
		{Message: "user : USER=root ; COMMAND=new command", PID: "4001962", Timestamp: "9000000", BootID: "boot-new"},
		{Message: "pam_unix(sudo:session): session opened for user root", PID: "4001962", Timestamp: "10000000", Session: "c54", BootID: "boot-new"},
	}
	got := sudoForExactSession(nil, "54", "boot-new", time.UnixMicro(11000000), rows)
	if got == nil || got.Command != "new command" || got.BootID != "boot-new" {
		t.Fatalf("reused PID/session mapped across boot: %+v", got)
	}
}

func TestSudoExactSessionRejectsOtherBoot(t *testing.T) {
	rows := []journalRecord{
		{Message: "user : USER=root ; COMMAND=old boot", PID: "54", Timestamp: "1000000", BootID: "boot-old"},
		{Message: "pam_unix(sudo:session): session opened for user root", PID: "54", Timestamp: "2000000", Session: "c54", BootID: "boot-old"},
	}
	if got := sudoForExactSession(nil, "54", "boot-new", time.UnixMicro(3000000), rows); got != nil {
		t.Fatalf("exact lookup crossed boot ID: %+v", got)
	}
}

func TestSudoCacheRequiresBootMatch(t *testing.T) {
	cache := newSudoLookupCache()
	opened := time.Unix(100, 0)
	cache.add("54", opened, "boot-old", &iowatch.SudoInfo{Command: "old boot", BootID: "boot-old"})
	if got, ok := cache.find("54", "boot-new", opened.Add(time.Minute)); ok || got != nil {
		t.Fatalf("cache returned another boot's command: %+v", got)
	}
}

func TestSudoCacheWithoutBootIDRequiresSessionLifetime(t *testing.T) {
	cache := newSudoLookupCache()
	opened, closed := time.Unix(100, 0), time.Unix(200, 0)
	cache.add("54", opened, "boot-old", &iowatch.SudoInfo{Command: "within session", BootID: "boot-old", OpenedAt: opened, ClosedAt: closed})
	if got, ok := cache.find("54", "", opened.Add(time.Minute)); !ok || got == nil || got.Command != "within session" {
		t.Fatalf("valid same-session cache entry missed: got=%+v ok=%v", got, ok)
	}
	if got, ok := cache.find("54", "", closed.Add(time.Minute)); ok || got != nil {
		t.Fatalf("cache entry escaped its session lifetime: got=%+v ok=%v", got, ok)
	}
}

func TestSudoExactSessionDoesNotSelectNeighbor(t *testing.T) {
	rows := []journalRecord{
		{Message: "user : USER=root ; COMMAND=dnf upgrade --releasever=491", PID: "491", Timestamp: "1000000", BootID: "boot"},
		{Message: "pam_unix(sudo:session): session opened for user root", PID: "491", Timestamp: "1100000", Session: "c491", BootID: "boot"},
		{Message: "user : USER=root ; COMMAND=dnf upgrade --refresh", PID: "492", Timestamp: "2000000", BootID: "boot"},
		{Message: "pam_unix(sudo:session): session opened for user root", PID: "492", Timestamp: "2100000", Session: "c492", BootID: "boot"},
	}
	got := sudoForExactSession(nil, "492", "boot", time.UnixMicro(3000000), rows)
	if got == nil || got.Command != "dnf upgrade --refresh" {
		t.Fatalf("c492 matched neighboring session: %+v", got)
	}
}

func TestSudoSameNumberWithoutBootIDDoesNotReuseAcrossBoots(t *testing.T) {
	oldCommand := time.Date(2026, time.November, 9, 21, 43, 7, 0, time.Local)
	oldOpen := oldCommand.Add(3 * time.Second)
	oldCase := oldCommand.Add(time.Minute)
	newCommand := time.Date(2026, time.November, 11, 14, 14, 15, 0, time.Local)
	newOpen := newCommand.Add(2 * time.Second)
	newCase := newCommand.Add(time.Minute)
	rows := []journalRecord{
		{Message: "user : USER=root ; COMMAND=dnf upgrade old", PID: "54", Timestamp: strconv.FormatInt(oldCommand.UnixMicro(), 10), BootID: "boot-09"},
		{Message: "pam_unix(sudo:session): session opened for user root", PID: "54", Timestamp: strconv.FormatInt(oldOpen.UnixMicro(), 10), Session: "c54", BootID: "boot-09"},
		{Message: "user : USER=root ; COMMAND=dnf upgrade --refresh", PID: "54", Timestamp: strconv.FormatInt(newCommand.UnixMicro(), 10), BootID: "boot-11"},
		{Message: "pam_unix(sudo:session): session opened for user root", PID: "54", Timestamp: strconv.FormatInt(newOpen.UnixMicro(), 10), Session: "c54", BootID: "boot-11"},
	}
	logind := []journalRecord{
		{Message: "New session 'c54' of user root.", Timestamp: strconv.FormatInt(oldOpen.Add(100*time.Millisecond).UnixMicro(), 10), BootID: "boot-09"},
		{Message: "Removed session c54.", Timestamp: strconv.FormatInt(oldCase.Add(time.Minute).UnixMicro(), 10), BootID: "boot-09"},
		{Message: "New session 'c54' of user root.", Timestamp: strconv.FormatInt(newOpen.Add(100*time.Millisecond).UnixMicro(), 10), BootID: "boot-11"},
		{Message: "Removed session c54.", Timestamp: strconv.FormatInt(newCase.Add(time.Minute).UnixMicro(), 10), BootID: "boot-11"},
	}
	a := appWithSudoFixture(rows, logind)
	cache := newSudoLookupCache()
	for _, tc := range []struct {
		at   int64
		want string
	}{{oldCase.UnixMicro(), "dnf upgrade old"}, {newCase.UnixMicro(), "dnf upgrade --refresh"}} {
		c := iowatch.Case{Start: time.UnixMicro(tc.at), Culprits: &iowatch.Culprit{Main: "session-c54.scope"}}
		if err := a.sudoInfo(t.Context(), &c, cache, false); err != nil {
			t.Fatal(err)
		}
		if c.Sudo == nil || c.Sudo.Command != tc.want {
			t.Fatalf("c54 at %s: got %+v, want %q", c.Start, c.Sudo, tc.want)
		}
	}
}

func TestSudoMissIsCachedButErrorsAreNot(t *testing.T) {
	calls := 0
	a := &app{sudoJournalSource: func(context.Context, string, string, ...string) ([]journalRecord, error) {
		calls++
		return nil, nil
	}}
	cache := newSudoLookupCache()
	for _, start := range []time.Time{time.Unix(100, 0), time.Unix(101, 0)} {
		c := iowatch.Case{Start: start, BootID: "boot", Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
		if err := a.sudoInfo(t.Context(), &c, cache, false); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("negative cache made %d journal calls, want one sudo and one logind call", calls)
	}
}

func TestLiveSudoIgnoresCommandsAfterCaseStart(t *testing.T) {
	rows := []journalRecord{
		{Message: "aisee : USER=root ; COMMAND=rm old", PID: "10", Timestamp: "1000000", BootID: "boot"},
		{Message: "aisee : USER=root ; COMMAND=rm future", PID: "10", Timestamp: "3000000", BootID: "boot"},
	}
	got := liveSudoForPIDs(nil, "7", rows, []int{10}, "boot", time.UnixMicro(2000000))
	if got == nil || got.Command != "rm old" {
		t.Fatalf("live lookup selected a future command: %+v", got)
	}
}

func TestSudoCommandForPIDChoosesLatestInSameBoot(t *testing.T) {
	rows := []journalRecord{
		{Message: "user : USER=root ; COMMAND=old", PID: "10", Timestamp: "1000000", BootID: "boot"},
		{Message: "user : USER=root ; COMMAND=new", PID: "10", Timestamp: "2000000", BootID: "boot"},
	}
	got := sudoCommandForPID(nil, rows, "10", "boot", "3000000")
	if got == nil || got.Command != "new" {
		t.Fatalf("PID reuse selected first command: %+v", got)
	}
}

func TestSudoLogindFallbackHasFiveSecondBound(t *testing.T) {
	for _, delta := range []struct {
		micros  int64
		matched bool
	}{{4_000_000, true}, {60_000_000, false}} {
		at := time.UnixMicro(100_000_000)
		logind := []journalRecord{{Message: "New session 'c8' of user root", Timestamp: "100000000", BootID: "boot"}}
		sudo := []journalRecord{
			{Message: "pam_unix(sudo:session): session opened for user root", PID: "8", Timestamp: strconv.FormatInt(100_000_000-delta.micros, 10), BootID: "boot"},
			{Message: "user : USER=root ; COMMAND=dnf upgrade", PID: "8", Timestamp: strconv.FormatInt(100_000_000-delta.micros-1_000, 10), BootID: "boot"},
		}
		got := sudoForLogindSession(nil, "8", "boot", at, sudo, logind)
		if (got != nil) != delta.matched {
			t.Fatalf("delta %d μs: got %+v", delta.micros, got)
		}
	}
}

func TestSudoMissIsRetried(t *testing.T) {
	_, sudo, logind := sudoFixture(t)
	calls := 0
	a := appWithSudoFixture(sudo, logind)
	a.sudoJournalSource = func(ctx context.Context, since, until string, filters ...string) ([]journalRecord, error) {
		calls++
		if calls == 1 {
			return nil, context.DeadlineExceeded
		}
		return appWithSudoFixture(sudo, logind).sudoJournalSource(ctx, since, until, filters...)
	}
	c := iowatch.Case{Start: time.UnixMicro(1794737305000000), Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	cache := newSudoLookupCache()
	_ = a.sudoInfo(context.Background(), &c, cache, false)
	_ = a.sudoInfo(context.Background(), &c, cache, false)
	if c.Sudo == nil || calls < 2 {
		t.Fatalf("failed lookup was cached as a permanent miss: info=%+v calls=%d", c.Sudo, calls)
	}
}

func TestClosedRootCaseDoesNotWaitForJournal(t *testing.T) {
	ctx := t.Context()
	started, release := make(chan struct{}, 1), make(chan struct{})
	a := &app{sudoJournalSource: func(ctx context.Context, _, _ string, _ ...string) ([]journalRecord, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	s := &ioService{a: a, ctx: ctx, cfg: iowatch.Default(), extra: make(chan func(), 2), sudoCache: newSudoLookupCache()}
	c := iowatch.Case{Start: time.Now(), DurMS: 3000, Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	returned := make(chan struct{})
	go func() { s.closed(c); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		close(release)
		t.Fatal("closed waited for journal I/O")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("closed did not schedule sudo enrichment")
	}
	close(release)
}

func TestClosedRootCasePersistsSudoBeforeRoutingOwner(t *testing.T) {
	_, sudo, logind := sudoFixture(t)
	a, _ := testIOApp(t)
	a.sudoJournalSource = appWithSudoFixture(sudo, logind).sudoJournalSource
	cfg := iowatch.Default()
	service := &ioService{
		a: a, ctx: t.Context(), cfg: cfg, extra: make(chan func(), 2), sudoCache: newSudoLookupCache(), bootID: "boot-a",
		route: ioroute.New(cfg.Route()), owners: &ioOwnerFinder{}, tickets: &ioTicketer{a: a, queue: make(chan ioTicketJob, 4)},
	}
	c := iowatch.Case{Start: time.UnixMicro(1794737837000000), DurMS: 5000, BootID: "boot-a",
		Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c498.scope"}}
	service.closed(c)
	select {
	case apply := <-service.extra:
		apply()
	case <-time.After(time.Second):
		t.Fatal("sudo attribution did not return to the case loop")
	}
	if service.cases[0].Sudo == nil || service.cases[0].Sudo.Owner != "machine-master" {
		t.Fatalf("case did not retain sudo attribution: %+v", service.cases[0].Sudo)
	}
	select {
	case job := <-service.tickets.queue:
		if job.owner != "machine-master" {
			t.Fatalf("ticket routed to %q, want machine-master", job.owner)
		}
	case <-time.After(time.Second):
		t.Fatal("closed root case did not route a ticket to its agent owner")
	}
}

func TestCanceledSudoSearchStillLogsAndRoutesCase(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var logs bytes.Buffer
	a, _ := testIOApp(t)
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.sudoJournalSource = func(ctx context.Context, _, _ string, _ ...string) ([]journalRecord, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cfg := iowatch.Default()
	cfg.Mode = iowatch.ModeLog
	cfg.Lead = "lead"
	cfg.UrgentMin = cfg.AlertMin
	service := &ioService{a: a, ctx: ctx, cfg: cfg, extra: make(chan func(), 2), sudoCache: newSudoLookupCache(),
		route: ioroute.New(cfg.Route()), owners: &ioOwnerFinder{}, tickets: &ioTicketer{a: a, queue: make(chan ioTicketJob, 4)}}
	c := iowatch.Case{Start: time.Now(), DurMS: 4000, Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	if !alertable(&c, cfg) {
		t.Fatalf("test case is not alertable: case=%+v config=%+v", c, cfg)
	}
	service.closed(c)
	cancel()
	service.drainSudoWorkers()
	service.letters.Wait()
	if len(service.cases) != 1 || service.cases[0].Sudo != nil {
		t.Fatalf("canceled lookup lost case or attached attribution: %+v", service.cases)
	}
	if !strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("case was not routed after canceled lookup: %s", logs.String())
	}
}

func TestClosedCasesRouteWithoutWaitingForRootLookup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var logs bytes.Buffer
	a, _ := testIOApp(t)
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.sudoJournalSource = func(ctx context.Context, _, _ string, _ ...string) ([]journalRecord, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	cfg := iowatch.Default()
	service := &ioService{a: a, ctx: t.Context(), cfg: cfg, extra: make(chan func(), 4), sudoCache: newSudoLookupCache(),
		route: ioroute.New(cfg.Route()), owners: &ioOwnerFinder{}, tickets: &ioTicketer{a: a, queue: make(chan ioTicketJob, 4)}}
	firstAt := time.Date(2026, 11, 15, 11, 9, 28, 0, time.Local)
	first := iowatch.Case{Start: firstAt, DurMS: 1000, Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	second := iowatch.Case{Start: firstAt.Add(time.Second), DurMS: 1000, Culprits: &iowatch.Culprit{Main: "adream.service"}}
	service.closed(first)
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("root lookup did not start")
	}
	service.closed(second)
	secondLine := firstAt.Add(time.Second).Format("02.01 15:04:05")
	if !strings.Contains(logs.String(), secondLine) {
		close(release)
		t.Fatalf("non-root case waited for sudo lookup: %s", logs.String())
	}
	close(release)
	select {
	case apply := <-service.extra:
		apply()
	case <-time.After(time.Second):
		t.Fatal("root lookup result did not return to case loop")
	}
	firstLine := firstAt.Format("02.01 15:04:05")
	if !strings.Contains(logs.String(), firstLine) || !strings.Contains(logs.String(), secondLine) {
		t.Fatalf("case output missing after sudo lookup: %s", logs.String())
	}
}

func TestProbeTrimKeepsRouting(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var logs bytes.Buffer
	a, _ := testIOApp(t)
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.sudoJournalSource = func(ctx context.Context, _, _ string, _ ...string) ([]journalRecord, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil, nil
	}
	cfg := iowatch.Default()
	cfg.Mode = iowatch.ModeLog
	cfg.Lead = "lead"
	service := &ioService{a: a, ctx: t.Context(), cfg: cfg, extra: make(chan func(), 4), sudoCache: newSudoLookupCache(),
		route: ioroute.New(cfg.Route()), owners: &ioOwnerFinder{}, tickets: &ioTicketer{a: a, queue: make(chan ioTicketJob, 4)}}
	old := iowatch.Case{Start: time.Now().Add(-400 * 24 * time.Hour), DurMS: 4000, Culprits: &iowatch.Culprit{Main: "old.service"}}
	service.cases = append(service.cases, old)
	firstAt := time.Now()
	first := iowatch.Case{Start: firstAt, DurMS: 4000, Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	service.closed(first)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("root lookup did not start")
	}
	service.kept(time.Now())
	second := iowatch.Case{Start: firstAt.Add(time.Second), DurMS: 4000, Culprits: &iowatch.Culprit{Main: "second.service"}}
	service.closed(second)
	if !strings.Contains(logs.String(), "second.service") {
		t.Fatalf("case after kept() was not logged/routed: %s", logs.String())
	}
	close(release)
	select {
	case apply := <-service.extra:
		apply()
	case <-time.After(time.Second):
		t.Fatal("pending sudo lookup did not return")
	}
	if !strings.Contains(logs.String(), "session-c497.scope") {
		t.Fatalf("trimmed case was not logged/routed after lookup: %s", logs.String())
	}
}

func TestExactPAMSessionResolvesBareRootCgroupWithoutLogind(t *testing.T) {
	rows := []journalRecord{
		{Message: "user : USER=root ; COMMAND=dnf upgrade", PID: "54", Timestamp: "1000000", BootID: "boot"},
		{Message: "pam_unix(sudo:session): session opened for user root(uid=0)", PID: "54", Timestamp: "1100000", Session: "c54", BootID: "boot"},
	}
	logindCalls := 0
	a := &app{sudoJournalSource: func(_ context.Context, _, _ string, filters ...string) ([]journalRecord, error) {
		if len(filters) > 0 && filters[0] == "_SYSTEMD_UNIT=systemd-logind.service" {
			logindCalls++
		}
		return rows, nil
	}}
	c := iowatch.Case{Start: time.UnixMicro(2_000_000), BootID: "boot", Culprits: &iowatch.Culprit{Main: "session-c54.scope"}}
	if err := a.sudoInfo(t.Context(), &c, newSudoLookupCache(), false); err != nil {
		t.Fatal(err)
	}
	if c.Sudo == nil || c.Sudo.Command != "dnf upgrade" || logindCalls != 0 {
		t.Fatalf("exact PAM match=%+v logind calls=%d", c.Sudo, logindCalls)
	}
}

func TestSudoNegativeCacheExpiryPrunesOldEntries(t *testing.T) {
	cache := newSudoLookupCache()
	start := time.Unix(100, 0)
	cache.add("54", start, "boot", nil)
	if _, ok := cache.find("54", "boot", start.Add(sudoNegativeCacheWindow-time.Second)); !ok {
		t.Fatal("negative cache miss expired too early")
	}
	if _, ok := cache.find("54", "boot", start.Add(sudoNegativeCacheWindow+time.Second)); ok {
		t.Fatal("negative cache miss survived its lifetime")
	}
	cache.add("54", start.Add(sudoNegativeCacheWindow+time.Second), "boot", nil)
	if got := len(cache.bySession["54"]); got != 1 {
		t.Fatalf("expired negative cache entries retained: %d", got)
	}
}

func TestTrimDuringSudoLookupDoesNotAttachToDifferentCase(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	start := time.Now().Add(-400 * 24 * time.Hour)
	openedAt := start.Add(-time.Millisecond)
	commandRow := journalRecord{Message: "aisee : USER=root ; COMMAND=trimmed case command", PID: "77", Timestamp: strconv.FormatInt(openedAt.UnixMicro(), 10), BootID: "boot"}
	a, _ := testIOApp(t)
	a.sudoJournalSource = func(ctx context.Context, _, _ string, _ ...string) ([]journalRecord, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return []journalRecord{commandRow, {Message: "pam_unix(sudo:session): session opened for user root", PID: "77", Timestamp: strconv.FormatInt(openedAt.Add(time.Microsecond).UnixMicro(), 10), Session: "c497", BootID: "boot"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	cfg := iowatch.Default()
	service := &ioService{a: a, ctx: t.Context(), cfg: cfg, extra: make(chan func(), 4), sudoCache: newSudoLookupCache(),
		route: ioroute.New(cfg.Route()), owners: &ioOwnerFinder{}, tickets: &ioTicketer{a: a, queue: make(chan ioTicketJob, 4)}}
	first := iowatch.Case{Start: start, DurMS: 4000, BootID: "boot", Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	service.closed(first)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sudo search did not start")
	}
	service.kept(time.Now())
	other := iowatch.Case{Start: start.Add(time.Second), DurMS: 4000, BootID: "boot", Culprits: &iowatch.Culprit{Main: "other.service"}}
	service.closed(other)
	close(release)
	select {
	case apply := <-service.extra:
		apply()
	case <-time.After(time.Second):
		t.Fatal("sudo result did not return after trim")
	}
	if len(service.cases) != 1 || service.cases[0].Culprits.Main != "other.service" || service.cases[0].Sudo != nil {
		t.Fatalf("trimmed result attached to wrong case: %+v", service.cases)
	}
}

func TestTrimmedRootCaseStillLogsAndRoutesAfterSudoLookup(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	var logs bytes.Buffer
	a, _ := testIOApp(t)
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.sudoJournalSource = func(ctx context.Context, _, _ string, _ ...string) ([]journalRecord, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	cfg := iowatch.Default()
	cfg.Mode, cfg.Lead, cfg.UrgentMin = iowatch.ModeLog, "lead", cfg.AlertMin
	service := &ioService{a: a, ctx: t.Context(), cfg: cfg, extra: make(chan func(), 4), sudoCache: newSudoLookupCache(),
		route: ioroute.New(cfg.Route()), owners: &ioOwnerFinder{}, tickets: &ioTicketer{a: a, queue: make(chan ioTicketJob, 4)}}
	start := time.Now()
	root := iowatch.Case{Start: start, End: start.Add(5 * time.Second), DurMS: 5000, BootID: "boot",
		Culprits: &iowatch.Culprit{Main: "/user.slice/user-0.slice/session-c497.scope"}}
	service.closed(root)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sudo search did not start")
	}
	service.kept(time.Now().Add(400 * 24 * time.Hour))
	if len(service.cases) != 0 {
		close(release)
		t.Fatalf("root case was not trimmed while lookup was pending: %+v", service.cases)
	}
	close(release)
	select {
	case apply := <-service.extra:
		apply()
	case <-time.After(time.Second):
		t.Fatal("sudo result did not return after trim")
	}
	service.letters.Wait()
	if !strings.Contains(logs.String(), "session-c497.scope") || !strings.Contains(logs.String(), "написал бы") {
		t.Fatalf("trimmed root case was not logged and routed: %s", logs.String())
	}
}

func TestParseSudoMessageOptionalTTYAndCommandSemicolons(t *testing.T) {
	info, ok := parseSudoMessage("aisee : PWD=/tmp ; USER=root ; COMMAND=/usr/bin/sh -c 'echo a ; echo b'")
	if !ok || info.TTY != "" || info.Command != "sh -c 'echo a ; echo b'" {
		t.Fatalf("parsed sudo row: ok=%v info=%+v", ok, info)
	}
}

func TestSudoSecretMaskingAndLimit(t *testing.T) {
	type maskCase struct {
		command string
		masked  bool
		want    string
	}
	caseOf := func(command string, masked bool, want ...string) maskCase {
		result := maskCase{command: command, masked: masked}
		if len(want) > 0 {
			result.want = want[0]
		}
		return result
	}
	cases := []maskCase{
		caseOf("mysql -uo\\'brien -pPW db", true, "mysql -uo\\'brien -p[REDACTED] db"),
		caseOf("mysql -e select\\'1\\' -pPW db", true, "mysql -e select\\'1\\' -p[REDACTED] db"),
		caseOf("mysql -pit\\'s db", true, "mysql -p[REDACTED] db"),
		caseOf("mysql -p'ab'cd db", true, "mysql '-p[REDACTED]' db"),
		caseOf("htpasswd -b f o\\'brien PW", true, "htpasswd -b f o\\'brien [REDACTED]"),
		caseOf("htpasswd -b f admin '<s e>'", true, "htpasswd -b f admin '[REDACTED]'"),
		caseOf("htpasswd -b f admin <secret", true, "htpasswd -b f admin [REDACTED]"),
		caseOf("mysql -pab& db", true, "mysql -p[REDACTED] db"),
		caseOf("mysql -pab|| db", true, "mysql -p[REDACTED] db"),
		caseOf("bash -c 'echo \\'a\\\\\\' ; mysql -pX'", true, "bash -c 'echo \\'a\\\\\\' ; mysql -p[REDACTED]'"),
		caseOf("bash -c 'mysql -pX ; echo -pNotSecret ; mysql -pY'", true, "bash -c 'mysql -p[REDACTED] ; echo -pNotSecret ; mysql -p[REDACTED]'"),
		caseOf("bash -c -- 'mysql -pX'", true, "bash -c -- 'mysql -p[REDACTED]'"),
		caseOf("mysql -pab&cd db", true, "mysql -p[REDACTED] db"),
		caseOf("mysql -pab|cd db", true, "mysql -p[REDACTED] db"),
		caseOf("mysql -pab>cd db", true, "mysql -p[REDACTED] db"),
		caseOf("bash -c 'mysql -p\\'ab&cd\\' db'", true, "bash -c 'mysql -p\\'[REDACTED]\\' db'"),
		caseOf("bash -c 'mysql -pab&cd'", true, "bash -c 'mysql -p[REDACTED]&cd'"),
		caseOf(`mysql -e select"x -pPW`, true, `mysql -e select"x -p[REDACTED]`),
		caseOf("mysql '-pa b' db", true, "mysql '-p[REDACTED]' db"),
		caseOf("bash -c 'mysql -p\\'a b\\''", true, "bash -c 'mysql -p\\'[REDACTED]\\''"),
		caseOf("htpasswd -b f admin ab&cd", true, "htpasswd -b f admin [REDACTED]"),
		caseOf("bash --norc -c 'mysql -pX'", true, "bash --norc -c 'mysql -p[REDACTED]'"),
		caseOf("bash --rcfile /x -c 'mysql -pX'", true, "bash --rcfile /x -c 'mysql -p[REDACTED]'"),
		caseOf("mysql -pS3cretPW", true),
		caseOf("mysql -pab#cd db", true, "mysql -p[REDACTED] db"),
		caseOf("mysql -pab#012x", true, "mysql -p[REDACTED]"),
		caseOf("mysql -e 'select 1; select 2' -pX", true, "mysql -e 'select 1; select 2' -p[REDACTED]"),
		caseOf(`mysql -e 'select "a;b"' -pX`, true, `mysql -e 'select "a;b"' -p[REDACTED]`),
		caseOf("mysql -e 'select 1 # c' -pX", true, "mysql -e 'select 1 # c' -p[REDACTED]"),
		caseOf("mysql -P3306 -pPW", true, "mysql -P3306 -p[REDACTED]"),
		caseOf("mysqldump -P3306 -u root -pPW db", true, "mysqldump -P3306 -u root -p[REDACTED] db"),
		caseOf("bash -c 'mysql -pX'", true, `bash -c 'mysql -p[REDACTED]'`),
		caseOf("bash -c 'htpasswd -b f admin PW # comment'", true, "bash -c 'htpasswd -b f admin [REDACTED] # comment'"),
		caseOf("bash -c 'echo hi # note#012mysql -u root -pX db'", true, "bash -c 'echo hi # note#012mysql -u root -p[REDACTED] db'"),
		caseOf("bash -c 'echo #x#012htpasswd -b f admin PW'", true, "bash -c 'echo #x#012htpasswd -b f admin [REDACTED]'"),
		caseOf("bash -c 'a=1;#c#012mysql -pX'", true, "bash -c 'a=1;#c#012mysql -p[REDACTED]'"),
		caseOf(`sh -c "cd /x && mysql -pX"`, true, `sh -c "cd /x && mysql -p[REDACTED]"`),
		caseOf("mysqldump -pDumpPW db", true, "mysqldump -p[REDACTED] db"),
		caseOf("mysqladmin -u root -pAdmPW status", true, "mysqladmin -u root -p[REDACTED] status"),
		caseOf("mysql_upgrade -pX", true, "mysql_upgrade -p[REDACTED]"),
		caseOf("htpasswd -b f user pw", true, "htpasswd -b f user [REDACTED]"),
		caseOf("htpasswd -cb f admin PW", true, "htpasswd -cb f admin [REDACTED]"),
		caseOf("htpasswd -bc f admin PW", true, "htpasswd -bc f admin [REDACTED]"),
		caseOf("htpasswd -nb admin PW", true, "htpasswd -nb admin [REDACTED]"),
		caseOf("htpasswd -Bbn admin PW", true, "htpasswd -Bbn admin [REDACTED]"),
		caseOf("htpasswd -b -c f admin PW", true, "htpasswd -b -c f admin [REDACTED]"),
		caseOf("htpasswd -B -b f admin PW", true, "htpasswd -B -b f admin [REDACTED]"),
		caseOf("htpasswd -b -C 10 f admin PW", true, "htpasswd -b -C 10 f admin [REDACTED]"),
		caseOf("htpasswd -b f admin PW; echo ok", true, "htpasswd -b f admin PW; echo [REDACTED]"),
		caseOf("htpasswd -b f admin PW | cat", true, "htpasswd -b f admin PW | [REDACTED]"),
		caseOf("bash -c 'htpasswd -b f admin PW > /dev/null'", true, "bash -c 'htpasswd -b f admin [REDACTED] > /dev/null'"),
		caseOf("bash -c 'htpasswd -b f admin PW 2>/dev/null'", true, "bash -c 'htpasswd -b f admin [REDACTED] 2>/dev/null'"),
		caseOf("bash -c 'htpasswd -b f admin PW >/dev/null'", true, "bash -c 'htpasswd -b f admin [REDACTED] >/dev/null'"),
		caseOf("bash -c 'htpasswd -nbB admin PWp | tee f'", true, "bash -c 'htpasswd -nbB admin [REDACTED] | tee f'"),
		caseOf("htpasswd -b f admin 'a b c'", true, "htpasswd -b f admin '[REDACTED]'"),
		caseOf("htpasswd -b f admin PW>/dev/null", true, "htpasswd -b f admin [REDACTED]"),
		caseOf("htpasswd -b f admin pa>ss", true, "htpasswd -b f admin [REDACTED]"),
		caseOf("htpasswd -b f admin 'pa>ss'", true, "htpasswd -b f admin '[REDACTED]'"),
		caseOf("sudo htpasswd -b f admin PWs", true, "sudo htpasswd -b f admin [REDACTED]"),
		caseOf("env X=1 htpasswd -b f admin PWe", true, "env X=1 htpasswd -b f admin [REDACTED]"),
		caseOf("nice mysql -pX", true, "nice mysql -p[REDACTED]"),
		caseOf("nohup mysql -pX", true, "nohup mysql -p[REDACTED]"),
		caseOf("nohup htpasswd -b f admin PW", true, "nohup htpasswd -b f admin [REDACTED]"),
		caseOf("ionice -c 3 -n 4 -t mysqldump -pX db", true, "ionice -c 3 -n 4 -t mysqldump -p[REDACTED] db"),
		caseOf("ionice -c2 -n7 mysql -pX db", true, "ionice -c2 -n7 mysql -p[REDACTED] db"),
		caseOf("ionice -n7 mysql -pX db", true, "ionice -n7 mysql -p[REDACTED] db"),
		caseOf("ionice --class=2 mysql -pX db", true, "ionice --class=2 mysql -p[REDACTED] db"),
		caseOf("ionice -c3 htpasswd -b f admin PW", true, "ionice -c3 htpasswd -b f admin [REDACTED]"),
		caseOf("ionice -p 123 mysql -pX", false, "ionice -p 123 mysql -pX"),
		caseOf("xargs -0 -r -n 2 -I X -P 4 -d , --show-limits mysql -pX", true, "xargs -0 -r -n 2 -I X -P 4 -d , --show-limits mysql -p[REDACTED]"),
		caseOf("xargs -I{} mysql -pX db", true, "xargs -I{} mysql -p[REDACTED] db"),
		caseOf("xargs -n1 -P4 mysql -pX", true, "xargs -n1 -P4 mysql -p[REDACTED]"),
		caseOf("xargs --max-args 1 mysql -pX", true, "xargs --max-args 1 mysql -p[REDACTED]"),
		caseOf("xargs -t mysql -pX", true, "xargs -t mysql -p[REDACTED]"),
		caseOf("xargs -L 1 htpasswd -b f a PW", true, "xargs -L 1 htpasswd -b f a [REDACTED]"),
		caseOf("xargs --max-args=2 --replace=X htpasswd -b f admin PW", true, "xargs --max-args=2 --replace=X htpasswd -b f admin [REDACTED]"),
		caseOf("docker exec -it -u app -e A=B -w /x --env=C=D db mysql -pX", true, "docker exec -it -u app -e A=B -w /x --env=C=D db mysql -p[REDACTED]"),
		caseOf("docker exec --user=root ctr mysql -pX", true, "docker exec --user=root ctr mysql -p[REDACTED]"),
		caseOf("docker exec -d ctr mysql -pX", true, "docker exec -d ctr mysql -p[REDACTED]"),
		caseOf("docker exec --privileged ctr mysql -pX", true, "docker exec --privileged ctr mysql -p[REDACTED]"),
		caseOf("docker exec -itu root ctr mysql -pX", true, "docker exec -itu root ctr mysql -p[REDACTED]"),
		caseOf("docker exec db htpasswd -b f admin PW", true, "docker exec db htpasswd -b f admin [REDACTED]"),
		caseOf("podman exec -i -t --user app db mysql -pX", true, "podman exec -i -t --user app db mysql -p[REDACTED]"),
		caseOf("podman exec db htpasswd -b f admin PW", true, "podman exec db htpasswd -b f admin [REDACTED]"),
		caseOf("docker run -u root ctr mysql -pX", false, "docker run -u root ctr mysql -pX"),
		caseOf("docker ps -a", false, "docker ps -a"),
		caseOf("podman run -u root ctr mysql -pX", false, "podman run -u root ctr mysql -pX"),
		caseOf("timeout 30 mysqldump -pX db", true, "timeout 30 mysqldump -p[REDACTED] db"),
		caseOf("sudo -u mysql mysql -pX", true, "sudo -u mysql mysql -p[REDACTED]"),
		caseOf("htpasswd -B -c f admin", false, "htpasswd -B -c f admin"),
		caseOf("sh -c 'cd /x && mysql -u root -pS3c'", true, "sh -c 'cd /x && mysql -u root -p[REDACTED]'"),
		caseOf(`bash -c "cd /x; mysql -pX"`, true, `bash -c "cd /x; mysql -p[REDACTED]"`),
		caseOf("mysql -u root -p'quoted pw' db", true, "mysql -u root '-p[REDACTED]' db"),
		caseOf("htpasswd -b /etc/nginx/.htpasswd admin 'my secret'", true, "htpasswd -b /etc/nginx/.htpasswd admin '[REDACTED]'"),
		caseOf("htpasswd -cb /etc/nginx/.htpasswd admin PWcb", true, "htpasswd -cb /etc/nginx/.htpasswd admin [REDACTED]"),
		caseOf("htpasswd -bc f admin PWbc", true, "htpasswd -bc f admin [REDACTED]"),
		caseOf("htpasswd -b -c f admin PWbc2", true, "htpasswd -b -c f admin [REDACTED]"),
		caseOf("htpasswd -B -b f admin PWBb", true, "htpasswd -B -b f admin [REDACTED]"),
		caseOf("htpasswd -bB f admin PWbB", true, "htpasswd -bB f admin [REDACTED]"),
		caseOf("htpasswd -nb admin PWnb", true, "htpasswd -nb admin [REDACTED]"),
		caseOf("htpasswd -Bbn admin PWBbn", true, "htpasswd -Bbn admin [REDACTED]"),
		caseOf("htpasswd -b -C 10 f admin PWC", true, "htpasswd -b -C 10 f admin [REDACTED]"),
		caseOf(`curl -H "X-Api-Key: 'v'" https://example.test`, true, `curl -H "X-Api-Key: '[REDACTED]'" https://example.test`),
		caseOf("curl -H 'X-Api-Key: abc123' https://example.test", true, "curl -H 'X-Api-Key: [REDACTED]' https://example.test"),
		caseOf(`curl -H "X-Api-Key: abc123" https://example.test`, true, `curl -H "X-Api-Key: [REDACTED]" https://example.test`),
		caseOf(`curl -H 'X-Api-Key: "abc"' https://example.test`, true, `curl -H 'X-Api-Key: "[REDACTED]"' https://example.test`),
		caseOf(`curl -H "X-Api-Key: 'abc" https://e`, true, `curl -H "X-Api-Key: '[REDACTED]" https://e`),
		caseOf(`curl -H 'X-Api-Key: ab"c' https://h`, true, `curl -H 'X-Api-Key: [REDACTED]' https://h`),
		caseOf("echo bearer of news", false, "echo bearer of news"),
		caseOf("/usr/bin/htpasswd -b f u PW", true, "/usr/bin/htpasswd -b f u [REDACTED]"),
		caseOf("env MYSQL_PWD=x mysql -pY", true, "env MYSQL_PWD=[REDACTED] mysql -p[REDACTED]"),
		caseOf("env -i mysql -pX", true, "env -i mysql -p[REDACTED]"),
		caseOf("mysql -u root -p\"q\" db", true, "mysql -u root -p\"[REDACTED]\" db"),
		caseOf(`sh -c "sudo -u mysql mysql -pX"`, true, `sh -c "sudo -u mysql mysql -p[REDACTED]"`),
		caseOf("exec mysql -pX", true, "exec mysql -p[REDACTED]"),
		caseOf("htpasswd -b f admin 'my secret'", true, "htpasswd -b f admin '[REDACTED]'"),
		caseOf("htpasswd -b f admin \"my secret\"", true, "htpasswd -b f admin \"my [REDACTED]"),
		caseOf(`curl -H "X-Api-Key: 'v'" https://h`, true, `curl -H "X-Api-Key: '[REDACTED]'" https://h`),
		caseOf("curl -H X-Api-Key:'abcdZ", true, "curl -H X-Api-Key:'[REDACTED]"),
		caseOf("curl --oauth2-bearer=TOKEN123 https://e", true, "curl --oauth2-bearer=[REDACTED] https://e"),
		caseOf("bash -euo pipefail -c '#012cd /x#012mysql -pX db#012'", true, "bash -euo pipefail -c '#012cd /x#012mysql -p[REDACTED] db#012'"),
		caseOf("bash -c 'cd /x#012mysqldump -pX db > /b.sql'", true, "bash -c 'cd /x#012mysqldump -p[REDACTED] db > /b.sql'"),
		caseOf("bash -c 'cd /x; mysql -pX'", true, "bash -c 'cd /x; mysql -p[REDACTED]'"),
		caseOf("bash -c 'htpasswd -b f admin PW; echo ok'", true, "bash -c 'htpasswd -b f admin [REDACTED]; echo ok'"),
		caseOf("htpasswd -b f admin PW extra", true, "htpasswd -b f admin PW [REDACTED]"),
		caseOf("mysql -u root -p'it''s' db", true, "mysql -u root '-p[REDACTED]' db"),
		caseOf("bash -c 'htpasswd -b f admin PW#012systemctl reload nginx'", true, "bash -c 'htpasswd -b f admin [REDACTED]#012systemctl reload nginx'"),
		caseOf("bash -c 'mysql -pX#012echo ok'", true, "bash -c 'mysql -p[REDACTED]#012echo ok'"),
		caseOf("bash -c 'echo a#012mysql -pX'", true, "bash -c 'echo a#012mysql -p[REDACTED]'"),
		caseOf("bash -c 'cd /x && htpasswd -b f a PWc'", true, "bash -c 'cd /x && htpasswd -b f a [REDACTED]'"),
		caseOf("htpasswd -b f admin \"a b c\"", true, "htpasswd -b f admin \"a b [REDACTED]"),
		caseOf("MYSQL_PWD=MySQLPass mysql", true),
		caseOf("docker login -p DockPass registry.example", true),
		caseOf("mariadb-dump -pDumpPW", true, "mariadb-dump -p[REDACTED]"),
		caseOf("tool --password secret", true),
		caseOf("tool --password=secret", true),
		caseOf("tool --token abc123", true),
		caseOf("tool --token=abc123", true),
		caseOf("tool --api-key abc123", true),
		caseOf("tool --api-key=abc123", true),
		caseOf("tool --secret abc123", true),
		caseOf("tool --secret=abc123", true),
		caseOf("curl --oauth2-bearer TOKEN123", true, "curl --oauth2-bearer [REDACTED]"),
		caseOf("curl -u user:pass https://example.test", true),
		caseOf("curl -uuser:pass https://example.test", true),
		caseOf("curl --user=user:pass https://example.test", true),
		caseOf("curl -H 'Authorization: Basic dXNlcjpwYXNz' https://example.test", true),
		caseOf("curl -H 'Authorization: Bearer abc123' https://example.test", true, "curl -H 'Authorization: Bearer [REDACTED]' https://example.test"),
		caseOf("curl -H 'X-Api-Key: abc123' https://h", true, "curl -H 'X-Api-Key: [REDACTED]' https://h"),
		caseOf(`curl -H "X-Api-Key: abc123" https://h`, true, `curl -H "X-Api-Key: [REDACTED]" https://h`),
		caseOf(`curl -H 'Authorization: Bearer "a b"' https://example.test`, true, `curl -H 'Authorization: Bearer "[REDACTED]"' https://example.test`),
		caseOf("tool --access-token abc123", true),
		caseOf("tool --passphrase abc123", true),
		caseOf("curl -H 'Authorization: Bearer abc123'", true),
		caseOf("git clone https://user:ghp_tok@github.com/x", true),
		caseOf("psql postgresql://u:pgpw@h/db", true),
		caseOf("curl https://example.test:8443/x", false),
		caseOf("mkdir -p /home/aisee/.cache/x", false),
		caseOf("ssh -p 2222 host", false),
		caseOf("cp -p source destination", false),
		caseOf("find . -perm 0644", false),
		caseOf("find /var/lib/mysql -perm 0644", false, "find /var/lib/mysql -perm 0644"),
		caseOf("ls /usr/share/htpasswd -b x y z", false, "ls /usr/share/htpasswd -b x y z"),
		caseOf("tool -p abc123", false),
		caseOf("tar -pxf a.tar", false),
		caseOf("tar -C /var/lib/mysql -pxf a.tar", false, "tar -C /var/lib/mysql -pxf a.tar"),
		// Corpus rows whose output intentionally differs from the older shell-like parser.
		caseOf("mysql -u root -pX'", true, "mysql -u root '-p[REDACTED]'"),
		caseOf("mysql -pab;cd db", true, "mysql -p[REDACTED] db"),
		caseOf("htpasswd -b f admin ab|cd", true, "htpasswd -b f admin [REDACTED]"),
		caseOf("htpasswd -b f admin ab;cd", true, "htpasswd -b f admin [REDACTED]"),
		caseOf("mysql -pab\\'cd db", true, "mysql -p[REDACTED] db"),
		caseOf("mysql -p'a b\\'c' db", true, "mysql '-p[REDACTED]' db"),
		caseOf("bash -c 'mysql -pX&&echo ok'", true, "bash -c 'mysql -p[REDACTED]&&echo ok'"),
		caseOf("bash -c 'cd /x&&mysql -pX||echo fail'", true, "bash -c 'cd /x&&mysql -p[REDACTED]||echo fail'"),
		caseOf("bash -c 'true||mysql -pX'", true, "bash -c 'true||mysql -p[REDACTED]'"),
		caseOf("bash -c 'mysql -pX>/tmp/o'", true, "bash -c 'mysql -p[REDACTED]>/tmp/o'"),
		caseOf("bash -c 'sh -c \"cd /x && htpasswd -b f a PWn\"'", true, "bash -c 'sh -c \"cd /x && htpasswd -b f a [REDACTED]\"'"),
		caseOf("bash -lc 'mysql -pX'", true, "bash -lc 'mysql -p[REDACTED]'"),
		caseOf("bash -ec 'mysql -pX'", true, "bash -ec 'mysql -p[REDACTED]'"),
		caseOf("sudo -E -u root mysqldump -pX db", true, "sudo -E -u root mysqldump -p[REDACTED] db"),
		caseOf("env -i PATH=/bin mysql -pX", true, "env -i PATH=/bin mysql -p[REDACTED]"),
		caseOf("env MYSQL_HOST=h mysql -pX", true, "env MYSQL_HOST=h mysql -p[REDACTED]"),
		caseOf("nice -n 10 mysql -pX", true, "nice -n 10 mysql -p[REDACTED]"),
		caseOf("timeout -k 5 30 mysql -pX", true, "timeout -k 5 30 mysql -p[REDACTED]"),
		caseOf("timeout -s KILL 30 mysql -pX", true, "timeout -s KILL 30 mysql -p[REDACTED]"),
		caseOf("htpasswd -i -b f admin", true, "htpasswd -i -b f [REDACTED]"),
		caseOf("htpasswd -bi f admin PWi", true, "htpasswd -bi f admin [REDACTED]"),
		caseOf("htpasswd -b f admin PW # comment", true, "htpasswd -b f admin PW # [REDACTED]"),
		caseOf("sudo -p prompt mysql -pX", true, "sudo -p prompt mysql -p[REDACTED]"),
	}
	seenCommands := make(map[string]bool, len(cases))
	for _, tc := range cases {
		if seenCommands[tc.command] {
			t.Fatalf("duplicate sudo masking test input: %q", tc.command)
		}
		seenCommands[tc.command] = true
		t.Run(tc.command, func(t *testing.T) {
			got := maskSudoSecrets(tc.command)
			if tc.want != "" && got != tc.want {
				t.Fatalf("output=%q, want %q", got, tc.want)
			}
			redacted := strings.Contains(got, "[REDACTED]")
			if redacted != tc.masked {
				t.Fatalf("masked=%v, want %v: %q", redacted, tc.masked, got)
			}
			for _, secret := range []string{"S3cretPW", "MySQLPass", "DockPass", "DumpPW", "AdmPW", "TOKEN123", "PW", "X", "ab&cd", "ab|cd", "ab>cd", "abc123", "user:pass", "dXNlcjpwYXNz", "2222", "ghp_tok", "pgpw"} {
				if secret == "X" || secret == "PW" { // Короткие секреты проверяются по полному ожидаемому выводу.
					continue
				}
				if tc.masked && strings.Contains(got, secret) {
					t.Fatalf("secret %q leaked in %q", secret, got)
				}
			}
		})
	}
	long, ok := parseSudoMessage("aisee : USER=root ; COMMAND=" + strings.Repeat("x", 700))
	if !ok || len(long.Command) > 512 {
		t.Fatalf("long command not bounded: %d", len(long.Command))
	}

	// Рост, а не часы: под -race и нагрузкой абсолютный предел ложно краснеет. Линейный проход даёт ~10×
	// на десятикратный вход, квадратичный (маска до конца строки после каждого ` ; `) — ~100×.
	fastest := func(n int) time.Duration {
		command := strings.Repeat("mysql -pX ; ", n)
		best := time.Duration(1<<63 - 1)
		for range 3 {
			started := time.Now()
			_ = maskSudoSecrets(command)
			best = min(best, time.Since(started))
		}
		return best
	}
	small, large := fastest(1_000), fastest(10_000)
	if ratio := float64(large) / float64(max(small, time.Microsecond)); ratio > 30 {
		t.Fatalf("masking grows superlinearly: 1,000 separated commands %s, 10,000 — %s (×%.0f, limit ×30)", small, large, ratio)
	}
}

func TestIOReportCountsAllRootSessionCandidates(t *testing.T) {
	a, out := testIOApp(t)
	a.sudoJournalSource = func(context.Context, string, string, ...string) ([]journalRecord, error) {
		return nil, nil
	}
	now := time.Now()
	cases := []iowatch.Case{
		{Start: now.Add(-2 * time.Hour), DurMS: 3000, Culprits: &iowatch.Culprit{Main: "session-c497.scope"}, Sudo: &iowatch.SudoInfo{Command: "matched"}},
		{Start: now.Add(-time.Hour), DurMS: 4000, Culprits: &iowatch.Culprit{Main: "session-c498.scope"}},
	}
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := iowatch.SaveCases(path, cases); err != nil {
		t.Fatal(err)
	}
	if err := a.ioReport(context.Background(), path, false, now.Add(-24*time.Hour), now, 0, "", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "root session-c*: сопоставлено 1 из 2 случаев") {
		t.Fatalf("root session summary did not count unmatched candidates: %s", out.String())
	}
}

func TestSudoSessionRootScopeDoesNotMatchUserSession(t *testing.T) {
	if rootSessionRE.MatchString("/user.slice/user-42.slice/session-c53.scope") {
		t.Fatal("non-root session matched as root")
	}
}

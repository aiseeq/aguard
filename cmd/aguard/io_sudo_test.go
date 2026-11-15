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

func TestParseSudoMessageOptionalTTYAndCommandSemicolons(t *testing.T) {
	info, ok := parseSudoMessage("aisee : PWD=/tmp ; USER=root ; COMMAND=/usr/bin/sh -c 'echo a ; echo b'")
	if !ok || info.TTY != "" || info.Command != "sh -c 'echo a ; echo b'" {
		t.Fatalf("parsed sudo row: ok=%v info=%+v", ok, info)
	}
}

func TestSudoSecretMaskingAndLimit(t *testing.T) {
	cases := []struct {
		command string
		masked  bool
	}{
		{"mysql -pS3cretPW", true},
		{"bash -c 'mysql -pX'", true},
		{"MYSQL_PWD=MySQLPass mysql", true},
		{"docker login -p DockPass registry.example", true},
		{"mariadb-dump -pDumpPW", true},
		{"tool --password secret", true},
		{"tool --password=secret", true},
		{"tool --token abc123", true},
		{"tool --token=abc123", true},
		{"tool --api-key abc123", true},
		{"tool --api-key=abc123", true},
		{"tool --secret abc123", true},
		{"tool --secret=abc123", true},
		{"curl -u user:pass https://example.test", true},
		{"curl -uuser:pass https://example.test", true},
		{"curl --user=user:pass https://example.test", true},
		{"curl -H 'Authorization: Basic dXNlcjpwYXNz' https://example.test", true},
		{"curl -H 'Authorization: Bearer abc123' https://example.test", true},
		{"curl -H 'X-Api-Key: abc123' https://example.test", true},
		{"tool --access-token abc123", true},
		{"tool --passphrase abc123", true},
		{"curl -H 'Authorization: Bearer abc123'", true},
		{"git clone https://user:ghp_tok@github.com/x", true},
		{"psql postgresql://u:pgpw@h/db", true},
		{"curl https://example.test:8443/x", false},
		{"mkdir -p /home/aisee/.cache/x", false},
		{"ssh -p 2222 host", false},
		{"tool -p abc123", false},
		{"tar -pxf a.tar", false},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			got := maskSudoSecrets(tc.command)
			redacted := strings.Contains(got, "[REDACTED]")
			if redacted != tc.masked {
				t.Fatalf("masked=%v, want %v: %q", redacted, tc.masked, got)
			}
			for _, secret := range []string{"S3cretPW", "MySQLPass", "DockPass", "DumpPW", "abc123", "user:pass", "dXNlcjpwYXNz", "2222", "ghp_tok", "pgpw"} {
				if tc.masked && strings.Contains(got, secret) {
					t.Fatalf("secret %q leaked in %q", secret, got)
				}
			}
			if strings.Contains(tc.command, "Authorization:") && !strings.Contains(got, "[REDACTED]'") {
				t.Fatalf("authorization masking removed the shell quote: %q", got)
			}
		})
	}
	long, ok := parseSudoMessage("aisee : USER=root ; COMMAND=" + strings.Repeat("x", 700))
	if !ok || len(long.Command) > 512 {
		t.Fatalf("long command not bounded: %d", len(long.Command))
	}
}

func TestSudoSessionRootScopeDoesNotMatchUserSession(t *testing.T) {
	if rootSessionRE.MatchString("/user.slice/user-42.slice/session-c53.scope") {
		t.Fatal("non-root session matched as root")
	}
}

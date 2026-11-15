package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aiseeq/aguard/internal/iowatch"
	"github.com/aiseeq/akit/proc"
)

var (
	rootSessionRE        = regexp.MustCompile(`^/user\.slice/user-0\.slice/session-c([0-9]+)\.scope$`)
	storedSessionRE      = regexp.MustCompile(`^session-c([0-9]+)\.scope$`)
	secretOptionRE       = regexp.MustCompile(`(?i)(--(?:password|passphrase|token|access-token|api-key|secret)(?:=|\s+)|(?:password|passphrase|token|access[_-]?token|secret|api[_-]?key)=)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	mysqlPasswordRE      = regexp.MustCompile(`(?i)(\s-p)([^\s;]+)`)
	dockerPasswordRE     = regexp.MustCompile(`(?i)(\bdocker\s+login\b[^;]*?\s-p(?:\s+|=))("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	curlUserPasswordRE   = regexp.MustCompile(`(?i)(\bcurl\b[^;]*?\s(?:-u\s*|--user(?:=|\s+)))("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	shellCommandRE       = regexp.MustCompile(`(?i)(\b(?:bash|sh|zsh)\b[^;]*?\s-c\s+)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')`)
	authorizationValueRE = regexp.MustCompile(`(?i)(authorization:\s*[a-z]+\s+|bearer\s+)(?:"([^"]*)"|'([^']*)'|([^\s;"']+))`)
	apiKeyHeaderRE       = regexp.MustCompile(`(?i)((?:x-)?api-key:\s*)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	mysqlPwdEnvRE        = regexp.MustCompile(`(?i)(\bMYSQL_PWD=)("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s;]+)`)
	// urlPasswordRE — пароль в URL: scheme://user:pass@host; host:port без @ не трогает.
	urlPasswordRE = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@;'"]+:)([^@\s;'"/]+)(@)`)
)

const (
	sudoJournalTimeout       = 2 * time.Second
	sudoHistoricalWindow     = 12 * time.Hour
	sudoLiveWindow           = 24 * time.Hour
	sudoNegativeCacheWindow  = 5 * time.Minute
	sudoLogindAssociationGap = 5 * time.Second
)

type sudoCacheEntry struct {
	checkedAt time.Time
	info      *iowatch.SudoInfo
	bootID    string
}

type sudoLookupCache struct {
	mu        sync.Mutex
	bySession map[string][]sudoCacheEntry
}

func newSudoLookupCache() *sudoLookupCache {
	return &sudoLookupCache{bySession: make(map[string][]sudoCacheEntry)}
}

func (c *sudoLookupCache) find(number, bootID string, at time.Time) (*iowatch.SudoInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var found *sudoCacheEntry
	entries := c.bySession[number]
	for i := range entries {
		entry := &entries[i]
		if bootID != "" && entry.bootID != bootID {
			continue
		}
		if entry.info == nil {
			if at.Sub(entry.checkedAt).Abs() <= sudoNegativeCacheWindow && (found == nil || entry.checkedAt.After(found.checkedAt)) {
				found = entry
			}
			continue
		}
		if entry.checkedAt.After(at) {
			continue
		}
		if bootID == "" && (entry.info.ClosedAt.IsZero() || at.Before(entry.info.OpenedAt) || !at.Before(entry.info.ClosedAt)) {
			continue
		}
		if found == nil || entry.checkedAt.After(found.checkedAt) {
			found = entry
		}
	}
	if found == nil {
		return nil, false
	}
	if found.info == nil {
		return nil, true
	}
	info := *found.info
	return &info, true
}

func (c *sudoLookupCache) add(number string, checkedAt time.Time, bootID string, info *iowatch.SudoInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.bySession[number]
	kept := entries[:0]
	for _, old := range entries {
		if old.info == nil && checkedAt.Sub(old.checkedAt) > sudoNegativeCacheWindow {
			continue
		}
		kept = append(kept, old)
	}
	entries = kept
	for i := range entries {
		old := &entries[i]
		if old.checkedAt.Equal(checkedAt) && old.bootID == bootID {
			c.bySession[number] = entries
			return
		}
	}
	var copied *iowatch.SudoInfo
	if info != nil {
		value := *info
		copied = &value
	}
	c.bySession[number] = append(entries, sudoCacheEntry{checkedAt: checkedAt, bootID: bootID, info: copied})
}

// sudoInfo enriches only root cgroups, preserves loaded attribution and handles main and foreign sessions independently.
func (a *app) sudoInfo(ctx context.Context, c *iowatch.Case, cache *sudoLookupCache, live bool) error {
	if c == nil || c.Culprits == nil {
		return nil
	}
	var lookupErrors []error
	for _, target := range []struct {
		path string
		dst  **iowatch.SudoInfo
	}{
		{caseSessionPath(c.Culprits, false), &c.Sudo}, {caseSessionPath(c.Culprits, true), &c.ForeignSudo},
	} {
		if *target.dst != nil {
			continue
		}
		number, fullRootPath := sessionCandidate(target.path)
		if number == "" {
			continue
		}
		info, cached := cache.find(number, c.BootID, c.Start)
		if cached {
			if info != nil {
				*target.dst = info
			}
			continue
		}
		if info == nil {
			var err error
			info, err = a.lookupSudoSession(ctx, number, c.BootID, c.Start, live && fullRootPath)
			if err != nil {
				lookupErrors = append(lookupErrors, fmt.Errorf("session-c%s: %w", number, err))
				continue
			}
			cache.add(number, c.Start, infoBootID(info, c.BootID), info)
		}
		if info != nil {
			*target.dst = info
		}
	}
	return errors.Join(lookupErrors...)
}

func caseSessionPath(c *iowatch.Culprit, foreign bool) string {
	if c == nil {
		return ""
	}
	if !foreign {
		return c.Main
	}
	if c.Foreign.Name == "" {
		return ""
	}
	if path := c.CgroupPaths[c.Foreign.Name]; path != "" {
		return path
	}
	return c.Foreign.Name
}

func rootSessionCandidatePath(path string) bool {
	return rootSessionRE.MatchString(path) || storedSessionRE.MatchString(path)
}

func hasRootSessionCandidate(c *iowatch.Case) bool {
	return c != nil && c.Culprits != nil && (rootSessionCandidatePath(caseSessionPath(c.Culprits, false)) || rootSessionCandidatePath(caseSessionPath(c.Culprits, true)))
}

func sessionCandidate(path string) (number string, fullRootPath bool) {
	if match := rootSessionRE.FindStringSubmatch(path); match != nil {
		return match[1], true
	}
	if match := storedSessionRE.FindStringSubmatch(path); match != nil {
		return match[1], false
	}
	return "", false
}

func infoBootID(info *iowatch.SudoInfo, fallback string) string {
	if info != nil && info.BootID != "" {
		return info.BootID
	}
	return fallback
}

func (a *app) lookupSudoSession(parent context.Context, number, caseBootID string, at time.Time, live bool) (*iowatch.SudoInfo, error) {
	until := at.Add(sudoHistoricalWindow).Format(time.RFC3339)
	if live {
		if info := a.liveSudoSession(parent, number, caseBootID, at, until); info != nil {
			return info, nil
		}
	}
	since := at.Add(-sudoHistoricalWindow).Format(time.RFC3339)
	sudoRows, err := a.readSudoJournal(parent, since, until, "_COMM=sudo")
	if err != nil {
		return nil, err
	}
	if info := sudoForExactSession(a.log, number, caseBootID, at, sudoRows); info != nil {
		return info, nil
	}
	logindRows, err := a.readSudoJournal(parent, since, until, "_SYSTEMD_UNIT=systemd-logind.service")
	if err != nil {
		return nil, err
	}
	return sudoForLogindSession(a.log, number, caseBootID, at, sudoRows, logindRows), nil
}

func (a *app) liveSudoSession(ctx context.Context, number, caseBootID string, at time.Time, until string) *iowatch.SudoInfo {
	pids, err := a.sessionPIDs(number)
	if err != nil || len(pids) == 0 {
		a.debugSudoError("read live sudo session PIDs", err)
		return nil
	}
	bootID, err := a.systemBootID()
	if err != nil || (caseBootID != "" && caseBootID != bootID) {
		a.debugSudoError("read live sudo session boot ID", err)
		return nil
	}
	rows, err := a.readSudoJournal(ctx, at.Add(-sudoLiveWindow).Format(time.RFC3339), until, sudoPIDFilters(pids)...)
	if err != nil {
		a.debugSudoError("read live sudo session journal", err)
		return nil
	}
	return liveSudoForPIDs(a.log, number, rows, pids, bootID, at)
}

func sudoPIDFilters(pids []int) []string {
	if len(pids) == 0 {
		return []string{"_COMM=sudo"}
	}
	filters := make([]string, 0, 3*len(pids))
	for i, pid := range pids {
		if i > 0 {
			filters = append(filters, "+")
		}
		filters = append(filters, "_COMM=sudo", "_PID="+strconv.Itoa(pid))
	}
	return filters
}

func (a *app) readSudoJournal(ctx context.Context, since, until string, filters ...string) ([]journalRecord, error) {
	callCtx, cancel := context.WithTimeout(ctx, sudoJournalTimeout)
	defer cancel()
	if a.sudoJournalSource != nil {
		return a.sudoJournalSource(callCtx, since, until, filters...)
	}
	return readJournal(callCtx, since, until, filters...)
}

func readJournal(parent context.Context, since, until string, filters ...string) ([]journalRecord, error) {
	ctx, cancel := context.WithTimeout(parent, sudoJournalTimeout)
	defer cancel()
	args := append([]string{"--no-pager", "-o", "json", "--since", since, "--until", until}, filters...)
	b, err := exec.CommandContext(ctx, "journalctl", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	return parseJournalJSON(string(b))
}

func liveSudoForPIDs(logger *slog.Logger, number string, rows []journalRecord, pids []int, bootID string, at time.Time) *iowatch.SudoInfo {
	pidSet := make(map[string]bool, len(pids))
	for _, pid := range pids {
		pidSet[strconv.Itoa(pid)] = true
	}
	var opened *journalRecord
	for i := range rows {
		row := &rows[i]
		if !pidSet[row.PID] || row.BootID != bootID || !strings.Contains(row.Message, "COMMAND=") {
			continue
		}
		rowAt, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "live sudo command", err)
			continue
		}
		if rowAt.After(at) {
			continue
		}
		if opened == nil || journalTimestampBefore(opened.Timestamp, row.Timestamp) {
			opened = row
		}
	}
	if opened == nil {
		return nil
	}
	info, ok := parseSudoMessage(opened.Message)
	if !ok {
		return nil
	}
	info.Session, info.BootID = "c"+number, bootID
	openedAt, err := journalTimestamp(opened.Timestamp)
	if err != nil {
		debugJournalTimestamp(logger, "live sudo command timestamp", err)
		return nil
	}
	info.OpenedAt = openedAt
	info.Owner, info.OwnerLabel = sudoOwnerFromCgroup(opened.CGroup)
	return &info
}

func sudoForExactSession(logger *slog.Logger, number, bootID string, at time.Time, rows []journalRecord) *iowatch.SudoInfo {
	want := "c" + number
	var selected *journalRecord
	for i := range rows {
		row := &rows[i]
		if (bootID != "" && row.BootID != bootID) || !isRootSessionOpen(*row, want) || (row.Session != want && !strings.HasSuffix(row.CGroup, "/session-"+want+".scope")) {
			continue
		}
		opened, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "sudo PAM session", err)
			continue
		}
		if opened.After(at) {
			continue
		}
		if selected == nil || journalTimestampBefore(selected.Timestamp, row.Timestamp) {
			selected = row
		}
	}
	if selected == nil {
		return nil
	}
	info := sudoCommandForPID(logger, rows, selected.PID, selected.BootID, selected.Timestamp)
	if info != nil {
		info.Session, info.BootID = want, selected.BootID
		info.ClosedAt = sudoPAMSessionClose(logger, rows, selected.PID, selected.BootID, selected.Timestamp)
	}
	return info
}

func sudoForLogindSession(logger *slog.Logger, number, bootID string, at time.Time, sudoRows, logindRows []journalRecord) *iowatch.SudoInfo {
	want := "c" + number
	var event *journalRecord
	for i := range logindRows {
		row := &logindRows[i]
		if (bootID != "" && row.BootID != bootID) || !isRootLogindSession(row.Message, want) {
			continue
		}
		t, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "logind session", err)
			continue
		}
		if t.After(at) {
			continue
		}
		if event == nil || journalTimestampBefore(event.Timestamp, row.Timestamp) {
			event = row
		}
	}
	if event == nil {
		return nil
	}
	var opened *journalRecord
	best := sudoLogindAssociationGap + time.Nanosecond
	for i := range sudoRows {
		row := &sudoRows[i]
		if !strings.Contains(row.Message, "session opened for user root") || row.BootID != event.BootID {
			continue
		}
		t, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "sudo PAM session", err)
			continue
		}
		eventAt, err := journalTimestamp(event.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "logind session", err)
			continue
		}
		distance := t.Sub(eventAt).Abs()
		if distance <= sudoLogindAssociationGap && distance < best {
			opened, best = row, distance
		}
	}
	if opened == nil {
		return nil
	}
	info := sudoCommandForPID(logger, sudoRows, opened.PID, opened.BootID, opened.Timestamp)
	if info != nil {
		info.Session, info.BootID = want, opened.BootID
		info.ClosedAt = logindSessionClose(logger, logindRows, want, opened.BootID, event.Timestamp)
	}
	return info
}

func sudoPAMSessionClose(logger *slog.Logger, rows []journalRecord, pid, bootID, openedAt string) time.Time {
	var closed time.Time
	for i := range rows {
		row := &rows[i]
		if row.PID != pid || row.BootID != bootID || !strings.Contains(row.Message, "session closed for user root") {
			continue
		}
		at, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "sudo PAM session close", err)
			continue
		}
		if journalTimestampBefore(row.Timestamp, openedAt) || !closed.IsZero() && !at.Before(closed) {
			continue
		}
		closed = at
	}
	return closed
}

func logindSessionClose(logger *slog.Logger, rows []journalRecord, want, bootID, openedAt string) time.Time {
	var closed time.Time
	for i := range rows {
		row := &rows[i]
		if row.BootID != bootID || !isRemovedLogindSession(row.Message, want) || journalTimestampBefore(row.Timestamp, openedAt) {
			continue
		}
		at, err := journalTimestamp(row.Timestamp)
		if err != nil {
			debugJournalTimestamp(logger, "logind session close", err)
			continue
		}
		if closed.IsZero() || at.Before(closed) {
			closed = at
		}
	}
	return closed
}

func isRemovedLogindSession(message, want string) bool {
	return strings.Contains(message, "Removed session "+want+".") || strings.Contains(message, "Removed session '"+want+"'")
}

func isRootLogindSession(message, want string) bool {
	prefix := "New session '" + want + "' of user "
	if !strings.HasPrefix(message, prefix) {
		return false
	}
	user := strings.TrimPrefix(message, prefix)
	fields := strings.Fields(user)
	return len(fields) > 0 && strings.Trim(fields[0], "'.") == "root"
}

func isRootSessionOpen(row journalRecord, want string) bool {
	return strings.Contains(row.Message, "session opened for user root") &&
		(row.Session == want || strings.HasSuffix(row.CGroup, "/session-"+want+".scope"))
}

func sudoCommandForPID(logger *slog.Logger, rows []journalRecord, pid, bootID, noLaterThan string) *iowatch.SudoInfo {
	var selected *journalRecord
	for i := range rows {
		row := &rows[i]
		if row.PID != pid || row.BootID != bootID || !strings.Contains(row.Message, "COMMAND=") || journalTimestampBefore(noLaterThan, row.Timestamp) {
			continue
		}
		if selected == nil || journalTimestampBefore(selected.Timestamp, row.Timestamp) {
			selected = row
		}
	}
	if selected == nil {
		return nil
	}
	info, ok := parseSudoMessage(selected.Message)
	if !ok {
		return nil
	}
	info.Session, info.BootID = selected.Session, selected.BootID
	openedAt, err := journalTimestamp(noLaterThan)
	if err != nil {
		debugJournalTimestamp(logger, "sudo command session", err)
		return nil
	}
	info.OpenedAt = openedAt
	info.Owner, info.OwnerLabel = sudoOwnerFromCgroup(selected.CGroup)
	return &info
}

func sudoOwnerFromCgroup(cgroup string) (string, string) {
	if strings.Contains(cgroup, "/agents.slice/") {
		if name, _, ok := proc.SessionScope(cgroup); ok {
			return name, name
		}
	}
	if strings.Contains(cgroup, "app-org.kde.konsole-") || strings.Contains(cgroup, "app-far2l@") {
		return "", "терминал пользователя (konsole/far2l)"
	}
	return "", "запуск root"
}

func parseSudoMessage(message string) (iowatch.SudoInfo, bool) {
	parts := strings.Split(strings.TrimSpace(message), " ; ")
	values := make(map[string]string, 4)
	user := ""
	command := ""
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if i == 0 {
			if before, after, ok := strings.Cut(part, " : "); ok {
				user, part = strings.TrimSpace(before), after
			}
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "COMMAND" {
			command = value
			if i+1 < len(parts) {
				command += " ; " + strings.Join(parts[i+1:], " ; ")
			}
			break
		}
		values[key] = value
	}
	if values["USER"] != "root" || command == "" {
		return iowatch.SudoInfo{}, false
	}
	for _, prefix := range []string{"/usr/bin/", "/usr/sbin/"} {
		command = strings.TrimPrefix(command, prefix)
	}
	return iowatch.SudoInfo{Command: truncateSudo(maskSudoSecrets(command), 512), TTY: values["TTY"], PWD: values["PWD"], User: user}, true
}

func maskSudoSecrets(command string) string {
	command = shellCommandRE.ReplaceAllStringFunc(command, func(value string) string {
		parts := shellCommandRE.FindStringSubmatch(value)
		quoted := parts[2]
		quote := quoted[:1]
		inner := quoted[1 : len(quoted)-1]
		return parts[1] + quote + maskSudoSecrets(inner) + quote
	})
	command = authorizationValueRE.ReplaceAllStringFunc(command, func(value string) string {
		parts := authorizationValueRE.FindStringSubmatch(value)
		if parts[2] != "" || strings.Contains(value, `"`) {
			return parts[1] + `"[REDACTED]"`
		}
		if parts[3] != "" || strings.Contains(value, "'") {
			return parts[1] + `'[REDACTED]'`
		}
		return parts[1] + "[REDACTED]"
	})
	command = apiKeyHeaderRE.ReplaceAllString(command, "$1[REDACTED]")
	command = mysqlPwdEnvRE.ReplaceAllString(command, "$1[REDACTED]")
	command = urlPasswordRE.ReplaceAllString(command, "$1[REDACTED]$3")
	command = dockerPasswordRE.ReplaceAllString(command, "$1[REDACTED]")
	command = curlUserPasswordRE.ReplaceAllString(command, "$1[REDACTED]")
	command = secretOptionRE.ReplaceAllString(command, "$1[REDACTED]")
	if mysqlClient(command) {
		command = mysqlPasswordRE.ReplaceAllString(command, "$1[REDACTED]")
	}
	return command
}

func mysqlClient(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	name := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	return strings.HasPrefix(name, "mysql") || strings.HasPrefix(name, "mariadb")
}

func truncateSudo(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes - len("…")
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

func (a *app) sessionPIDs(number string) ([]int, error) {
	path := filepath.Join(a.cgroupRoot(), "user.slice/user-0.slice/session-c"+number+".scope/cgroup.procs")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sudo session cgroup PIDs: %w", err)
	}
	var pids []int
	for field := range strings.FieldsSeq(string(b)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("parse sudo session PID %q: %w", field, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func journalTimestampBefore(a, b string) bool {
	ta, errA := journalTimestamp(a)
	tb, errB := journalTimestamp(b)
	return errA == nil && errB == nil && ta.Before(tb)
}

func (a *app) cgroupRoot() string {
	if a.cgroupDir != "" {
		return a.cgroupDir
	}
	return "/sys/fs/cgroup"
}

func (a *app) procRoot() string {
	if a.procDir != "" {
		return a.procDir
	}
	return "/proc"
}

func (a *app) debugSudoError(action string, err error) {
	if err != nil && a.log != nil {
		a.log.Debug("aguard io sudo lookup", "action", action, "err", err)
	}
}

func debugJournalTimestamp(logger *slog.Logger, action string, err error) {
	if err != nil && logger != nil {
		logger.Debug("aguard io: invalid journal timestamp", "record", action, "err", err)
	}
}

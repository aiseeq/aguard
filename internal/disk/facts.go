package disk

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// verdictLine — строка сухого прогона graft worktree clean: «graft: <вердикт> <путь> (<ветка>): <почему>».
var verdictLine = regexp.MustCompile(`^graft: (would remove|check|in work|unmerged)\s+(\S+) \(([^)]*)\): (.*)$`)

// ParseVerdicts — вердикты graft worktree clean (без -apply) по путям worktree.
func ParseVerdicts(out []byte, into map[string]Verdict) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if m := verdictLine.FindStringSubmatch(sc.Text()); m != nil {
			into[m[2]] = Verdict{Branch: m[3], Word: m[1], Reason: m[4]}
		}
	}
}

// WorktreeRepos — репозитории, чьи worktree есть среди записей, и их worktree.
func WorktreeRepos(list []string) (map[string][]string, error) {
	out := map[string][]string{}
	var errs []error
	for _, p := range list {
		if isScratchpad(p) {
			continue
		}
		repo, err := worktreeRepo(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if repo != "" {
			out[repo] = append(out[repo], p)
		}
	}
	return out, errors.Join(errs...)
}

// NoVerdict — graft не судит worktree (нет remote и т. п.): след, только если давно не менялся.
const NoVerdict = "без вердикта"

// Btime — время создания по statx; файловая система его не знает — ноль без ошибки.
func Btime(path string) (time.Time, error) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &st); err != nil {
		return time.Time{}, fmt.Errorf("statx %s: %w", path, err)
	}
	if st.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, nil
	}
	return time.Unix(st.Btime.Sec, int64(st.Btime.Nsec)), nil
}

// SessionID — id сессии по пути транскрипта «<папка проекта>/<id>.jsonl».
func SessionID(rel string) string {
	base, ok := strings.CutSuffix(rel[strings.LastIndexByte(rel, '/')+1:], ".jsonl")
	if !ok {
		return ""
	}
	return base
}

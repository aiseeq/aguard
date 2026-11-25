package prune

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const transcriptOverlap = 512 // project component is capped by NAME_MAX; keeps full paths across read chunks.

var scratchpadIDRE = regexp.MustCompile(`(?i)claude-tmp/claude-1000/[^/\\\s"'<>]+/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:/|["'<>!#\\\s,})\x60.:;\]|&*?])`)
var outsideNameRE = regexp.MustCompile(`(?i)claude-tmp/([a-z0-9._-]+)`)

type referenceFile struct {
	mtime time.Time
	size  int64
	info  os.FileInfo
}

// References holds the initial ticket/transcript view and paths' mtimes. Refresh stats paths,
// but reads only files created or modified since their last indexed version.
type References struct {
	home, projects   string
	since            time.Time
	ticketFiles      map[string]referenceFile
	transcriptFiles  map[string]referenceFile
	ticketByFile     map[string]map[string]bool
	transcriptByFile map[string]map[string]bool
	ticketIDs        map[string]bool
	transcriptIDs    map[string]bool
	readTranscript   func(context.Context, string, func(context.Context) error, map[string]bool) error
}

func NewReferences(ctx context.Context, home, projects string, since time.Time, yield func(context.Context) error) (*References, error) {
	r := &References{
		home: home, projects: projects, since: since,
		ticketFiles: map[string]referenceFile{}, transcriptFiles: map[string]referenceFile{},
		ticketByFile: map[string]map[string]bool{}, transcriptByFile: map[string]map[string]bool{},
		ticketIDs: map[string]bool{}, transcriptIDs: map[string]bool{},
	}
	if err := r.scanTickets(ctx, yield); err != nil {
		return nil, err
	}
	if err := r.scanTranscripts(ctx, yield, true); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *References) TicketIDs() map[string]bool { return r.ticketIDs }

func (r *References) TranscriptIDs() map[string]bool { return r.transcriptIDs }

func (r *References) Refresh(ctx context.Context, yield func(context.Context) error) error {
	if err := r.scanTickets(ctx, yield); err != nil {
		return err
	}
	return r.scanTranscripts(ctx, yield, false)
}

func (r *References) scanTickets(ctx context.Context, yield func(context.Context) error) error {
	seen := map[string]bool{}
	err := walkTicketFiles(ctx, r.home, yield, func(path string, info fs.FileInfo) error {
		seen[path] = true
		old, known := r.ticketFiles[path]
		if known && !referenceFileChanged(old, info) {
			return nil
		}
		ids := map[string]bool{}
		if err := collectTicketFile(path, ids); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		r.ticketFiles[path] = newReferenceFile(info)
		r.ticketByFile[path] = ids
		return nil
	})
	if err != nil {
		return err
	}
	for path := range r.ticketFiles {
		if !seen[path] {
			delete(r.ticketFiles, path)
			delete(r.ticketByFile, path)
		}
	}
	r.ticketIDs = unionReferenceIDs(r.ticketByFile)
	return nil
}

func (r *References) scanTranscripts(ctx context.Context, yield func(context.Context) error, initial bool) error {
	seen := map[string]bool{}
	err := walkTranscriptFiles(ctx, r.projects, r.since, yield, func(path string, info fs.FileInfo) error {
		seen[path] = true
		old, known := r.transcriptFiles[path]
		if known && !referenceFileChanged(old, info) {
			return nil
		}
		ids := map[string]bool{}
		reader := r.readTranscript
		if reader == nil {
			reader = scanFreshTranscript
		}
		if err := reader(ctx, path, yield, ids); err != nil {
			if !initial && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		r.transcriptFiles[path] = newReferenceFile(info)
		r.transcriptByFile[path] = ids
		if id := transcriptFilenameID(path); id != "" {
			r.transcriptByFile[path][id] = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	for path := range r.transcriptFiles {
		if !seen[path] {
			delete(r.transcriptFiles, path)
			delete(r.transcriptByFile, path)
		}
	}
	r.transcriptIDs = unionReferenceIDs(r.transcriptByFile)
	return nil
}

func newReferenceFile(info os.FileInfo) referenceFile {
	return referenceFile{mtime: info.ModTime(), size: info.Size(), info: info}
}

func referenceFileChanged(old referenceFile, current os.FileInfo) bool {
	return !old.mtime.Equal(current.ModTime()) || old.size != current.Size() || !os.SameFile(old.info, current)
}

func transcriptFilenameID(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if !uuidRE.MatchString(name) {
		return ""
	}
	return strings.ToLower(name)
}

func unionReferenceIDs(files map[string]map[string]bool) map[string]bool {
	ids := map[string]bool{}
	for _, fileIDs := range files {
		for id := range fileIDs {
			ids[id] = true
		}
	}
	return ids
}

func ticketRootPatterns(home string) []string {
	return []string{
		filepath.Join(home, "docs", "tasks"),
		filepath.Join(home, "git", "*", "docs", "tasks"),
		filepath.Join(home, "work", "*", "docs", "tasks"),
		filepath.Join(home, "git", "tasks"),
		filepath.Join(home, "work", "tasks"),
	}
}

func resolvedTicketRoots(home string) ([]string, error) {
	seen := map[string]bool{}
	var roots []string
	for _, pattern := range ticketRootPatterns(home) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("expand task root %s: %w", pattern, err)
		}
		for _, match := range matches {
			root, err := filepath.EvalSymlinks(match)
			if errors.Is(err, fs.ErrNotExist) {
				info, lstatErr := os.Lstat(match)
				if lstatErr == nil && info.Mode()&fs.ModeSymlink != 0 {
					return nil, fmt.Errorf("resolve task root %s: dangling symlink", match)
				}
				if lstatErr != nil && !errors.Is(lstatErr, fs.ErrNotExist) {
					return nil, fmt.Errorf("lstat task root %s: %w", match, lstatErr)
				}
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("resolve task root %s: %w", match, err)
			}
			info, err := os.Stat(root)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("stat task root %s: %w", root, err)
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("task root %s is not a directory", root)
			}
			if seen[root] {
				continue
			}
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots, nil
}

func walkTicketFiles(ctx context.Context, home string, yield func(context.Context) error, visit func(string, fs.FileInfo) error) error {
	roots, err := resolvedTicketRoots(home)
	if err != nil {
		return err
	}
	seenFiles := map[string]bool{}
	for _, root := range roots {
		if err := walkTicketTree(ctx, root, yield, seenFiles, visit); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // this root itself disappeared; other roots still need scanning
			}
			return err
		}
	}
	return nil
}

func walkTicketTree(ctx context.Context, root string, yield func(context.Context) error, seen map[string]bool, visit func(string, fs.FileInfo) error) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && filepath.Clean(path) != filepath.Clean(root) {
				return nil // a ticket or subtree disappeared; continue with the rest of this root
			}
			return fmt.Errorf("walk ticket path %s: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 || entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		clean := filepath.Clean(path)
		if seen[clean] {
			return nil
		}
		seen[clean] = true
		if yield != nil {
			if err := yield(ctx); err != nil {
				return err
			}
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat ticket %s: %w", path, err)
		}
		return visit(path, info)
	})
	if err != nil {
		return fmt.Errorf("scan tickets under %s: %w", root, err)
	}
	return nil
}

func walkTranscriptFiles(ctx context.Context, projects string, since time.Time, yield func(context.Context) error, visit func(string, fs.FileInfo) error) error {
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return fmt.Errorf("read transcript projects %s: %w", projects, err)
	}
	for _, project := range dirs {
		if !project.IsDir() || project.Type()&fs.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(projects, project.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read transcript directory %s: %w", dir, err)
		}
		for _, file := range files {
			if file.IsDir() || file.Type()&fs.ModeSymlink != 0 || !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if yield != nil {
				if err := yield(ctx); err != nil {
					return err
				}
			}
			path := filepath.Join(dir, file.Name())
			info, err := file.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("stat transcript %s: %w", path, err)
			}
			if info.ModTime().Before(since) {
				continue
			}
			if err := visit(path, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func freshTranscriptIDs(ctx context.Context, projects string, since time.Time, yield func(context.Context) error) (map[string]bool, error) {
	ids := map[string]bool{}
	err := walkTranscriptFiles(ctx, projects, since, yield, func(path string, _ fs.FileInfo) error {
		return scanFreshTranscript(ctx, path, yield, ids)
	})
	return ids, err
}

func scanFreshTranscript(ctx context.Context, path string, yield func(context.Context) error, ids map[string]bool) (retErr error) {
	in, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transcript %s: %w", path, err)
	}
	defer func() {
		if err := in.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close transcript %s: %w", path, err))
		}
	}()
	reader := bufio.NewReaderSize(in, 64*1024)
	var carry []byte
	buffer := make([]byte, 0, 64*1024+transcriptOverlap+1)
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) != 0 || errors.Is(readErr, io.EOF) {
			buffer = buffer[:0]
			buffer = append(buffer, carry...)
			buffer = append(buffer, chunk...)
			if errors.Is(readErr, io.EOF) {
				buffer = append(buffer, '\n')
			}
			for _, match := range scratchpadIDRE.FindAllSubmatchIndex(buffer, -1) {
				id := string(buffer[match[2]:match[3]])
				if !ids[id] {
					ids[strings.ToLower(id)] = true
				}
			}
			if len(buffer) > transcriptOverlap {
				carry = append(carry[:0], buffer[len(buffer)-transcriptOverlap:]...)
			} else {
				carry = append(carry[:0], buffer...)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if yield != nil {
			if err := yield(ctx); err != nil {
				return err
			}
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read transcript %s: %w", path, readErr)
		}
	}
}

func collectTicketFile(path string, ids map[string]bool) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read ticket %s: %w", path, err)
	}
	content := string(body)
	if !hasOpenStatus(content) {
		return nil
	}
	for _, id := range uuidInTextRE.FindAllString(content, -1) {
		ids[strings.ToLower(id)] = true
	}
	for _, match := range outsideNameRE.FindAllStringSubmatchIndex(content, -1) {
		start, end := match[2], match[3]
		name := strings.TrimRight(strings.ToLower(content[start:end]), ".")
		if name == "" {
			continue
		}
		if end < len(content) && content[end] == '*' && strings.HasSuffix(name, "-") && (end+1 == len(content) || content[end+1] != '*') {
			name += "*"
		}
		ids["outside:"+name] = true
	}
	return nil
}

func hasOpenStatus(content string) bool {
	head := content
	if len(head) > 8192 {
		cut := 8192
		for cut > 0 && cut < len(head) && !utf8RuneStart(head[cut]) {
			cut--
		}
		head = head[:cut]
	}
	head = strings.ToLower(head)
	for _, status := range []string{"Статус: открыт", "Статус: в работе", "Статус: ждёт", "Статус: ждет", "Статус: на проверке"} {
		if strings.Contains(head, strings.ToLower(status)) {
			return true
		}
	}
	return false
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func openTicketIDs(ctx context.Context, home string, yield func(context.Context) error) (map[string]bool, error) {
	ids := map[string]bool{}
	err := walkTicketFiles(ctx, home, yield, func(path string, _ fs.FileInfo) error {
		if err := collectTicketFile(path, ids); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else {
			return err
		}
	})
	return ids, err
}

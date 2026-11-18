package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aiseeq/aguard/internal/policy"
	"github.com/aiseeq/aguard/internal/procscan"
	"github.com/aiseeq/akit/proc"
)

type tmpHolder struct {
	command string
	pid     int
}

// findTmpHolders проверяет только переданные записи и делает один проход по
// списку процессов. Сокетные inode сопоставляются с /proc/net/unix.
func findTmpHolders(procRoot string, entries []policy.Item) (map[string]tmpHolder, error) {
	holders := make(map[string]tmpHolder)
	if len(entries) == 0 {
		return holders, nil
	}
	paths := tmpEntryPaths(entries)
	socketPaths, err := readTmpUnixSockets(procRoot, paths)
	if err != nil {
		return nil, err
	}
	procs, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("список процессов: %w", err)
	}
	for _, ent := range procs {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || !ent.IsDir() {
			continue
		}
		procDir := filepath.Join(procRoot, ent.Name())
		used, err := tmpProcessEntries(procDir, paths, socketPaths)
		if err != nil {
			return nil, fmt.Errorf("pid %d: %w", pid, err)
		}
		if len(used) == 0 {
			continue
		}
		command, err := tmpProcessCommand(procDir)
		if err != nil {
			if proc.Gone(err) {
				continue
			}
			return nil, fmt.Errorf("pid %d command: %w", pid, err)
		}
		for entry := range used {
			if _, exists := holders[entry]; !exists {
				holders[entry] = tmpHolder{command: command, pid: pid}
			}
		}
	}
	return holders, nil
}

func tmpEntryPaths(entries []policy.Item) []string {
	paths := make([]string, 0, len(entries))
	for i := range entries {
		paths = append(paths, filepath.Clean(entries[i].Entry.Path))
	}
	sort.Strings(paths)
	return paths
}

func readTmpUnixSockets(procRoot string, paths []string) (map[string]string, error) {
	sockets := map[string]string{}
	f, err := os.Open(filepath.Join(procRoot, "net/unix"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return sockets, nil
		}
		return nil, fmt.Errorf("открыть /proc/net/unix: %w", err)
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		inode, path, ok := parseTmpUnixSocket(scanner.Text(), paths)
		if ok {
			sockets[inode] = path
		}
	}
	scanErr := scanner.Err()
	closeErr := f.Close()
	if scanErr != nil {
		return nil, fmt.Errorf("читать /proc/net/unix: %w", scanErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("закрыть /proc/net/unix: %w", closeErr)
	}
	return sockets, nil
}

func parseTmpUnixSocket(line string, paths []string) (inode, path string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return "", "", false
	}
	inode = fields[6]
	path = filepath.Clean(strings.Join(fields[7:], " "))
	return inode, path, inTmpEntry(path, paths)
}

func tmpProcessEntries(procDir string, paths []string, sockets map[string]string) (map[string]bool, error) {
	used := map[string]bool{}
	mark := func(path string) {
		for _, entry := range paths {
			if inTmpEntry(path, []string{entry}) {
				used[entry] = true
			}
		}
	}
	if cwd, err := os.Readlink(filepath.Join(procDir, "cwd")); err == nil {
		mark(strings.TrimSuffix(cwd, " (deleted)"))
	} else if !proc.Gone(err) && !os.IsPermission(err) {
		return nil, fmt.Errorf("cwd: %w", err)
	}
	fdDir := filepath.Join(procDir, "fd")
	err := procscan.ReadFDLinks(fdDir, func(link string) {
		if inode, ok := tmpSocketInode(link); ok {
			if path := sockets[inode]; path != "" {
				mark(path)
			}
			return
		}
		link = strings.TrimSuffix(link, " (deleted)")
		mark(link)
	})
	if err != nil {
		return nil, fmt.Errorf("fd: %w", err)
	}
	return used, nil
}

func tmpSocketInode(link string) (string, bool) {
	if !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), true
}

func tmpProcessCommand(procDir string) (string, error) {
	comm, err := os.ReadFile(filepath.Join(procDir, "comm"))
	if err != nil {
		return "", fmt.Errorf("comm: %w", err)
	}
	command := strings.TrimSpace(string(comm))
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		if !os.IsPermission(err) {
			return "", fmt.Errorf("cmdline: %w", err)
		}
		return fallbackTmpCommand(command), nil
	}
	if first, _, ok := strings.Cut(string(cmdline), "\x00"); ok && first != "" {
		command = filepath.Base(first)
	}
	if command == "" {
		command = "?"
	}
	return command, nil
}

func fallbackTmpCommand(comm string) string {
	if comm == "" {
		return "?"
	}
	return comm
}

func inTmpEntry(path string, entries []string) bool {
	for _, entry := range entries {
		rel, err := filepath.Rel(entry, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func markTmpHolders(groups []policy.Group, holders map[string]tmpHolder) {
	for gi := range groups {
		for ii := range groups[gi].Items {
			it := &groups[gi].Items[ii]
			it.Busy, it.BusyCommand, it.BusyPid = false, "", 0
			if h, ok := holders[filepath.Clean(it.Entry.Path)]; ok {
				it.Busy, it.BusyCommand, it.BusyPid = true, h.command, h.pid
			}
		}
	}
}

func (a *app) markTmpHolders(procRoot string, groups []policy.Group) {
	var entries []policy.Item
	for gi := range groups {
		entries = append(entries, groups[gi].Items...)
	}
	if len(entries) == 0 {
		return
	}
	holders, err := findTmpHolders(procRoot, entries)
	if err != nil {
		a.warn("tmp", "занятость записей не проверена", "err", err)
		return
	}
	markTmpHolders(groups, holders)
}

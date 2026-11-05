// Package statefile stores a JSON snapshot and an append-only journal of object changes.
package statefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/aiseeq/aguard/internal/record"
)

const (
	compactAt       = 256 << 10
	maxCachedStates = 8
)

type stamp struct {
	exists bool
	size   int64
	mtime  int64
	inode  uint64
}

type loadedState struct {
	root     *jsonNode
	validEnd int64
	snapshot stamp
	journal  stamp
}

var loadedStates = struct {
	sync.Mutex
	byPath map[string]loadedState
}{byPath: make(map[string]loadedState)}

// Load reads the snapshot and applies complete journal records. An incomplete final record is ignored.
func Load(path string) (state []byte, result error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) { // папки ещё нет (ручной tick до первого запуска демона): состояние пустое
		return []byte("{}"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return nil, fmt.Errorf("lock state for reading: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	root, validEnd, err := readTree(path)
	if err != nil {
		return nil, err
	}
	snapshotStamp, err := fileStamp(path)
	if err != nil {
		return nil, err
	}
	journalStamp, err := fileStamp(path + ".journal")
	if err != nil {
		return nil, err
	}
	remember(path, loadedState{root: root, validEnd: validEnd, snapshot: snapshotStamp, journal: journalStamp})
	state = root.marshalJSON()
	return state, nil
}

// Save appends the difference from the on-disk state, compacting the journal when it grows large.
func Save(path string, state []byte) (result error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer func() { result = errors.Join(result, lock.Close()) }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return saveLocked(path, state)
}

func saveLocked(path string, state []byte) (result error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(path + ".journal"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove orphaned state journal: %w", err)
		}
		return record.WriteFileAtomic(path, state, 0o600)
	} else if err != nil {
		return fmt.Errorf("stat state snapshot: %w", err)
	}

	current, validEnd, err := loadedForSave(path)
	if err != nil {
		return fmt.Errorf("read state before save: %w", err)
	}
	journalPath := path + ".journal"
	journal, err := os.OpenFile(journalPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state journal: %w", err)
	}
	defer func() { result = errors.Join(result, journal.Close()) }()
	return appendState(path, journal, current, state, validEnd)
}

func loadedForSave(path string) (*jsonNode, int64, error) {
	loaded, ok := takeRemembered(path)
	if ok {
		snapshot, err := fileStamp(path)
		if err != nil {
			return nil, 0, err
		}
		journal, err := fileStamp(path + ".journal")
		if err != nil {
			return nil, 0, err
		}
		if snapshot == loaded.snapshot && journal == loaded.journal {
			return loaded.root, loaded.validEnd, nil
		}
	}
	return readTree(path)
}

func remember(path string, loaded loadedState) {
	loadedStates.Lock()
	defer loadedStates.Unlock()
	if len(loadedStates.byPath) >= maxCachedStates {
		delete(loadedStates.byPath, slices.Sorted(maps.Keys(loadedStates.byPath))[0])
	}
	loadedStates.byPath[path] = loaded
}

func takeRemembered(path string) (loadedState, bool) {
	loadedStates.Lock()
	defer loadedStates.Unlock()
	loaded, ok := loadedStates.byPath[path]
	delete(loadedStates.byPath, path)
	return loaded, ok
}

func fileStamp(path string) (stamp, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return stamp{}, nil
	}
	if err != nil {
		return stamp{}, fmt.Errorf("stat state file %s: %w", path, err)
	}
	s := stamp{exists: true, size: info.Size(), mtime: info.ModTime().UnixNano()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		s.inode = stat.Ino
	}
	return s, nil
}

func appendState(path string, journal *os.File, current *jsonNode, state []byte, validEnd int64) error {
	info, err := journal.Stat()
	if err != nil {
		return fmt.Errorf("stat state journal: %w", err)
	}
	if validEnd < info.Size() {
		if err := journal.Truncate(validEnd); err != nil {
			return fmt.Errorf("repair state journal: %w", err)
		}
	}
	if _, err := journal.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek state journal: %w", err)
	}
	patch, changed, err := makePatch(current, state)
	if err != nil {
		return fmt.Errorf("diff state: %w", err)
	}
	if !changed {
		return nil
	}
	frame, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encode state change: %w", err)
	}
	frame = append(frame, '\n')
	if _, err := journal.Write(frame); err != nil {
		return fmt.Errorf("append state journal: %w", err)
	}
	info, err = journal.Stat()
	if err != nil {
		return fmt.Errorf("stat state journal: %w", err)
	}
	snapshot, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat state snapshot: %w", err)
	}
	if info.Size() > compactAt && info.Size() > snapshot.Size()/4 {
		return compact(path, journal, state)
	}
	return nil
}

// compact — состояние в снимок, журнал пуст. Проход без fsync: сбой питания теряет хвост журнала, строки перечитаются.
// Уплотнение редкое и единственное, где порядок на диске важен: журнал целиком на диске до замены снимка (повтор всего
// журнала над новым снимком даёт тот же снимок, а его начало — откат), снимок на диске до усечения журнала.
func compact(path string, journal *os.File, state []byte) error {
	if err := journal.Sync(); err != nil {
		return fmt.Errorf("sync state journal: %w", err)
	}
	if err := record.WriteFileAtomic(path, state, 0o600); err != nil {
		return fmt.Errorf("compact state snapshot: %w", err)
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		if err := syncPath(p); err != nil {
			return fmt.Errorf("sync compacted state: %w", err)
		}
	}
	if err := journal.Truncate(0); err != nil {
		return fmt.Errorf("truncate state journal: %w", err)
	}
	return nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open for sync: %w", err)
	}
	return errors.Join(f.Sync(), f.Close())
}

// read returns the reconstructed JSON and the byte offset through the last complete valid record.
func readTree(path string) (*jsonNode, int64, error) {
	stateJSON, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &jsonNode{raw: json.RawMessage(`{}`)}, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read state snapshot %s: %w", path, err)
	}
	if !json.Valid(stateJSON) {
		return nil, 0, fmt.Errorf("state snapshot %s: invalid JSON", path)
	}
	jpath := path + ".journal"
	b, err := os.ReadFile(jpath)
	if errors.Is(err, os.ErrNotExist) {
		return &jsonNode{raw: stateJSON}, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read state journal %s: %w", jpath, err)
	}
	state := &jsonNode{raw: stateJSON}
	end := int64(0)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		var p patch
		if err := json.Unmarshal(b[:i], &p); err != nil {
			return nil, end, fmt.Errorf("state journal %s: %w", jpath, err)
		}
		if err := applyPatch(state, p); err != nil {
			return nil, end, fmt.Errorf("state journal %s: %w", jpath, err)
		}
		end += int64(i + 1)
		b = b[i+1:]
	}
	return state, end, nil
}

type patch struct {
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Delete []string                   `json:"delete,omitempty"`
	Merge  map[string]patch           `json:"merge,omitempty"`
}

func makePatch(old *jsonNode, newJSON []byte) (patch, bool, error) {
	previous, err := old.object()
	if err != nil {
		return patch{}, false, fmt.Errorf("decode existing state object: %w", err)
	}
	var next map[string]json.RawMessage
	if err := json.Unmarshal(newJSON, &next); err != nil {
		return patch{}, false, fmt.Errorf("decode new state: %w", err)
	}
	p, changed, err := diffObjects(old, previous, next)
	return p, changed, err
}

func diffObjects(oldNode *jsonNode, old, next map[string]json.RawMessage) (patch, bool, error) {
	p := patch{}
	for k := range old {
		if _, ok := next[k]; !ok {
			p.Delete = append(p.Delete, k)
		}
	}
	for k, value := range next {
		before, ok := old[k]
		if !ok {
			if p.Set == nil {
				p.Set = map[string]json.RawMessage{}
			}
			p.Set[k] = value
			continue
		}
		prior := (*jsonNode)(nil)
		if oldNode != nil {
			prior = oldNode.children[k]
		}
		child, isObject, changed, err := diffMember(prior, before, value)
		if err != nil {
			return patch{}, false, err
		}
		storeChange(&p, k, child, isObject, changed, value)
	}
	return p, len(p.Set)+len(p.Delete)+len(p.Merge) > 0, nil
}

func diffMember(prior *jsonNode, before, value []byte) (patch, bool, bool, error) {
	if prior != nil && prior.opened {
		if next, err := decodeObject(value); err == nil {
			p, changed, err := diffObjects(prior, prior.members, next)
			return p, true, changed, err
		}
		before = prior.marshalJSON()
	}
	return diffRaw(before, value)
}

func storeChange(dst *patch, key string, value patch, isObject, changed bool, raw json.RawMessage) {
	if !changed {
		return
	}
	if isObject {
		if dst.Merge == nil {
			dst.Merge = map[string]patch{}
		}
		dst.Merge[key] = value
		return
	}
	if dst.Set == nil {
		dst.Set = map[string]json.RawMessage{}
	}
	dst.Set[key] = raw
}

func diffRaw(before, value []byte) (patch, bool, bool, error) {
	if bytes.Equal(before, value) {
		return patch{}, false, false, nil
	}
	old, oldErr := decodeObject(before)
	next, nextErr := decodeObject(value)
	if oldErr == nil && nextErr == nil {
		p, changed, err := diffObjects(nil, old, next)
		return p, true, changed, err
	}
	equal, err := jsonEqualChecked(before, value)
	return patch{}, false, !equal, err
}

func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("decode JSON object: %w", err)
	}
	if obj == nil {
		return nil, fmt.Errorf("value is not an object")
	}
	return obj, nil
}

func jsonEqual(a, b []byte) bool {
	equal, err := jsonEqualChecked(a, b)
	if err != nil {
		panic(err)
	}
	return equal
}

func jsonEqualChecked(a, b []byte) (bool, error) {
	left, err := jsonNumberValue(a)
	if err != nil {
		return false, err
	}
	right, err := jsonNumberValue(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(left, right), nil
}

func jsonNumberValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON value: %w", err)
	}
	return value, nil
}

// jsonNode keeps untouched values in their original encoding and expands objects only when a patch enters them.
type jsonNode struct {
	raw      json.RawMessage
	members  map[string]json.RawMessage
	children map[string]*jsonNode
	opened   bool
}

func (n *jsonNode) object() (map[string]json.RawMessage, error) {
	if n.opened {
		if n.members == nil {
			return nil, fmt.Errorf("patch target is not an object")
		}
		return n.members, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(n.raw, &raw); err != nil {
		return nil, fmt.Errorf("decode JSON object: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("patch target is not an object")
	}
	n.members = raw
	n.children = make(map[string]*jsonNode)
	n.opened = true
	return n.members, nil
}

func (n *jsonNode) marshalJSON() []byte {
	out := make([]byte, 0, len(n.raw))
	return n.appendJSON(out)
}

func (n *jsonNode) appendJSON(out []byte) []byte {
	if !n.opened {
		return append(out, n.raw...)
	}
	out = append(out, '{')
	first := true
	for _, key := range slices.Sorted(maps.Keys(n.members)) {
		value := n.members[key]
		if !first {
			out = append(out, ',')
		}
		first = false
		out = appendJSONString(out, key)
		out = append(out, ':')
		if child := n.children[key]; child != nil {
			out = child.appendJSON(out)
		} else {
			out = append(out, value...)
		}
	}
	return append(out, '}')
}

func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		switch r {
		case '"', '\\':
			dst = append(dst, '\\', byte(r))
		case '\b':
			dst = append(dst, `\b`...)
		case '\f':
			dst = append(dst, `\f`...)
		case '\n':
			dst = append(dst, `\n`...)
		case '\r':
			dst = append(dst, `\r`...)
		case '\t':
			dst = append(dst, `\t`...)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hex[byte(r)>>4], hex[byte(r)&0x0f])
			} else if r == utf8.RuneError && size == 1 {
				dst = append(dst, '\xef', '\xbf', '\xbd')
			} else {
				dst = append(dst, s[:size]...)
			}
		}
		s = s[size:]
	}
	return append(dst, '"')
}

func applyPatch(state *jsonNode, p patch) error {
	obj, err := state.object()
	if err != nil {
		return fmt.Errorf("patch target: %w", err)
	}
	for _, key := range p.Delete {
		delete(obj, key)
		delete(state.children, key)
	}
	for key, value := range p.Set {
		obj[key] = value
		delete(state.children, key)
	}
	for key, child := range p.Merge {
		value := state.children[key]
		if value == nil {
			raw, ok := obj[key]
			if !ok {
				raw = json.RawMessage(`{}`)
			}
			value = &jsonNode{raw: raw}
			state.children[key] = value
		}
		if err := applyPatch(value, child); err != nil {
			return err
		}
	}
	return nil
}

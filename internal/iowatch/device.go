package iowatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNoDevice — диска из настройки device нет в системе: замер не идёт, пока его не поправят.
var ErrNoDevice = errors.New("нет диска")

// blockDevices — major:minor диска и всех устройств device mapper, стоящих на нём или его разделах
// (LUKS, LVM — по цепочке holders). Первым идёт сам диск.
//
// Зачем все: io.stat cgroup ведёт строку на каждое устройство, и одна и та же запись видна и на dm,
// и на диске под ним (ядро 7.2, btrfs на LUKS: строки 259:0 и 252:0 совпадают и при буферной записи,
// и при O_DIRECT); бывает, что O_DIRECT виден только на dm. Поэтому по каждому полю берётся
// наибольшая дельта среди этих устройств, а не сумма (сумма удваивает запись) и не один диск.
func blockDevices(blockRoot, device string) ([]string, error) {
	diskDir := filepath.Join(blockRoot, device)
	id, err := readDevID(diskDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s нет в %s", ErrNoDevice, device, blockRoot)
	}
	if err != nil {
		return nil, err
	}
	ids := []string{id}
	queue, err := holderNames(diskDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(diskDir)
	if err != nil {
		return nil, fmt.Errorf("разделы %s: %w", device, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue // файлы атрибутов диска: dev, size, stat
		}
		part := filepath.Join(diskDir, entry.Name())
		_, err := os.Stat(filepath.Join(part, "partition"))
		if errors.Is(err, os.ErrNotExist) {
			continue // не раздел: queue, holders, power и прочие папки устройства
		}
		if err != nil {
			return nil, fmt.Errorf("раздел %s: %w", part, err)
		}
		names, err := holderNames(part)
		if err != nil {
			return nil, err
		}
		queue = append(queue, names...)
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		dir := filepath.Join(blockRoot, name)
		holderID, err := readDevID(dir)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ids, holderID) {
			ids = append(ids, holderID)
		}
		names, err := holderNames(dir)
		if err != nil {
			return nil, err
		}
		queue = append(queue, names...)
	}
	return ids, nil
}

func readDevID(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "dev"))
	if err != nil {
		return "", fmt.Errorf("major:minor устройства %s: %w", filepath.Base(dir), err)
	}
	return strings.TrimSpace(string(b)), nil
}

// holderNames — устройства, стоящие на dir (dm поверх диска или раздела); нет папки holders — нет и их.
func holderNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "holders"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("holders %s: %w", filepath.Base(dir), err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

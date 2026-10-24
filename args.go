package main

import "strings"

// Флаги claude, которые cl знает при перезапуске. Повторяемые — переходят в новый запуск как есть; сбрасываемые —
// про выбор беседы, имя и права: их cl ставит сам (--resume <сессия>, -n <имя>). Любой другой флаг — перезапуск
// потерял бы его, и сессия не перезапускается.
var (
	replayWithValue = map[string]bool{"--model": true, "--effort": true, "--agent": true, "--add-dir": true, "--settings": true}
	replayBare      = map[string]bool{"--chrome": true, "--no-chrome": true}
	dropWithValue   = map[string]bool{"-n": true, "--name": true, "--session-id": true}
	dropOptional    = map[string]bool{"-r": true, "--resume": true}
	dropBare        = map[string]bool{"-c": true, "--continue": true, "--dangerously-skip-permissions": true}
)

// restartArgs — аргументы пользователя, которые перезапуск повторит, и почему перезапуск невозможен (пусто — можно).
// Позиционный аргумент — первая реплика, она уже отдана сессии и не повторяется.
func restartArgs(args []string) ([]string, string) {
	var keep []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, _, inline := strings.Cut(a, "=")
		switch {
		case !strings.HasPrefix(a, "-"):
		case replayWithValue[flag]:
			keep = append(keep, a)
			if !inline && i+1 < len(args) {
				i++
				keep = append(keep, args[i])
			}
		case replayBare[a]:
			keep = append(keep, a)
		case dropWithValue[flag]:
			if !inline {
				i++
			}
		case dropOptional[flag]:
			if !inline && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case dropBare[a]:
		default:
			return nil, "свой флаг claude " + a + ": перезапуск его потерял бы"
		}
	}
	return keep, ""
}

// bare — запуск без своих аргументов, кроме имени: только такой старт получает строку роли (свой промпт, --resume или
// -c пользователь выбрал сам).
func bare(args []string) bool {
	for i := 0; i < len(args); i++ {
		flag, _, inline := strings.Cut(args[i], "=")
		if !dropWithValue[flag] || flag == "--session-id" {
			return false
		}
		if !inline {
			i++
		}
	}
	return true
}

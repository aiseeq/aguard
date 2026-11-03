#!/bin/sh
# Снимки и журнал старых служб — в папки aguard. Заведённое новым слежением не затирается; старых папок нет — копировать нечего.
set -eu
state=$HOME/.local/state/aguard
copy() {
	mkdir -p "$2"
	[ -d "$1" ] || return 0
	for f in "$1"/state.json "$1"/owners.jsonl*; do
		[ ! -e "$f" ] || cp --update=none "$f" "$2/"
	done
}
copy /var/lib/tmpguard "$state/track"
copy /var/lib/tmpguard-home "$state/track-home"

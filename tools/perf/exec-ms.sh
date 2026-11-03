#!/usr/bin/env bash
# exec-ms.sh <command> [arguments...] prints the median wall time in ms from
# akit timeit. A command's exit 3 becomes 1: in tools/perf, 3 means too little data.
set -uo pipefail
(( $# > 0 )) || { echo "usage: exec-ms.sh <command> [arguments...]" >&2; exit 2; }
timeit --runs 5 --budget 45s --limit 60s -- "$@"
rc=$?
(( rc == 3 )) && rc=1
exit "$rc"

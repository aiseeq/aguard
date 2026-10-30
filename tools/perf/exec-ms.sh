#!/usr/bin/env bash
# exec-ms.sh <command> [arguments...] prints the median wall time in ms, with
# up to five runs while the configured time budget remains.
set -uo pipefail
(( $# > 0 )) || { echo "usage: exec-ms.sh <command> [arguments...]" >&2; exit 2; }
budget_ms=$(( ${AGUARD_PERF_BUDGET_S:-45} * 1000 )); limit_ms=$(( ${AGUARD_PERF_LIMIT_S:-60} * 1000 ))
[[ $budget_ms =~ ^[0-9]+$ && $limit_ms =~ ^[0-9]+$ ]] || { echo "exec-ms: budget values must be nonnegative integers" >&2; exit 2; }
samples=(); started=$(date +%s%N) || { echo "exec-ms: cannot read clock" >&2; exit 1; }
while (( ${#samples[@]} < 5 )); do
 now=$(date +%s%N) || { echo "exec-ms: cannot read clock" >&2; exit 1; }
 (( ${#samples[@]} == 0 || (now-started)/1000000 < budget_ms )) || break
 "$@" >/dev/null 2>&1; rc=$?; end=$(date +%s%N) || { echo "exec-ms: cannot read clock" >&2; exit 1; }
 if (( rc != 0 )); then (( rc==3 )) && rc=1; exit "$rc"; fi
 samples+=( $(((end-now)/1000000)) )
 if (( ${#samples[@]}==1 && limit_ms-samples[0]<budget_ms )); then budget_ms=$((limit_ms-samples[0])); ((budget_ms<0))&&budget_ms=0; fi
done
printf '%s\n' "${samples[@]}" | sort -n | awk '{a[NR]=$1}END{if(NR%2)print a[(NR+1)/2];else printf "%.0f\n",(a[NR/2]+a[NR/2+1])/2}'

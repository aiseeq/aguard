#!/usr/bin/env bash
# unit-cpu-day.sh [--until T] prints aguard's CPU seconds per day over the
# preceding 24 hours, using systemd journal accounting and the active unit.
set -uo pipefail

usage() { echo "usage: unit-cpu-day.sh [--until T]" >&2; exit 2; }
until_arg=
if (( $# == 2 )) && [[ $1 == --until ]]; then until_arg=$2
elif (( $# != 0 )); then usage
fi
if [[ -n $until_arg ]]; then end=$(date -d "$until_arg" +%s 2>/dev/null) || usage
else end=$(date +%s) || { echo "unit-cpu-day: cannot read clock" >&2; exit 1; }; fi
start=$((end-86400)); query_start=$((start-8*86400))
test_clock=0
if [[ -n ${AGUARD_PERF_NOW:-} ]]; then now=$AGUARD_PERF_NOW; test_clock=1
else now=$(date +%s) || { echo "unit-cpu-day: cannot read clock" >&2; exit 1; }; fi
[[ $now =~ ^[0-9]+([.][0-9]+)?$ ]] || { echo "unit-cpu-day: invalid AGUARD_PERF_NOW" >&2; exit 1; }
if [[ -n ${AGUARD_PERF_JOURNAL:-} ]]; then journal=$(cat "$AGUARD_PERF_JOURNAL") || { echo "unit-cpu-day: cannot read journal fixture" >&2; exit 1; }
else journal=$(journalctl --user -u aguard -o short-unix --no-pager --since "@$query_start") || { echo "unit-cpu-day: journalctl failed" >&2; exit 1; }; fi
result=$(awk -v lo="$start" -v hi="$end" '
function seconds(s,total,n,u) {
 total=0
 while (match(s,/^[0-9]+([.][0-9]+)?(min|ms|us|µs|d|h|m|s)/)) {
  n=substr(s,1,RLENGTH); s=substr(s,RLENGTH+1); u=n
  sub(/^[0-9]+([.][0-9]+)?/,"",u); sub(/(min|ms|us|µs|d|h|m|s)$/,"",n); n+=0
  if(u=="d")total+=n*86400; else if(u=="h")total+=n*3600
  else if(u=="min"||u=="m")total+=n*60; else if(u=="ms")total+=n/1000
  else if(u=="us"||u=="µs")total+=n/1000000; else total+=n
  sub(/^ +/,"",s)
 }
 return total
}
{
 if($0 !~ /Consumed .* CPU time over .* wall clock time/)next
 epoch=$1+0; line=$0; sub(/^.*Consumed /,"",line); sub(/ CPU time over .*$/,"",line); cpu=seconds(line)
 line=$0; sub(/^.* CPU time over /,"",line); sub(/ wall clock time.*$/,"",line); wall=seconds(line)
 if(wall<=0)next
 a=epoch-wall; x=(a>lo?a:lo); y=(epoch<hi?epoch:hi)
 if(y>x){cpu_total+=cpu*(y-x)/wall; seg[++n,1]=x;seg[n,2]=y}
}
END {
 for(i=1;i<=n;i++)for(j=i+1;j<=n;j++)if(seg[i,1]>seg[j,1]){t=seg[i,1];seg[i,1]=seg[j,1];seg[j,1]=t;t=seg[i,2];seg[i,2]=seg[j,2];seg[j,2]=t}
 covered=0
 for(i=1;i<=n;i++){if(i==1){a=seg[i,1];b=seg[i,2]}else if(seg[i,1]<=b){if(seg[i,2]>b)b=seg[i,2]}else{covered+=b-a;a=seg[i,1];b=seg[i,2]}}
 if(n)covered+=b-a
 printf "%.9f %.9f\n",cpu_total,covered
}' <<<"$journal") || { echo "unit-cpu-day: journal parsing failed" >&2; exit 1; }
read -r cpu covered <<<"$result"
if [[ $test_clock == 1 ]]; then
 if [[ -n ${AGUARD_PERF_SHOW:-} ]]; then show=$(cat "$AGUARD_PERF_SHOW") || { echo "unit-cpu-day: cannot read systemctl fixture" >&2; exit 1; }
 else show=; fi
else show=$(systemctl --user show aguard -p CPUUsageNSec -p ActiveEnterTimestampMonotonic -p ActiveState) || { echo "unit-cpu-day: systemctl show failed" >&2; exit 1; }; fi
state='' cpu_ns='' active_us=''
while IFS='=' read -r key value; do case $key in ActiveState)state=$value;; CPUUsageNSec)cpu_ns=$value;; ActiveEnterTimestampMonotonic)active_us=$value;; esac; done <<<"$show"
if [[ $state == active ]]; then
 if [[ ! $cpu_ns =~ ^[0-9]+$ || ! $active_us =~ ^[0-9]+$ ]]; then echo "unit-cpu-day: invalid active unit properties" >&2; exit 1; fi
 if [[ -n ${AGUARD_PERF_UPTIME:-} ]]; then uptime=$AGUARD_PERF_UPTIME; else uptime=$(cut -d' ' -f1 /proc/uptime) || { echo "unit-cpu-day: cannot read uptime" >&2; exit 1; }; fi
 [[ $uptime =~ ^[0-9]+([.][0-9]+)?$ ]] || { echo "unit-cpu-day: invalid uptime" >&2; exit 1; }
 read -r cpu covered < <(awk -v c="$cpu" -v cov="$covered" -v lo="$start" -v hi="$end" -v now="$now" -v up="$uptime" -v active="$active_us" -v ns="$cpu_ns" 'BEGIN{a=now-(up-active/1000000);x=(a>lo?a:lo);y=(now<hi?now:hi);if(y>x){c+=(ns/1000000000)*(y-x)/(now-a);cov+=y-x}printf "%.9f %.9f\n",c,cov}')
fi
awk -v c="$cpu" -v cov="$covered" 'BEGIN{if(cov<43200)exit 3;printf "%.0f\n",c*86400/cov}'

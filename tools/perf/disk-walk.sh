#!/usr/bin/env bash
# disk-walk.sh [--until T] prints the median unpaused duration of aguard disk
# walks in the preceding 24 hours, in seconds.
set -uo pipefail
usage(){ echo "usage: disk-walk.sh [--until T]" >&2; exit 2; }
until_arg=
if (( $# == 2 )) && [[ $1 == --until ]]; then until_arg=$2; elif (( $# != 0 )); then usage; fi
if [[ -n $until_arg ]]; then end=$(date -d "$until_arg" +%s 2>/dev/null) || usage; else end=$(date +%s) || { echo "disk-walk: cannot read clock" >&2; exit 1; }; fi
start=$((end-86400))
if [[ -n ${AGUARD_PERF_JOURNAL:-} ]]; then journal=$(cat "$AGUARD_PERF_JOURNAL") || { echo "disk-walk: cannot read journal fixture" >&2; exit 1; }
else journal=$(journalctl --user -u aguard -o short-unix --no-pager --since "@$start" --until "@$end") || { echo "disk-walk: journalctl failed" >&2; exit 1; }; fi
awk -v lo="$start" -v hi="$end" '
function duration(s,total,n,u){total=0;while(match(s,/^[0-9]+([.][0-9]+)?(ns|us|µs|ms|h|m|s)/)){n=substr(s,1,RLENGTH);s=substr(s,RLENGTH+1);u=n;sub(/^[0-9]+([.][0-9]+)?/,"",u);sub(/(ns|us|µs|ms|h|m|s)$/,"",n);n+=0;if(u=="h")total+=n*3600;else if(u=="m")total+=n*60;else if(u=="ms")total+=n/1000;else if(u=="us"||u=="µs")total+=n/1000000;else if(u=="ns")total+=n/1000000000;else total+=n}return total}
{if($0 !~ /msg="aguard disk: обход"/||$0 !~ /took=/||$0 !~ /paused=/||$0~/interrupted=true/)next;ts=$1+0;if(ts<lo||ts>hi)next;took=paused="";for(i=1;i<=NF;i++){if($i~/^took=/){took=$i;sub(/^took=/,"",took)}if($i~/^paused=/){paused=$i;sub(/^paused=/,"",paused)}}if(took==""||paused=="")next;v=duration(took)-duration(paused);if(v>=0)a[++n]=v}
END{if(n<3)exit 3;for(i=1;i<=n;i++)for(j=i+1;j<=n;j++)if(a[i]>a[j]){t=a[i];a[i]=a[j];a[j]=t};if(n%2)printf "%.1f\n",a[(n+1)/2];else printf "%.1f\n",(a[n/2]+a[n/2+1])/2}' <<<"$journal"

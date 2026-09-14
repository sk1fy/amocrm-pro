#!/usr/bin/env bash
# Small server health snapshot for the amocrm-pro host.
# Usage: ./server-health.sh
set -uo pipefail

hdr() { printf '\n\033[1m%s\033[0m\n' "$1"; }
ok()  { printf '  \033[32m%s\033[0m\n' "$1"; }
warn(){ printf '  \033[33m%s\033[0m\n' "$1"; }
bad() { printf '  \033[31m%s\033[0m\n' "$1"; }

# --- CPU usage from /proc/stat over 1s ---
read -r _ a b c idle rest < /proc/stat
sleep 1
read -r _ a2 b2 c2 idle2 rest2 < /proc/stat
busy=$(( (a2+b2+c2) - (a+b+c) ))
total=$(( busy + (idle2-idle) ))
cpu_pct=$(( total > 0 ? busy*100/total : 0 ))
ncpu=$(nproc)
read -r load1 load5 load15 _ < /proc/loadavg

hdr "HOST"
printf '  %s | up %s | kernel %s\n' "$(hostname)" "$(uptime -p 2>/dev/null | sed 's/^up //')" "$(uname -r)"

hdr "CPU / LOAD"
printf '  cores: %s | usage: %s%% | load: %s %s %s\n' "$ncpu" "$cpu_pct" "$load1" "$load5" "$load15"

hdr "MEMORY"
free -h | awk '/^Mem:/{printf "  RAM   used %s / %s (%.0f%%), available %s\n",$3,$2,$3*100/$2,$7} /^Swap:/{if($2!="0B")printf "  Swap  used %s / %s\n",$3,$2}'

hdr "DISK"
df -h -x tmpfs -x devtmpfs -x overlay 2>/dev/null | awk 'NR==1{next}{printf "  %-20s %5s used  %5s free  %s  %s\n",$6,$3,$4,$5,$1}'
echo "  --- inodes ---"
df -i -x tmpfs -x devtmpfs -x overlay 2>/dev/null | awk 'NR>1{printf "  %-20s %5s inode-use  %s\n",$6,$5,$1}'

hdr "TOP PROCESSES (by CPU)"
ps -eo pcpu,pmem,comm --sort=-pcpu 2>/dev/null | awk 'NR==1{next} NR<=6{printf "  %5s%% cpu  %5s%% mem  %s\n",$1,$2,$3}'

hdr "DOCKER"
total_c=$(docker ps -aq 2>/dev/null | wc -l)
run_c=$(docker ps -q 2>/dev/null | wc -l)
bad_c=$(docker ps --format '{{.Names}} {{.Status}}' 2>/dev/null | grep -viE 'healthy|Up [0-9]+ (second|minute|hour|day)' | wc -l)
printf '  containers: %s running / %s total\n' "$run_c" "$total_c"
if [ "$bad_c" -eq 0 ]; then ok "no unhealthy/restarting containers"; else bad "$bad_c container(s) not in a clean state"; docker ps --format '  {{.Names}} {{.Status}}' | grep -viE 'healthy|Up [0-9]+ (second|minute|hour|day)'; fi

hdr "OBSERVABILITY"
if docker inspect observability-prometheus-1 >/dev/null 2>&1; then
  upc=$(docker exec observability-prometheus-1 wget -qO- 'http://127.0.0.1:9090/api/v1/query?query=count(up%3D%3D1)' 2>/dev/null | sed -n 's/.*"value":\[[^,]*,"\([0-9]*\)".*/\1/p')
  tot=$(docker exec observability-prometheus-1 wget -qO- 'http://127.0.0.1:9090/api/v1/query?query=count(up)' 2>/dev/null | sed -n 's/.*"value":\[[^,]*,"\([0-9]*\)".*/\1/p')
  fired=$(docker exec observability-prometheus-1 wget -qO- 'http://127.0.0.1:9090/api/v1/query?query=ALERTS%7Balertstate%3D%22firing%22%7D' 2>/dev/null | grep -c '"alertname"')
  printf '  scrape targets up: %s / %s\n' "${upc:-?}" "${tot:-?}"
  if [ "${fired:-0}" -eq 0 ]; then ok "no firing alerts"; else warn "$fired firing alert(s) — Grafana /grafana/d/platform-overview"; fi
else
  warn "observability stack not running"
fi
echo

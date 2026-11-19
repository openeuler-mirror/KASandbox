#!/bin/bash
# CubeBench multi-round stability: CRI create/density + native §3.2/3.3/4.1–4.6
#
# Lives next to cube-verify, but is not part of ./run_all.sh. One 5-round pass
# takes about an hour and shells out to CubeSandbox tests/perf/cubebench.sh.
# Machine-specific disk and IP defaults stay outside this script; set them
# with ROOT / ROUNDS / CUBE_PROXY_NODE_IP / ENSURE_OE_XFS.
#
# Usage (on the Cube node, as root, from a tree that contains tests/perf/cubebench.sh
# or with BENCH_DIR pointing at that tree):
#   ROUNDS=5 CUBE_PROXY_NODE_IP=<node-ip> bash run_stability.sh
#
# Optional env:
#   ROOT                 output dir (default: /home/cube/bench/stability)
#   STABILITY_TITLE      report title
#   CRI_BIN              dir with cube-e2e / cri-multiplex (default: /home/cube/cri/bin)
#   BENCH_DIR            CubeSandbox bench tree with tests/perf/cubebench.sh
#   ENSURE_OE_XFS=1      mount opencloudos-home → /mnt/oc9-xfs → /home/data (33468 OE profile)
#   CUBELET_ADDR         override cubelet address for cri-multiplex
#                        (default: unix:///data/cubelet/cubelet.sock if sock exists, else 127.0.0.1:9999)
#
set -euo pipefail

CRI_BIN=${CRI_BIN:-/home/cube/cri/bin}
BENCH_DIR=${BENCH_DIR:-/home/cube/bench}
export PATH="${CRI_BIN}:/usr/bin:/usr/sbin:/usr/local/bin:${BENCH_DIR}/examples/cube-bench/bin:${PATH:-}"

ROUNDS=${ROUNDS:-5}
NODE=${CUBE_PROXY_NODE_IP:-127.0.0.1}
ROOT=${ROOT:-/home/cube/bench/stability}
OUT=$ROOT/out
SUMMARY=$ROOT/summary.tsv
STABILITY_TITLE=${STABILITY_TITLE:-"CubeBench multi-round stability"}
ENSURE_OE_XFS=${ENSURE_OE_XFS:-0}

log() { echo "[$(date '+%F %T')] $*" | tee -a "${ROOT}/run.log"; }

ensure_oe_xfs_data() {
  [[ "$ENSURE_OE_XFS" == "1" ]] || return 0
  findmnt /mnt/oc9-xfs >/dev/null 2>&1 || mount -t xfs /dev/mapper/opencloudos-home /mnt/oc9-xfs
  findmnt /home/data >/dev/null 2>&1 || mount --bind /mnt/oc9-xfs/oe-cube-data /home/data
  mkdir -p /home/cube
  if [[ ! -e /home/cube/bench ]]; then
    ln -sfn /mnt/oc9-xfs/cube/bench /home/cube/bench
  fi
  ln -sfn /mnt/oc9-xfs/cube/cri /home/cube/cri 2>/dev/null || true
  df -hT /data || true
  cp --reflink=always /data/.oe-cube-xfs-ok /data/.reflink-stab-check 2>/dev/null && echo REFLINK_OK || true
}

ensure_quota() {
  local CFG=/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml
  [[ -f "$CFG" ]] || { echo "quota_skip_no_cfg"; return 0; }
  python3 - <<'PY'
from pathlib import Path
import re
p = Path("/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml")
text = p.read_text(encoding="utf-8")
text2 = text
text2 = re.sub(r'(?m)^(\s*mcpu_limit:\s*).*$', r'\g<1>1000000', text2, count=1)
text2 = re.sub(r'(?m)^(\s*mem_limit:\s*).*$', r'\g<1>"4096Gi"', text2, count=1)
text2 = re.sub(r'(?m)^(\s*mvm_limit:\s*).*$', r'\g<1>2000', text2, count=1)
if text2 != text:
    p.write_text(text2, encoding="utf-8")
    print("quota_patched")
else:
    print("quota_ok")
PY
}

wait_cubelet_ready() {
  log "wait cubelet gRPC sock"
  for i in $(seq 1 90); do
    if [[ -S /data/cubelet/cubelet.sock ]]; then
      if ss -lntp 2>/dev/null | grep -qE ':9999\b'; then
        log "cubelet_ready sock+tcp try=$i"
      else
        log "cubelet_ready sock-only try=$i (tcp9999 absent; multiplex will use unix)"
      fi
      return 0
    fi
    sleep 1
  done
  log "WARN cubelet sock not ready; continue with best-effort"
  ls -l /data/cubelet/cubelet.sock 2>/dev/null || true
  ss -lntp | grep -E '9999|3000|8089' || true
}

resolve_cubelet_addr() {
  if [[ -n "${CUBELET_ADDR:-}" ]]; then
    echo "$CUBELET_ADDR"
    return 0
  fi
  if [[ -S /data/cubelet/cubelet.sock ]]; then
    echo "unix:///data/cubelet/cubelet.sock"
  elif ss -lntp 2>/dev/null | grep -qE ':9999\b'; then
    echo "127.0.0.1:9999"
  else
    echo "unix:///data/cubelet/cubelet.sock"
  fi
}

ensure_proxy_cri() {
  curl -sS -m 3 -o /dev/null -w 'proxy80=%{http_code}\n' -H 'Host: cube.app' "http://${NODE}:80/" || true
  wait_cubelet_ready
  mkdir -p /tmp/cube-verify/state
  killall -q cri-multiplex 2>/dev/null || true
  sleep 1
  rm -f /tmp/cube-verify/cri.sock /tmp/cube-verify/admin.sock
  local addr
  addr=$(resolve_cubelet_addr)
  log "cri-multiplex cubelet-address=$addr"
  nohup "${CRI_BIN}/cri-multiplex" \
    -cube-enabled=true -cubelet-address="$addr" \
    -socket=/tmp/cube-verify/cri.sock -admin-socket=/tmp/cube-verify/admin.sock \
    -state-dir=/tmp/cube-verify/state >"${ROOT}/cri-multiplex.log" 2>&1 &
  for i in $(seq 1 30); do
    [[ -S /tmp/cube-verify/cri.sock ]] && break
    sleep 0.5
  done
  ls -l /tmp/cube-verify/cri.sock

  if curl -sS -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:3000/templates/sandbox-code | grep -q 200; then
    TPL=sandbox-code
  else
    TPL=$(curl -sS -m 5 http://127.0.0.1:3000/templates \
      | python3 -c 'import sys,json; d=json.load(sys.stdin); print(next(x["templateID"] for x in d if x.get("status")=="READY"))' 2>/dev/null || true)
  fi
  if [[ -z "${TPL:-}" ]]; then
    log "ERROR no READY template"
    exit 2
  fi
  export CUBE_E2E_TEMPLATE="$TPL"
  log "template=$TPL"
}

native_env() {
  export CUBE_API_URL=http://127.0.0.1:3000 CUBE_API_KEY=e2b_000000
  export E2B_API_URL=http://127.0.0.1:3000 E2B_API_KEY=e2b_000000
  export E2B_DOMAIN=cube.app
  export CUBE_MASTER_IP=127.0.0.1 CUBE_MASTER_PORT=8089
  export CUBE_PROXY_NODE_IP="$NODE"
  export CUBE_PROXY_PORT_HTTP=80
  export CUBE_BENCH_YES=1
  export PIP_INDEX_URL=${PIP_INDEX_URL:-https://mirrors.cloud.tencent.com/pypi/simple/}
  cd "$BENCH_DIR"
}

cleanup_sandboxes() {
  "${CRI_BIN}/cube-e2e" cleanup 2>/dev/null || true
  cubecli unsafe rm --all 2>/dev/null || true
  sleep 2
}

parse_cri_metrics() {
  local round=$1 file=$2
  grep -E '^\[CRIMetric\]' "$file" | while read -r line; do
    c=$(echo "$line" | sed -n 's/.* c=\([0-9]*\).*/\1/p')
    avg=$(echo "$line" | sed -n 's/.* avg_ms=\([0-9.]*\).*/\1/p')
    p50=$(echo "$line" | sed -n 's/.* p50_ms=\([0-9.]*\).*/\1/p')
    p95=$(echo "$line" | sed -n 's/.* p95_ms=\([0-9.]*\).*/\1/p')
    thr=$(echo "$line" | sed -n 's/.* throughput_per_s=\([0-9.]*\).*/\1/p')
    [[ -n "$avg" ]] || continue
    echo -e "cri_create\t${round}\tc${c}_avg_ms\t${avg}" >> "$SUMMARY"
    [[ -n "$p50" ]] && echo -e "cri_create\t${round}\tc${c}_p50_ms\t${p50}" >> "$SUMMARY"
    [[ -n "$p95" ]] && echo -e "cri_create\t${round}\tc${c}_p95_ms\t${p95}" >> "$SUMMARY"
    echo -e "cri_create\t${round}\tc${c}_thr\t${thr}" >> "$SUMMARY"
  done
}

# Default: small waves (n≈c) to avoid sandbox accumulation inflating p95.
# Old CubeBench-like load: CRI_CREATE_PAIRS="1 20|10 200|20 300|50 500"
CRI_CREATE_PAIRS=${CRI_CREATE_PAIRS:-"1 20|10 20|20 20|50 50"}

run_cri_create() {
  local round=$1
  local f=$OUT/cri-create-r${round}.log
  : > "$f"
  log "CRI create round=$round pairs=${CRI_CREATE_PAIRS}"
  cleanup_sandboxes
  SOCK=unix:///tmp/cube-verify/cri.sock
  ADMIN=unix:///tmp/cube-verify/admin.sock
  local pair C N
  local _old_ifs=$IFS
  IFS='|'
  # shellcheck disable=SC2086
  for pair in $CRI_CREATE_PAIRS; do
    IFS=$' \t\n'
    # shellcheck disable=SC2086
    set -- $pair
    C=$1; N=$2
    echo "===== CRI c=$C n=$N round=$round =====" | tee -a "$f"
    "${CRI_BIN}/cube-e2e" \
      -mode concurr -c "$C" -n "$N" \
      -cri-socket "$SOCK" -admin-socket "$ADMIN" \
      -cube-api http://127.0.0.1:3000 \
      -template "$CUBE_E2E_TEMPLATE" \
      -timeout 10m \
      2>&1 | tee -a "$f" || true
    # Drop in-flight sandboxes between tiers so the next c/n is not polluted.
    cleanup_sandboxes
    IFS='|'
  done
  IFS=$_old_ifs
  parse_cri_metrics "$round" "$f"
}

run_cri_density() {
  local round=$1
  local f=$OUT/cri-density-r${round}.log
  : > "$f"
  log "CRI density round=$round"
  cleanup_sandboxes
  "${CRI_BIN}/cube-e2e" \
    -mode density -n 500 \
    -cri-socket unix:///tmp/cube-verify/cri.sock \
    -admin-socket unix:///tmp/cube-verify/admin.sock \
    -cube-api http://127.0.0.1:3000 \
    -template "$CUBE_E2E_TEMPLATE" \
    -timeout 30m \
    2>&1 | tee "$f" || true
  if grep -q 'density summary free-used:' "$f"; then
    per=$(grep 'density summary free-used:' "$f" | tail -1 | sed -n 's/.*per_vm_mb=\([0-9.]*\).*/\1/p')
  else
    per=$(grep -E 'density summary' "$f" | tail -1 | sed -n 's/.*per_vm_mb=\([0-9.]*\).*/\1/p')
  fi
  for n in 100 200 300 400 500; do
    line=$(grep -E "alive=${n} " "$f" | tail -1 || true)
    if [[ -n "$line" ]]; then
      if echo "$line" | grep -q used_per=; then
        v=$(echo "$line" | sed -n 's/.*used_per=\([0-9.]*\).*/\1/p')
      else
        v=$(echo "$line" | sed -n 's/.*per_vm_mb=\([0-9.]*\).*/\1/p')
      fi
      echo -e "cri_density\t${round}\tlive${n}_mib\t${v}" >> "$SUMMARY"
    fi
  done
  [[ -n "${per:-}" ]] && echo -e "cri_density\t${round}\tlive500_mib\t${per}" >> "$SUMMARY"
}

run_native_section() {
  local round=$1
  local sections=$2
  local tag=$3
  native_env
  local md=$OUT/native-${tag}-r${round}.md
  local lg=$OUT/native-${tag}-r${round}.log
  export OUTPUT_MD="$md"
  log "native $sections round=$round"
  cleanup_sandboxes
  bash tests/perf/cubebench.sh run -y $sections 2>&1 | tee "$lg" || true
  if [[ ! -f "$md" ]]; then
    local latest
    latest=$(ls -t /tmp/cubebench_*.md 2>/dev/null | head -1 || true)
    [[ -n "$latest" ]] && cp -a "$latest" "$md"
  fi
  if echo "$sections" | grep -q 3.2 && [[ -f "$md" ]]; then
    python3 - "$md" "$round" "$SUMMARY" <<'PY'
import re,sys
md,round,summary=sys.argv[1:4]
text=open(md,encoding='utf-8',errors='replace').read()
for m in re.finditer(r'\|\s*(\d+)\s*\|\s*\d+(?:\s*\(\d+\))?\s*\|\s*([0-9.]+)\s*ms', text):
    c,avg=m.group(1),m.group(2)
    open(summary,'a').write(f"native_create\t{round}\tc{c}_avg_ms\t{avg}\n")
PY
  fi
  if echo "$sections" | grep -q 3.3 && [[ -f "$md" ]]; then
    python3 - "$md" "$round" "$SUMMARY" <<'PY'
import re,sys
md,round,summary=sys.argv[1:4]
text=open(md,encoding='utf-8',errors='replace').read()
for m in re.finditer(r'\|\s*(\d+)\s*\|[^|\n]*\|\s*~\s*([0-9.]+)\s*MiB', text):
    live,per=m.group(1),m.group(2)
    if live in {'100','200','300','400','500'}:
        open(summary,'a').write(f"native_density\t{round}\tlive{live}_mib\t{per}\n")
PY
  fi
  if [[ "$tag" == "4x" && -f "$md" ]]; then
    # Parse wall/snapshot avg columns (not Rounds / Dirty Page).
    python3 - "$md" "$round" "$SUMMARY" <<'PY'
import re,sys
md,round,summary=sys.argv[1:4]
text=open(md,encoding='utf-8',errors='replace').read()

def w(name, val):
    open(summary,'a').write(f"native_4x\t{round}\t{name}\t{val}\n")

m=re.search(r'### 4\.1[\s\S]*?\n\|\s*1\s*\|\s*\d+\s*\|\s*([0-9.]+)', text)
if m: w('snap_c1_wall', m.group(1))

m=re.search(r'### 4\.2[\s\S]*?\n\|\s*0\s*\|\s*[0-9.]+\s*\|\s*([0-9.]+)', text)
if m: w('dirty0_snap', m.group(1))
m=re.search(r'### 4\.2[\s\S]*?\n\|\s*1024\s*\|\s*[0-9.]+\s*\|\s*([0-9.]+)', text)
if m: w('dirty1024_snap', m.group(1))

m=re.search(r'### 4\.3[\s\S]*?\n\|\s*1\s*\|\s*\d+\s*\|\s*\d+\s*\|\s*([0-9.]+)', text)
if m: w('fromsnap_c1_wall', m.group(1))

m=re.search(r'### 4\.4[\s\S]*?\n\|\s*1\s*\|\s*\d+\s*\|\s*([0-9.]+)', text)
if m: w('rollback_c1_wall', m.group(1))

m=re.search(r'### 4\.5[\s\S]*?\n\|\s*1\s*\|\s*1\s*\|\s*\d+\s*\|\s*([0-9.]+)', text)
if m: w('clone_c1_wall', m.group(1))

# 4.6: Concurrency | Rounds | pause_wall | ... | per_pause | resume_wall | ...
m=re.search(
    r'### 4\.6[\s\S]*?\n\|\s*1\s*\|\s*\d+\s*\|\s*([0-9.]+)\s*\|(?:[^|\n]*\|){4}\s*([0-9.]+)',
    text,
)
if m:
    w('pause_c1', m.group(1))
    w('resume_c1', m.group(2))
PY
  fi
}

finalize_stats() {
  log "computing stats"
  python3 - "$SUMMARY" "$ROOT/stability-report.md" "$STABILITY_TITLE" "$ROOT" <<'PY'
import collections,sys,statistics
src,dst,title,root=sys.argv[1:5]
rows=[]
with open(src,encoding='utf-8') as f:
    next(f)
    for line in f:
        line=line.strip()
        if not line: continue
        kind,round,metric,value=line.split('\t')
        try: value=float(value)
        except: continue
        rows.append((kind,metric,value,int(round)))
seen=set(); cleaned=[]
for kind,metric,value,round in rows:
    key=(kind,metric,round)
    if key in seen: continue
    seen.add(key)
    cleaned.append((kind,metric,value,round))
by=collections.defaultdict(list)
for kind,metric,value,round in cleaned:
    by[(kind,metric)].append((round,value))

def stats(vals):
    vals=sorted(vals)
    n=len(vals)
    mean=sum(vals)/n
    if n>=2:
        sd=statistics.stdev(vals)
        cv=sd/mean*100 if mean else 0
    else:
        sd=0; cv=0
    return n,mean,min(vals),max(vals),sd,cv

lines=[]
lines.append(f'# {title}')
lines.append('')
lines.append(f'> ROUNDS from `{src}` · out `{root}/out/`')
lines.append('')
lines.append('| 类别 | 指标 | n | 均值 | 最小 | 最大 | 标准差 | CV% | 各轮 |')
lines.append('|:--|:--|---:|---:|---:|---:|---:|---:|:--|')
order=['cri_create','native_create','cri_density','native_density','native_4x']
for kind in order:
    items=[(k,m,v) for (k,m),v in by.items() if k==kind]
    items.sort(key=lambda x: x[1])
    for _,metric,pairs in items:
        vals=[v for _,v in sorted(pairs)]
        n,mean,mn,mx,sd,cv=stats(vals)
        rounds=', '.join(f'{v:.2f}' for v in vals)
        lines.append(f'| {kind} | {metric} | {n} | **{mean:.2f}** | {mn:.2f} | {mx:.2f} | {sd:.2f} | {cv:.1f} | {rounds} |')
lines.append('')
lines.append('## 解读')
lines.append('')
lines.append('- **CV%** = 标准差/均值×100；Create c=1 一般希望 CV 个位数到十余。')
lines.append('- 密度 Live=500 以 MiB/沙箱计；§4.x 为 c=1 wall/snapshot avg（ms）。')
lines.append(f'- 原始：`{root}/out/` · TSV：`summary.tsv`')
open(dst,'w',encoding='utf-8').write('\n'.join(lines)+'\n')
print('wrote',dst)
PY
}

# ---- main ----
pkill -f 'tests/perf/cubebench.sh' 2>/dev/null || true
pkill -f 'cube-e2e' 2>/dev/null || true
sleep 1
ensure_oe_xfs_data
mkdir -p "$OUT" "$ROOT/rounds" /tmp/cube-verify/state
: > "$SUMMARY"
echo -e "kind\tround\tmetric\tvalue" >> "$SUMMARY"
: > "$ROOT/run.log"
log "START rounds=$ROUNDS node=$NODE root=$ROOT"
qout=$(ensure_quota)
log "quota: $qout"
if echo "$qout" | grep -q quota_patched; then
  systemctl restart cube-sandbox-cubelet.service || true
fi
systemctl is-active cube-sandbox-cubelet.service cube-sandbox-cube-api.service cube-sandbox-cubemaster.service || true
ensure_proxy_cri

log "warm native 3.2 once"
native_env
cleanup_sandboxes
bash tests/perf/cubebench.sh run -y 3.2 2>&1 | tee "$OUT/warm-32.log" || true

for r in $(seq 1 "$ROUNDS"); do
  log "======== ROUND $r / $ROUNDS ========"
  ensure_proxy_cri
  run_cri_create "$r"
  run_native_section "$r" "3.2" "32"
  run_native_section "$r" "4.1 4.2 4.3 4.4 4.5 4.6" "4x"
  run_native_section "$r" "3.3" "33"
  run_cri_density "$r"
  log "ROUND $r done"
done

finalize_stats
log "ALL_STABILITY_OK"
ls -la "$ROOT" "$OUT" | tee -a "$ROOT/run.log"

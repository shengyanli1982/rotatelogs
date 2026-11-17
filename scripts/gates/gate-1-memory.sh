#!/usr/bin/env bash
# gate-1 内存稳定（§14.2-1）：
# yes | head -c 1GB 持续写入 + -s 10 -t 0 高频切分（约 103 次），
# macOS /usr/bin/time -l（bytes）/ Linux /usr/bin/time -v（kbytes）测 max RSS，
# 断言 ≤ 16MB（2026-11-13 用户裁定修订，原 <10MB 与 Go runtime 基线 ~8MB 物理冲突）。耗时分钟级属预期，禁止缩水数据量（runbook focus_prompt）。

set -euo pipefail
export LC_ALL=C

GATE_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$GATE_DIR/../.." && pwd)"
BIN="${ROTATELOGS_BIN:-$REPO_ROOT/bin/rotatelogs}"
# 默认 bin 每次强制重建，防止陈旧二进制静默测试旧代码；显式 ROTATELOGS_BIN 覆盖时按原样使用
[ -n "${ROTATELOGS_BIN:-}" ] || (cd "$REPO_ROOT" && go build -o bin/rotatelogs .)

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

echo "gate-1: 1GB 写入 + -s 10 -t 0（约 103 次切分），采样 max RSS …"
uname_s=$(uname -s)

# 管道中 yes 因 head 关闭管道收到 SIGPIPE 属预期，用 PIPESTATUS 只取 time 的退出码
set +o pipefail
if [ "$uname_s" = "Darwin" ]; then
	yes | head -c 1073741824 | /usr/bin/time -l "$BIN" -s 10 -t 0 "$T/m.log" 2> "$T/time.out"
	st=${PIPESTATUS[2]}
else
	yes | head -c 1073741824 | /usr/bin/time -v "$BIN" -s 10 -t 0 "$T/m.log" 2> "$T/time.out"
	st=${PIPESTATUS[2]}
fi
set -o pipefail

[ "$st" = "0" ] || { echo "GATE1 FAIL: rotatelogs exit=$st" >&2; cat "$T/time.out" >&2; exit 1; }

# 提取 max RSS（darwin: "maximum resident set size" 单位 bytes；linux: kbytes）
if [ "$uname_s" = "Darwin" ]; then
	rss=$(grep "maximum resident set size" "$T/time.out" | awk '{print $1}')
	limit=16777216
	unit="bytes"
else
	rss=$(grep "Maximum resident set size" "$T/time.out" | awk '{print $NF}')
	limit=16384
	unit="kbytes"
fi
[ -n "$rss" ] || { echo "GATE1 FAIL: 未能从 time 输出解析 RSS" >&2; cat "$T/time.out" >&2; exit 1; }

# 证明确实发生了高频切分（1GB/10MB ≈ 103 个文件）
files=$(find "$T" -maxdepth 1 -type f -name 'm.log*' | wc -l | tr -d ' ')
[ "$files" -ge 100 ] || { echo "GATE1 FAIL: 切分文件数 $files < 100（-s 10 未生效？）" >&2; exit 1; }

echo "  max RSS = $rss $unit（限制 < $limit $unit = 16MB）"
echo "  切分文件数 = $files"
if [ "$rss" -lt "$limit" ]; then
	echo "GATE1 PASS: 1GB 写入 max RSS ${rss} ${unit} ≤ 16MB，内存恒定（切分 ${files} 次）"
	grep -E "maximum resident set size|Maximum resident set size" "$T/time.out"
else
	echo "GATE1 FAIL: max RSS ${rss} ${unit} > 16MB" >&2
	exit 1
fi

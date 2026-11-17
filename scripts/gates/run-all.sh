#!/usr/bin/env bash
# run-all.sh — 顺序执行全部门禁（gate-1~4），汇总 PASS/FAIL。
# gate-1 需写入 1GB（分钟级、需 ≥2GB 空闲磁盘），属预期耗时。
# 用法: scripts/gates/run-all.sh

set -uo pipefail
export LC_ALL=C

DIR="$(cd "$(dirname "$0")" && pwd)"

(cd "$DIR/../.." && go build -o bin/rotatelogs .) || { echo "BUILD FAILED" >&2; exit 1; }

failed=0
total=0
for g in "$DIR"/gate-*.sh; do
	total=$((total + 1))
	echo "=== $(basename "$g") ==="
	if bash "$g"; then
		:
	else
		failed=$((failed + 1))
		echo "--- ^^ FAILED: $(basename "$g")" >&2
	fi
	echo
done

echo "================================"
if [ "$failed" -eq 0 ]; then
	echo "GATES ALL PASS ($total/$total)"
	exit 0
fi
echo "GATES FAILED: $failed/$total 项失败" >&2
exit 1

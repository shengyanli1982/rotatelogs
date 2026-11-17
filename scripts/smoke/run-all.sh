#!/usr/bin/env bash
# run-all.sh — 顺序执行全部冒烟脚本（smoke-1~9），汇总 PASS/FAIL。
# 任一失败 → 总退出码 1；失败不中断后续项（一次跑完收集全部证据）。
# 用法: scripts/smoke/run-all.sh

set -uo pipefail
export LC_ALL=C

DIR="$(cd "$(dirname "$0")" && pwd)"

# 预构建二进制，供全部子脚本复用
(cd "$DIR/../.." && go build -o bin/rotatelogs .) || { echo "BUILD FAILED" >&2; exit 1; }

failed=0
total=0
for s in "$DIR"/smoke-*.sh; do
	total=$((total + 1))
	echo "=== $(basename "$s") ==="
	if bash "$s"; then
		:
	else
		failed=$((failed + 1))
		echo "--- ^^ FAILED: $(basename "$s")" >&2
	fi
	echo
done

echo "================================"
if [ "$failed" -eq 0 ]; then
	echo "SMOKE ALL PASS ($total/$total)"
	exit 0
fi
echo "SMOKE FAILED: $failed/$total 项失败" >&2
exit 1

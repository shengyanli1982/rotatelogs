#!/usr/bin/env bash
# smoke-2 EOF 立即退出（§14.1-10）：
# 管道关闭 → 进程立即 exit 0（无挂起，限时断言），-v 下 stderr 仍全空，
# 数据落盘完整。固定名 pattern 显式 -t 0：隐式默认会降级并输出一行警告
# （破坏 stderr 全空断言），显式 -t >0 触发 M2 硬报错（2026-11-16 意图降级变更）。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

printf 'x' > "$T/in"
start=$(date +%s)
"$BIN" -v -t 0 "$T/a.log" < "$T/in" > /dev/null 2> "$T/stderr" &
PID=$!
wait_with_timeout "$PID" 5; st=$ST
PID=""
elapsed=$(( $(date +%s) - start ))

[ "$st" = "0" ] || fail "smoke-2: exit=$st, want 0 (EOF 应立即优雅退出)"
[ "$elapsed" -lt 3 ] || fail "smoke-2: took ${elapsed}s, EOF 退出不应挂起"
cmp "$T/a.log" "$T/in" || fail "smoke-2: content mismatch"
[ ! -s "$T/stderr" ] || fail "smoke-2: stderr not empty: $(cat "$T/stderr")"

pass "smoke-2 EOF 立即退出（exit=0，${elapsed}s < 3s，stderr 空）"

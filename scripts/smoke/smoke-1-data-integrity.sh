#!/usr/bin/env bash
# smoke-1 数据完整性（§14.1-1）+ strftime 文件名核对（§14.1-12 旁证）：
# printf 'hello\n' | rotatelogs pattern → exit 0，文件字节与输入 cmp 一致，
# 文件名含今日 UTC 日期。EOF 正常路径 stderr 必须全空（即使 -v）。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir
PATTERN="$T/app-%Y%m%d.log"
EXPECTED="$T/expected"
printf 'hello\n' > "$EXPECTED"

printf 'hello\n' > "$T/in"
"$BIN" -v "$PATTERN" < "$T/in" > /dev/null 2> "$T/stderr" &
PID=$!
wait_with_timeout "$PID" 10; st=$ST
PID=""

[ "$st" = "0" ] || fail "smoke-1: exit=$st, want 0"

OUT="$T/app-$(date -u +%Y%m%d).log"
[ -f "$OUT" ] || fail "smoke-1: expected file $OUT not found (strftime %Y%m%d, UTC); got: $(ls "$T")"
cmp "$OUT" "$EXPECTED" || fail "smoke-1: content mismatch"
[ ! -s "$T/stderr" ] || fail "smoke-1: stderr not empty: $(cat "$T/stderr")"

pass "smoke-1 数据完整性（exit=0，字节一致，文件名含 UTC 日期，stderr 空）"

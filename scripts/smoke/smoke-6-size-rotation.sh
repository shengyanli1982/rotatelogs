#!/usr/bin/env bash
# smoke-6 大小切分 E2E（§14.1-3，D1 分片）：
# 200MB 输入 + -s 50 -t 0 → ≥4 文件且每个 ≤50MB、按生成顺序拼接后与输入 cmp 一致。
# 固定名 + -t 0 的切分命名序：big.log → big.log.1 → big.log.2 …（旧文件保留原名，
# 新活跃文件冲突追加序号），拼接顺序即该序号序（runbook 提示的 ls -tr 备用序不必需）。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

head -c 209715200 /dev/urandom > "$T/input.bin" # 200MB

"$BIN" -t 0 -s 50 "$T/big.log" < "$T/input.bin" > /dev/null 2> "$T/stderr" &
PID=$!
wait_with_timeout "$PID" 120; st=$ST
PID=""
[ "$st" = "0" ] || fail "smoke-6: exit=$st, want 0; stderr: $(cat "$T/stderr" 2>/dev/null)"

# 按命名约定构造生成序文件列表：big.log, big.log.1, big.log.2 …
ordered="$T/big.log"
count=1
i=1
while [ -f "$T/big.log.$i" ]; do
	ordered="$ordered $T/big.log.$i"
	count=$((count + 1))
	i=$((i + 1))
done
# 不允许出现命名序之外的意外文件（input.bin/stderr 除外）
extra=$(find "$T" -maxdepth 1 -type f -name 'big.log*' | wc -l | tr -d ' ')
[ "$extra" = "$count" ] || fail "smoke-6: 发现序号序之外的 big.log* 文件（$extra vs $count）"

[ "$count" -ge 4 ] || fail "smoke-6: 文件数 $count < 4"

# 每文件 ≤ 50MB（darwin: stat -f%z / linux: stat -c%s）
for f in $ordered; do
	sz=$(stat -f%z "$f" 2>/dev/null || stat -c%s "$f")
	[ "$sz" -le 52428800 ] || fail "smoke-6: OVERSIZE $f ($sz > 52428800)"
done

# shellcheck disable=SC2086
cat $ordered > "$T/rebuilt"
cmp "$T/input.bin" "$T/rebuilt" || fail "smoke-6: 拼接还原与输入不一致"
[ ! -s "$T/stderr" ] || fail "smoke-6: stderr not empty: $(cat "$T/stderr")"

pass "smoke-6 大小切分 E2E（${count} 文件，全部 ≤50MB，拼接还原 cmp 一致，exit=0）"

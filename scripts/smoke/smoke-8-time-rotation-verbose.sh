#!/usr/bin/env bash
# smoke-8 时间切分 E2E（§14.1-2）+ verbose 输出（dispatch 清单补充项）：
# -t 1s + 含 %H%M%S 占位符 pattern（满足 M2），FIFO 持续写入约 3.2s →
# 跨越 ≥2 个自然秒边界（D2 对齐）→ ≥3 个文件；按文件名时序拼接与输入一致；
# -v stderr 出现 ≥2 行 "rotatelogs: rotated" 切分事件。
# 守卫：跨 UTC 午夜（%H%M%S 回绕破坏字典序=时序假设）时报错要求重跑。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

start_fifo_reader -v -t 1s "$T/app-%H%M%S.log"
open_fifo_writer
day_before=$(date -u +%j)

i=0
while [ "$i" -lt 320 ]; do
	line=$(printf 'line-%04d-aaaaaaaaaaaaaaaaaaaaaaaa\n' "$i")
	printf '%s' "$line" >&3
	printf '%s' "$line" >> "$T/expected"
	sleep 0.01
	i=$((i + 1))
done

close_fifo_writer
wait_with_timeout "$PID" 10; st=$ST
PID=""
day_after=$(date -u +%j)
[ "$day_before" = "$day_after" ] || fail "smoke-8: 测试期间跨 UTC 午夜，文件名时序假设失效，请重跑"

[ "$st" = "0" ] || fail "smoke-8: exit=$st, want 0; stderr: $(cat "$T/stderr" 2>/dev/null)"

count=$(find "$T" -maxdepth 1 -type f -name 'app-*.log*' | wc -l | tr -d ' ')
[ "$count" -ge 3 ] || fail "smoke-8: 文件数 $count < 3（-t 1s 持续写 3.2s 应切分 ≥2 次）; got: $(ls "$T")"

# 同小时内 %H%M%S 字典序=时序；同秒冲突的 .N 后缀字典序也正确（app-X.log < app-X.log.1）
# shellcheck disable=SC2046
cat $(find "$T" -maxdepth 1 -type f -name 'app-*.log*' | sort) > "$T/rebuilt"
cmp "$T/expected" "$T/rebuilt" || fail "smoke-8: 时序拼接与输入不一致"

rotated=$(grep -c '^rotatelogs: rotated ' "$T/stderr" || true)
[ "$rotated" -ge 2 ] || fail "smoke-8: verbose 切分事件 $rotated < 2; stderr: $(cat "$T/stderr")"

pass "smoke-8 时间切分+verbose（${count} 文件，拼接一致，rotated 事件 ${rotated} 行，exit=0）"

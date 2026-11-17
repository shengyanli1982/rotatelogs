#!/usr/bin/env bash
# smoke-3 SIGHUP 强制切分（§14.1-4）：
# mkfifo 保持 stdin 开启：写 one → kill -HUP → 写 two（HUP 后须再写数据才触发
# 切分，ST-04 移交要点 2）→ 关写端 EOF → exit 0。
# 固定名 + -t 0 产物：h.log（旧数据 one）+ h.log.1（新活跃文件 two）。
# 注意：不能用 runbook 原文的 "-t 0 -s 0"——M1 校验（Addendum 2 §3.2）拒绝
# 双 0 配置（exit 1）；保留默认 -s 100，几字节数据不会触发大小切分。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

start_fifo_reader -v -t 0 "$T/h.log"
open_fifo_writer
printf 'one\n' >&3
sleep 0.3
kill -HUP "$PID"
sleep 0.3
printf 'two\n' >&3
sleep 0.3
close_fifo_writer # 关写端 → EOF 优雅退出
wait_with_timeout "$PID" 10; st=$ST
PID=""

[ "$st" = "0" ] || fail "smoke-3: exit=$st, want 0; stderr: $(cat "$T/stderr" 2>/dev/null)"
[ -f "$T/h.log" ] || fail "smoke-3: h.log missing; got: $(ls "$T")"
[ -f "$T/h.log.1" ] || fail "smoke-3: h.log.1 missing (SIGHUP 未切分); got: $(ls "$T")"
cmp "$T/h.log" <(printf 'one\n') || fail "smoke-3: h.log should hold pre-HUP data 'one'"
cmp "$T/h.log.1" <(printf 'two\n') || fail "smoke-3: h.log.1 should hold post-HUP data 'two'"
grep -q "rotatelogs: rotated $T/h.log -> $T/h.log.1" "$T/stderr" \
	|| fail "smoke-3: verbose 切分事件缺失; stderr: $(cat "$T/stderr")"

pass "smoke-3 SIGHUP 切分（h.log=one / h.log.1=two，verbose 事件命中，exit=0）"

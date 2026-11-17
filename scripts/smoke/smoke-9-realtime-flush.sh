#!/usr/bin/env bash
# smoke-9 心跳实时可见性（DESIGN §4.1 addendum，痛点修复验证）：
# FIFO 写入一行后不关写端、不退出进程，sleep 3s（> FlushIdleTimeout 2s +
# FlushHeartbeatInterval 300ms，确保心跳已刷），断言进程仍在运行时日志文件
# 已含该行（tail -f 近实时可见）。A′ 为纯心跳非行级即时：可见性上界 =
# idleTimeout 2s，必须 sleep 超过该阈值。随后关写端 EOF 优雅退出 exit 0，
# stderr 全空。遵循 common.sh D7 时序与 wait_with_timeout（禁止命令替换取 ST）。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

start_fifo_reader "$T/r-%Y%m%d.log"
open_fifo_writer
printf 'realtime-probe\n' >&3
sleep 3

# 核心断言：进程仍在运行（未 Close、未 EOF），数据已经心跳 flush 落盘。
kill -0 "$PID" 2>/dev/null || fail "smoke-9: reader exited prematurely"
OUT="$T/r-$(date -u +%Y%m%d).log"
[ -f "$OUT" ] || fail "smoke-9: log file $OUT not found while running; got: $(ls "$T")"
grep -q 'realtime-probe' "$OUT" \
	|| fail "smoke-9: line not visible while running (heartbeat idle flush failed): $(cat "$OUT")"

close_fifo_writer
wait_with_timeout "$PID" 10; st=$ST
PID=""
[ "$st" = "0" ] || fail "smoke-9: exit=$st, want 0"
[ ! -s "$T/stderr" ] || fail "smoke-9: stderr not empty: $(cat "$T/stderr")"

pass "smoke-9 心跳实时可见性（运行中 sleep 3s 后行已落盘，EOF exit 0，stderr 空）"
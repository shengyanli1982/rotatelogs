#!/usr/bin/env bash
# smoke-4 SIGTERM 优雅关停（§14.1-9，D7 时序）：
# FIFO 写入数据 → kill -TERM（进程不立即退出：阻塞 Read 不响应 cancel，
# ST-04 移交要点 1）→ 关闭 FIFO 写端（exec 3>&-）解除阻塞 → exit 0，
# 已写数据完整无截断。固定名 pattern 显式 -t 0（隐式默认降级会输出警告，
# 破坏 stderr 全空断言；显式 -t >0 触发 M2 硬报错）。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

start_fifo_reader -t 0 "$T/t.log"
open_fifo_writer
printf 'data-before-term\n' >&3
sleep 0.3
kill -TERM "$PID"
sleep 0.1
close_fifo_writer # D7: 必须关闭 stdin 写端解除阻塞 Read
wait_with_timeout "$PID" 10; st=$ST
PID=""

[ "$st" = "0" ] || fail "smoke-4: exit=$st, want 0 (SIGTERM+关写端应优雅退出); stderr: $(cat "$T/stderr" 2>/dev/null)"
cmp "$T/t.log" <(printf 'data-before-term\n') \
	|| fail "smoke-4: 内容截断/丢失（bufio 未 flush？）"
[ ! -s "$T/stderr" ] || fail "smoke-4: stderr not empty: $(cat "$T/stderr")"

pass "smoke-4 SIGTERM 优雅关停（D7 时序，exit=0，内容完整无截断）"

#!/usr/bin/env bash
# smoke-5 symlink 指向（§14.1-8）：
# -l 运行 + SIGHUP 触发切分 → readlink 始终指向最新活跃文件。
# 同目录 symlink 使用相对路径（§4.5）：期望 readlink 输出 "s.log.1"。
# 注意：不能用 runbook 原文的 "-t 0 -s 0"——M1 校验拒绝；保留默认 -s 100。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

start_fifo_reader -v -t 0 -l "$T/current.log" "$T/s.log"
open_fifo_writer # FIFO 读端在写端打开前阻塞，必须先开写端进程才真正启动

# 启动即断言：symlink 指向初始活跃文件（§4.5 启动时必须创建/更新）
sleep 0.3
link=$(readlink "$T/current.log" 2>/dev/null) || fail "smoke-5: current.log 不是 symlink 或不存在"
[ "$link" = "s.log" ] || fail "smoke-5: 启动时 readlink=$link, want s.log（同目录相对路径）"

printf 'a' >&3
sleep 0.2
kill -HUP "$PID"
sleep 0.2
printf 'b' >&3 # HUP 后须再写数据触发切分
sleep 0.2
close_fifo_writer
wait_with_timeout "$PID" 10; st=$ST
PID=""

[ "$st" = "0" ] || fail "smoke-5: exit=$st, want 0; stderr: $(cat "$T/stderr" 2>/dev/null)"
link=$(readlink "$T/current.log") || fail "smoke-5: readlink failed"
[ "$link" = "s.log.1" ] || fail "smoke-5: 切分后 readlink=$link, want s.log.1"
cmp "$T/s.log" <(printf 'a') || fail "smoke-5: s.log content"
cmp "$T/s.log.1" <(printf 'b') || fail "smoke-5: s.log.1 content"

pass "smoke-5 symlink（启动→s.log，切分→s.log.1，相对路径，exit=0）"

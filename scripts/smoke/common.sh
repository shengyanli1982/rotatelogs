#!/usr/bin/env bash
# common.sh — 冒烟脚本共享工具（被 source，不单独执行）。
#
# 约定（ST-04 移交的二进制行为要点，脚本必须遵守）：
#   D7 信号时序: SIGTERM/SIGINT 到达后进程不立即退出——阻塞的 stdin Read 不响应
#     cancel。必须再写入一行数据或关闭 FIFO 写端（exec 3>&-）解除阻塞，
#     进程随后优雅退出 exit 0。
#   SIGHUP: 置位 flag 后须再有数据写入才触发切分。
#   固定文件名（无 strftime 占位符）pattern：隐式默认 -t 24h 降级为仅大小切分
#     并输出一行警告（2026-11-16 意图降级变更）；显式 -t >0 触发 M2 硬报错 exit 1。
#     需要 stderr 全空的用例必须显式 -t 0；-s/-t 不能同时为 0（M1 校验），默认 -s 100 即可满足。
#   错误路径: 恰一行 stderr；缺参 exit 1；M1 违规 exit 1；EOF 正常路径 exit 0 且 stderr 全空。

set -euo pipefail
export LC_ALL=C

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
BIN="${ROTATELOGS_BIN:-$REPO_ROOT/bin/rotatelogs}"

fail() { echo "FAIL: $*" >&2; exit 1; }

pass() { echo "PASS: $*"; }

# ensure_bin 确保被测二进制存在（缺失时在仓库根构建）。
ensure_bin() {
	if [ ! -x "$BIN" ]; then
		(cd "$REPO_ROOT" && go build -o bin/rotatelogs .)
	fi
	[ -x "$BIN" ] || fail "binary not found: $BIN"
}

# make_tmpdir 创建临时目录并注册 EXIT 清理（含 fd3 与后台进程兜底回收）。
# 必须直接调用（设置全局 T）；禁止经命令替换取返回值 —— 命令替换在子 shell
# 中执行，子 shell 退出即触发 EXIT trap 把目录删掉。
make_tmpdir() {
	T="$(mktemp -d)"
	trap cleanup EXIT
}

cleanup() {
	# shellcheck disable=SC2064
	trap - EXIT
	exec 3>&- 2>/dev/null || true
	if [ -n "${PID:-}" ]; then kill -KILL "$PID" 2>/dev/null || true; fi
	if [ -n "${WRITER:-}" ]; then kill -KILL "$WRITER" 2>/dev/null || true; fi
	if [ -n "${T:-}" ] && [ -d "$T" ]; then rm -rf "$T"; fi
}

# wait_with_timeout PID SECS — 等待后台子进程退出，结果写入全局 ST；
# 超时则 SIGKILL 并置 ST=124（防产品假死拖挂整个冒烟）。
# 必须在当前 shell 直接调用（wait 只能等本 shell 的子进程）；
# 禁止 ST=$(wait_with_timeout …) —— 命令替换子 shell 中 wait 不生效。
wait_with_timeout() {
	local pid=$1 secs=$2
	( sleep "$secs"; kill -KILL "$pid" 2>/dev/null ) &
	local wd=$!
	ST=0
	wait "$pid" || ST=$?
	kill "$wd" 2>/dev/null || true
	wait "$wd" 2>/dev/null || true
	if [ "$ST" -eq 137 ]; then
		# 仅看门狗会发 SIGKILL；产品自身退出码只有 0/1/2
		ST=124
	fi
}

# start_fifo_reader ARGS... — 以 FIFO 为 stdin 后台启动二进制（调用方先 mkfifo $T/fifo）。
# 结果: 全局 PID=进程号；随后调用方必须 open_fifo_writer 打开写端。
start_fifo_reader() {
	mkfifo "$T/fifo"
	"$BIN" "$@" < "$T/fifo" > /dev/null 2> "$T/stderr" &
	PID=$!
}

# open_fifo_writer 打开 FIFO 写端并持有（fd 3），防止 EOF 提前触发。
open_fifo_writer() {
	exec 3> "$T/fifo"
}

# close_fifo_writer 关闭写端（D7：解除阻塞 Read / 触发 EOF 优雅退出）。
close_fifo_writer() {
	exec 3>&-
}

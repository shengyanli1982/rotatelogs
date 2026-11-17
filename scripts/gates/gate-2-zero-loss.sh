#!/usr/bin/env bash
# gate-2 零数据丢失（§14.2-2，D7 时序）：
# 已知序列经 FIFO 持续写入 → 中途 kill -TERM → 关闭脚本持有的写端 fd3 解除
# 阻塞（ST-04 移交要点 1；若写端进程仍在写，其数据本身也会唤醒 Read，双保险）
# → exit 0 → 文件内容 == 输入前缀且末行完整（wc -l + head + cmp 三重校验：
# 若末行被截断，wc -l 不计该行而 cmp 会因多出残字节失败）。
#
# 与 runbook Step 6.3 的两处刻意偏差（实测驱动）：
# 1) 序列生成器 seq → awk：BSD seq（macOS）对大数值退化为 %g 科学计数法，
#    `seq 1 2000000` 末尾约 5 万行全为重复的 "2e+06"（实测 wc -l 甚至为
#    2000001），"已知序列"失去唯一性；awk printf "%d\n" 精确无重复。
# 2) 数据量 2M → 8M 行（约 63MB）：实测 2M 行 <1s 即写完，kill -TERM 落在
#    空闲期而非"中途"；8M 行确保 TERM 时写入仍在进行（不缩水，只加强）。
# 配置说明：固定名 pattern 显式 -t 0（隐式默认降级为警告、显式 -t >0 触发 M2
# 硬报错，2026-11-16 意图降级变更）；-s 100 保证 63MB 输入不触发大小
# 切分（单文件对账）；不能用 runbook 原文 "-s 0 -t 0"（M1 校验拒绝）。

set -euo pipefail
export LC_ALL=C

GATE_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$GATE_DIR/../.." && pwd)"
BIN="${ROTATELOGS_BIN:-$REPO_ROOT/bin/rotatelogs}"
# 默认 bin 每次强制重建，防止陈旧二进制静默测试旧代码；显式 ROTATELOGS_BIN 覆盖时按原样使用
[ -n "${ROTATELOGS_BIN:-}" ] || (cd "$REPO_ROOT" && go build -o bin/rotatelogs .)

LINES=8000000
gen() { awk -v n="$LINES" 'BEGIN { for (i = 1; i <= n; i++) printf "%d\n", i }'; }

T="$(mktemp -d)"
PID=""
WRITER=""
cleanup() {
	trap - EXIT
	exec 3>&- 2>/dev/null || true
	if [ -n "$PID" ]; then kill -KILL "$PID" 2>/dev/null || true; fi
	if [ -n "$WRITER" ]; then kill -KILL "$WRITER" 2>/dev/null || true; fi
	if [ -n "$T" ] && [ -d "$T" ]; then rm -rf "$T"; fi
}
trap cleanup EXIT

mkfifo "$T/fifo"
"$BIN" -s 100 -t 0 "$T/d.log" < "$T/fifo" > /dev/null 2> "$T/stderr" &
PID=$!
exec 3> "$T/fifo" # 脚本持有写端：生成器提前写完也不会触发 EOF
gen >&3 &
WRITER=$!

sleep 1 # 数据流动约 1 秒后中途终止
kill -TERM "$PID"
exec 3>&- # D7: 关闭最后的写端 → EOF 解除阻塞 Read

st=0
( sleep 15; kill -KILL "$PID" 2>/dev/null ) & wd=$! # 看门狗：防产品假死拖挂门禁
wait "$PID" || st=$?
kill "$wd" 2>/dev/null || true
wait "$wd" 2>/dev/null || true
PID=""
kill "$WRITER" 2>/dev/null || true # 生成器可能已因 SIGPIPE 退出
wait "$WRITER" 2>/dev/null || true
WRITER=""

if [ "$st" = "137" ]; then
	echo "GATE2 FAIL: kill -TERM + 关写端后进程 15s 未退出（假死，D7 时序未生效？）" >&2
	exit 1
fi
[ "$st" = "0" ] || { echo "GATE2 FAIL: exit=$st, want 0; stderr: $(cat "$T/stderr")" >&2; exit 1; }

[ -s "$T/d.log" ] || { echo "GATE2 FAIL: d.log 为空（数据全部丢失）" >&2; exit 1; }
N=$(wc -l < "$T/d.log" | tr -d ' ')
[ "$N" -gt 0 ] || { echo "GATE2 FAIL: d.log 无完整行（末行截断）" >&2; exit 1; }
if [ "$N" -ge "$LINES" ]; then
	echo "  注意: 生成器在 TERM 前已写完全部 $LINES 行（本机极快），仍验证零丢失"
fi

# 文件 == 输入前 N 行（无丢失、无重复、无截断）
cmp <(gen | head -n "$N") "$T/d.log" \
	|| { echo "GATE2 FAIL: 文件内容与输入前缀不一致" >&2; exit 1; }

# 末行完整性显式断言：最后一个字节必须是换行符
last=$(tail -c 1 "$T/d.log" | od -An -c | tr -d ' ')
[ "$last" = "\\n" ] || { echo "GATE2 FAIL: 末行截断（最后字节 [$last] != \\n）" >&2; exit 1; }

echo "GATE2 PASS: 中途 kill -TERM 后 exit 0；落盘 $N/$LINES 行 == 输入前缀，末行完整无截断"

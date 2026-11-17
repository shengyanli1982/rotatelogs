#!/usr/bin/env bash
# smoke-7 启动失败退出码（§4.8 行1、§3.5）+ 错误路径实测断言（ST-04 移交要点 3）：
#   a) 缺参           → exit 1，恰一行 stderr "rotatelogs: accepts 1 arg(s), received 0"
#   b) M1 违规(-s 0 -t 0) → exit 1，恰一行 stderr（Addendum 2 §3.2）
#   c) M2 违规(固定名+显式 -t 1h) → exit 1，恰一行 stderr（2026-11-16 意图降级
#      变更后：隐式默认 -t 24h 降级为 size-only+警告，不再触发 M2；显式 -t >0 才硬报错）
#   d) 目录无法创建(-t 0 过校验后 MkdirAll 失败) → exit 1，恰一行 stderr
# 说明: macOS 根目录只读（sealed volume），/nonexistent-root-xyz 对任何用户都失败。

. "$(cd "$(dirname "$0")" && pwd)/common.sh"

ensure_bin
make_tmpdir

# assert_startup_failure NAME EXPECTED_STDERR ARGS...
# 断言 exit 1、stderr 恰一行、且与期望逐字一致。
assert_startup_failure() {
	local name=$1 expected=$2
	shift 2
	local errf="$T/err-$name"
	local st=0
	"$BIN" "$@" < /dev/null > /dev/null 2> "$errf" &
	local pid=$!
	wait_with_timeout "$pid" 10; st=$ST
	[ "$st" = "1" ] || fail "smoke-7($name): exit=$st, want 1"
	local lines
	lines=$(wc -l < "$errf" | tr -d ' ')
	[ "$lines" = "1" ] || fail "smoke-7($name): stderr 应恰一行，实际 $lines 行: $(cat "$errf")"
	local actual
	actual=$(cat "$errf")
	[ "$actual" = "$expected" ] || fail "smoke-7($name): stderr=[$actual], want=[$expected]"
	echo "  ok: $name → exit 1, stderr=[$actual]"
}

# cobra 层错误经 execute 的 fmt.Fprintln(stderr, MsgPrefix, err)：Fprintln 在
# 操作数间加空格、MsgPrefix 自带尾空格 → 前缀后双空格（HEAD 既有行为，cmd
# 单测以 Contains 子串钉住；此处逐字对齐实际输出，非本轮变更引入）。
assert_startup_failure missing-arg \
	"rotatelogs:  accepts 1 arg(s), received 0"

assert_startup_failure m1-violation \
	"rotatelogs: invalid configuration: at least one of MaxSize or RotationTime must be > 0" \
	-s 0 -t 0 "$T/x.log"

assert_startup_failure m2-violation \
	"rotatelogs: invalid configuration: Pattern contains no time placeholder while RotationTime is enabled; add a strftime placeholder (e.g. %Y-%m-%d) or disable time rotation with --rotation-time 0" \
	-t 1h "$T/y.log"

# d) mkdir 失败：消息含底层 OS 错误（darwin: read-only file system；linux: permission
#    denied），只断言前缀 + 单行 + exit 1。
"$BIN" -t 0 "/nonexistent-root-xyz/sub/app.log" < /dev/null > /dev/null 2> "$T/err-mkdir" &
p=$!
wait_with_timeout "$p" 10; st=$ST
[ "$st" = "1" ] || fail "smoke-7(mkdir): exit=$st, want 1"
lines=$(wc -l < "$T/err-mkdir" | tr -d ' ')
[ "$lines" = "1" ] || fail "smoke-7(mkdir): stderr 应恰一行: $(cat "$T/err-mkdir")"
case "$(cat "$T/err-mkdir")" in
"rotatelogs: mkdir /nonexistent-root-xyz/sub: "*) ;;
*) fail "smoke-7(mkdir): stderr 前缀不符: $(cat "$T/err-mkdir")" ;;
esac
echo "  ok: mkdir-failure → exit 1, stderr=[$(cat "$T/err-mkdir")]"

pass "smoke-7 启动失败路径（缺参/M1/M2/mkdir 全部 exit 1 + 恰一行 stderr）"

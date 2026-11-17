package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shengyanli1982/rotatelogs/internal/rotator"
	"github.com/stretchr/testify/require"
)

// requireStderrLines 断言三段式输出契约（裁定 4）：stderr 非静默且恰好 n 行，
// 每个错误路径无双输出、无吞没。
func requireStderrLines(t *testing.T, output string, n int) {
	t.Helper()
	trimmed := strings.TrimSuffix(output, "\n")
	require.NotEmpty(t, trimmed, "stderr must not be silent on error paths")
	lines := strings.Split(trimmed, "\n")
	require.Lenf(t, lines, n, "stderr must contain exactly %d line(s), got: %q", n, output)
}

// TestExitErrorMapping 验证退出码映射（§3.5、裁定 3）：exitError 携带自身
// code；裸 ErrInvalidConfig → exitStartup；未知裸错误防御性回退 exitStartup。
func TestExitErrorMapping(t *testing.T) {
	e := &exitError{code: exitRuntime, err: errBoom}
	require.Equal(t, "boom", e.Error())
	require.ErrorIs(t, e, errBoom, "Unwrap must expose the underlying error")
	require.Equal(t, exitRuntime, exitCodeOf(e))

	wrapped := fmt.Errorf("%w: MaxSize", rotator.ErrInvalidConfig)
	require.Equal(t, exitStartup, exitCodeOf(wrapped))

	require.Equal(t, exitStartup, exitCodeOf(errBoom))
}

// TestRootCommandCopy 钉住裁定 1 的文案契约（调研备选 B + kubectl conventions）：
// Use/Short 逐字、DisableFlagsInUseLine、Long 首句逐字重复 Short（R5）、
// 运维契约段不可缺失、Example 无 $ 前缀（R9）且覆盖 §3.4 三条精简命令
// （5292ba5 精简裁定：示例从五条裁至三条）。
// UseLine 断言同时关闭调研报告 §7 待验证项 1（cobra v1.10.2 实测）。
func TestRootCommandCopy(t *testing.T) {
	a := &app{stdin: strings.NewReader(""), stderr: io.Discard}
	c := newRootCmd(a)

	require.Equal(t, "rotatelogs [flags] <filename-pattern>", c.Use)
	require.True(t, c.DisableFlagsInUseLine)
	require.Equal(t, c.Use, c.UseLine(),
		"DisableFlagsInUseLine must prevent cobra from re-appending \" [flags]\"")

	require.Equal(t, "Rotate piped log data by time and size", c.Short)
	require.LessOrEqual(t, len(c.Short), 50, "R4: Short ≤ 50 chars")
	require.False(t, strings.HasSuffix(c.Short, "."), "R2: Short must not end with a period")

	require.True(t, strings.HasPrefix(c.Long, c.Short+"."),
		"R5: Long must repeat Short verbatim as its first sentence")
	// 运维契约段（报告裁定：不可裁剪）。断言文案为 5292ba5 精简版
	// （streamline CLI help text），提交时漏改本测试——此处同步钉住精简后规格。
	require.Contains(t, c.Long, "Rotation triggers:")
	require.Contains(t, c.Long, "SIGHUP received (Unix only)")
	require.Contains(t, c.Long, "Exit codes: 0 success; 1 startup failure; 2 runtime I/O error.")

	require.True(t, c.SilenceUsage, "三段式：cobra usage 输出必须静默")
	require.True(t, c.SilenceErrors, "三段式：cobra 错误输出必须静默")

	require.NotContains(t, c.Example, "$ ", "R9: examples must be copy-pastable (no $ prefix)")
	require.Equal(t, 3, strings.Count(c.Example, "| rotatelogs"),
		"R10: Example must cover the three streamlined §3.4 cases")
	require.Contains(t, c.Example, "# Daily rotation with 100 MB size cap")
}

// TestExecuteSuccessOnEOF 验证黄金路径（§4.7、§14.1-10 实现面）：
// stdin EOF → 优雅关停 → exit 0，stderr 零输出，文件字节一致。
func TestExecuteSuccessOnEOF(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "app-%Y%m%d.log")
	const input = "hello rotatelogs\n"

	var stderr bytes.Buffer
	code := execute(strings.NewReader(input), &stderr, []string{pattern})

	require.Equal(t, 0, code)
	require.Empty(t, stderr.String(), "success path must not write to stderr")

	name := filepath.Join(dir, time.Now().UTC().Format("app-20060102.log"))
	content, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, input, string(content))
}

// TestExecuteRejectsWrongArgCount 验证 ExactArgs(1) 的 cobra 层错误路径
// （裁定 4 第三段）：缺 pattern 与多 pattern 均 exit 1、stderr 恰一行。
func TestExecuteRejectsWrongArgCount(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing pattern", nil, "accepts 1 arg(s), received 0"},
		{"extra pattern", []string{"p1", "p2"}, "accepts 1 arg(s), received 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := execute(strings.NewReader(""), &stderr, tt.args)
			require.Equal(t, exitStartup, code)
			requireStderrLines(t, stderr.String(), 1)
			require.Contains(t, stderr.String(), tt.want)
		})
	}
}

// TestExecuteRejectsUnknownFlag 验证未知 flag（cobra 层错误）：
// exit 1、stderr 恰一行、无 usage 倾泻（SilenceUsage 生效）。
func TestExecuteRejectsUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := execute(strings.NewReader(""), &stderr, []string{"--bogus", "pattern"})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), "unknown flag: --bogus")
}

// TestExecuteRejectsInvalidDuration 验证非法 duration 值（pflag 解析错误）：
// exit 1、stderr 恰一行。
func TestExecuteRejectsInvalidDuration(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "app.log")
	code := execute(strings.NewReader(""), &stderr, []string{"-t", "notaduration", pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), `invalid argument "notaduration"`)
}

// TestExecuteInvalidConfigExitsStartup 验证配置校验失败映射（裁定 3 + addendum：
// cmd 层不重复校验，仅映射）：M1 违规（-s 0 且 -t 0）→ rotator.New 返回
// ErrInvalidConfig → exit 1、stderr 恰一行且自带 "rotatelogs: " 前缀（无双前缀）。
func TestExecuteInvalidConfigExitsStartup(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "app.log")
	code := execute(strings.NewReader("data\n"), &stderr, []string{"-s", "0", "-t", "0", pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	line := stderr.String()
	require.Contains(t, line, "rotatelogs: invalid configuration")
	require.NotContains(t, line, "rotatelogs: rotatelogs:", "prefix must appear exactly once")
	require.Contains(t, line, "MaxSize", "validator detail must name the offending field")
}

// TestExecuteSymlinkCollisionExitsStartup 钉住 M3a 的 execute 级契约
// （GAP-P2-05，addendum-flag-validation:30 "M1/M2/M3a/V1 各有启动校验
// 单测或冒烟断言：错误恰一行 stderr + exit code 1"）：Symlink 与初始
// 日志文件绝对化等值 → exit 1、恰一行、消息含 "Symlink"。
func TestExecuteSymlinkCollisionExitsStartup(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "app.log")
	// -t 0：无占位符 pattern 合法（M2 仅在 RotationTime>0 时检查）；
	// -l 与 pattern 相同 → M3a 绝对化等值拒绝（symlink 自我覆盖）。
	code := execute(strings.NewReader("x"), &stderr, []string{"-t", "0", "-l", pattern, pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), "rotatelogs: invalid configuration")
	require.Contains(t, stderr.String(), "Symlink")
}

// TestExecuteNegativeMaxBackupsExitsStartup 钉住 V1 的 execute 级契约
// （GAP-P2-05）：字段负值 → gte=0 校验失败 → exit 1、恰一行、消息含字段名。
func TestExecuteNegativeMaxBackupsExitsStartup(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "app-%Y%m%d.log")
	code := execute(strings.NewReader("x"), &stderr, []string{"-b", "-1", pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), "rotatelogs: invalid configuration")
	require.Contains(t, stderr.String(), "MaxBackups must be >= 0")
}

// TestExecuteGlobMetaPatternExitsStartup 钉住 M5 的 execute 级契约
// （2026-11-13 审计 DA-3）：pattern 字面量含 glob 元字符 → exit 1、恰一行、
// 消息含 sentinel 与拒绝原因。归档清理按 glob 匹配文件名，元字符会导致
// 清理静默失效（欠匹配）或误删第三方文件（过度匹配），启动即拒绝。
func TestExecuteGlobMetaPatternExitsStartup(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "x*.log")
	// -t 0：排除 M2 干扰（无占位符 pattern 合法），失败必须归因于 M5 本身。
	code := execute(strings.NewReader("x"), &stderr, []string{"-t", "0", pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), "rotatelogs: invalid configuration")
	require.Contains(t, stderr.String(), "glob metacharacters")
}

// TestExecuteFileCreationFailureExitsStartup 验证文件创建失败 → exit 1（§4.8、
// 裁定 3）："不可写路径"构造——pattern 指向只读目录内的文件。
func TestExecuteFileCreationFailureExitsStartup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not restrict root; run as a normal user")
	}
	dir := t.TempDir()
	readOnly := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(readOnly, 0o500))
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	var stderr bytes.Buffer
	// -t 0：pattern 无占位符也合法（M2 仅在 RotationTime>0 时检查）。
	code := execute(strings.NewReader("x"), &stderr, []string{"-t", "0", filepath.Join(readOnly, "app.log")})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), "rotatelogs: open")
}

// TestExecuteRuntimeIOErrorExitsRuntime 验证运行期 IO 错误 → exit 2（§3.5）：
// stdin 读取失败（failingReader 注入，不 mock 文件系统）→ pump 包装错误 →
// exitError{exitRuntime} → stderr 恰一行。
func TestExecuteRuntimeIOErrorExitsRuntime(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "app-%Y%m%d.log")
	code := execute(failingReader{errBoom}, &stderr, []string{pattern})

	require.Equal(t, exitRuntime, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), "rotatelogs: read stdin: boom")
}

// TestExecuteInvalidConfigDoesNotCreateFile 验证校验闸门先于文件操作
// （Addendum 2 §3.3）：M1 违规时目录内不得残留任何日志文件。
func TestExecuteInvalidConfigDoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	code := execute(strings.NewReader("x"), &stderr, []string{"-s", "0", "-t", "0", filepath.Join(dir, "app.log")})

	require.Equal(t, exitStartup, code)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "validation must fail before any file is created")
}

// TestExecuteRejectsOversizeMaxSize 验证 --max-size 的 MB→bytes 换算溢出
// 防护（P2-6）：超过 MaxInt64/bytesPerMB 的输入此前乘法回绕为负、被
// gte=0 校验误报 "MaxSize must be >= 0"（用户传入的明明是正数，消息误导
// 排障），且存在极窄的正数回绕窗口会静默接受错误配置。修复后必须给出
// 指名 --max-size 的清晰单行错误，exit 1。
func TestExecuteRejectsOversizeMaxSize(t *testing.T) {
	var stderr bytes.Buffer
	// pattern 含时间占位符：排除 M2 干扰，失败必须归因于溢出防护本身。
	pattern := filepath.Join(t.TempDir(), "app-%Y%m%d.log")
	code := execute(strings.NewReader("x"), &stderr, []string{"-s", "9999999999999999", pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	require.Contains(t, stderr.String(), "--max-size")
}

// TestErrorsIsOnExitErrorChain 保证 errors.Is 可穿透 exitError 判别底层
// sentinel（裁定 3 的 errors.Is(err, rotator.ErrInvalidConfig) 通路）。
func TestErrorsIsOnExitErrorChain(t *testing.T) {
	err := &exitError{code: exitStartup, err: fmt.Errorf("%w: Pattern", rotator.ErrInvalidConfig)}
	require.True(t, errors.Is(err, rotator.ErrInvalidConfig))
}

// TestExecuteImplicitDowngradeToSizeOnly 钉住 M2 意图区分降级的隐式路径
// （2026-11-16 行为变更，方案 B）：pattern 无占位符 + 未传 -t（隐式默认
// 24h）+ -s 默认 100 > 0 → 降级为纯大小切分：exit 0、stderr 恰一行警告
// （含前缀、pattern 原文、降级语义说明）。exit 0 即证明 RotationTime 已
// 归零——若未降级，rotator.New 的 M2 闸门会以 exit 1 拒绝（默认 24h>0 且
// 无占位符）；数据完整写入进一步钉住 size-only 会话全程可运行。
func TestExecuteImplicitDowngradeToSizeOnly(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "app.log")
	const input = "downgraded data\n"

	var stderr bytes.Buffer
	code := execute(strings.NewReader(input), &stderr, []string{pattern})

	require.Equal(t, 0, code)
	requireStderrLines(t, stderr.String(), 1)
	line := stderr.String()
	require.Contains(t, line, rotator.MsgPrefix, "warning must carry the message prefix")
	require.NotContains(t, line, "rotatelogs: rotatelogs:", "prefix must appear exactly once")
	require.Contains(t, line, pattern, "warning must quote the pattern verbatim")
	require.Contains(t, line, "no time placeholder")
	require.Contains(t, line, "size", "warning must state the size-only downgrade")

	content, err := os.ReadFile(pattern)
	require.NoError(t, err)
	require.Equal(t, input, string(content), "size-only session must deliver every byte")
}

// TestExecuteExplicitRotationTimeKeepsM2Error 钉住显式路径（criteria 4）：
// 显式传 -t 1h + pattern 无占位符 → M2 硬报错语义保持：exit 1、恰一行、
// 错误文案追加修复提示（占位符示例与 --rotation-time 0 关闭方式）。
// fail-fast 只惩罚显式矛盾，隐式默认矛盾才降级（方案 B 的判别轴）。
func TestExecuteExplicitRotationTimeKeepsM2Error(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "app.log")
	code := execute(strings.NewReader("x"), &stderr, []string{"-t", "1h", pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	line := stderr.String()
	require.Contains(t, line, "rotatelogs: invalid configuration")
	require.NotContains(t, line, "rotatelogs: rotatelogs:", "prefix must appear exactly once")
	require.Contains(t, line, "no time placeholder")
	require.Contains(t, line, "add a strftime placeholder")
	require.Contains(t, line, "--rotation-time 0")
}

// TestExecuteExplicitZeroRotationTimeNoWarning 钉住 -t 0 显式禁用路径
// （criteria 5）：-t 0 + pattern 无占位符 → 时间切分本来就被显式关闭，
// 既不触发 M2 也无降级警告（降级语义仅针对"隐式默认造成的矛盾"）。
func TestExecuteExplicitZeroRotationTimeNoWarning(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "app.log")
	const input = "size only\n"

	var stderr bytes.Buffer
	code := execute(strings.NewReader(input), &stderr, []string{"-t", "0", pattern})

	require.Equal(t, 0, code)
	require.Empty(t, stderr.String(), "explicit -t 0 must not warn")
	content, err := os.ReadFile(pattern)
	require.NoError(t, err)
	require.Equal(t, input, string(content))
}

// TestExecutePlaceholderPatternNoDowngrade 钉住回归路径（criteria 6）：
// pattern 含占位符 + 未传 -t → 行为与降级变更前完全一致：默认 24h 时间
// 切分正常生效、stderr 零输出（无降级警告误伤）。
func TestExecutePlaceholderPatternNoDowngrade(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "app-%Y%m%d.log")
	const input = "time based\n"

	var stderr bytes.Buffer
	code := execute(strings.NewReader(input), &stderr, []string{pattern})

	require.Equal(t, 0, code)
	require.Empty(t, stderr.String(), "placeholder pattern must not warn")
	name := filepath.Join(dir, time.Now().UTC().Format("app-20060102.log"))
	content, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, input, string(content))
}

// TestExecuteZeroMaxSizeKeepsM2Error 钉住降级排除路径（criteria 7）：
// -s 0 + pattern 无占位符 + 未传 -t → 不得降级（降级会把 -s 0 变成
// M1 双零非法），保持 M2 报错现状；stderr 恰一行错误、禁止警告+错误两行。
func TestExecuteZeroMaxSizeKeepsM2Error(t *testing.T) {
	var stderr bytes.Buffer
	pattern := filepath.Join(t.TempDir(), "app.log")
	code := execute(strings.NewReader("x"), &stderr, []string{"-s", "0", pattern})

	require.Equal(t, exitStartup, code)
	requireStderrLines(t, stderr.String(), 1)
	line := stderr.String()
	require.Contains(t, line, "rotatelogs: invalid configuration")
	require.Contains(t, line, "no time placeholder")
	require.NotContains(t, line, "size", "downgrade warning must not precede the M2 error")
}

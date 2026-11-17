// Package cmd 实现 rotatelogs 的命令行层：flags 解析、跨平台信号接线、
// stdin 泵送主循环与退出码映射（DESIGN §3、§6.3、§7.2）。
package cmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/shengyanli1982/rotatelogs/internal/rotator"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	// readBufferSize 是 stdin 读缓冲大小（DESIGN §2.1，Addendum 2 §2）。
	readBufferSize = 4096
	// bytesPerMB 是 --max-size 的 MB→bytes 换算系数（Addendum 2 §2）。
	bytesPerMB = 1024 * 1024
)

// 进程退出码（DESIGN §3.5）；正常退出使用字面量 0（Addendum 2 §2）。
const (
	// exitStartup 表示启动失败：参数错误、配置校验失败、无法创建日志文件。
	exitStartup = 1
	// exitRuntime 表示运行时 IO 错误：磁盘满、权限问题、stdin 读取失败等。
	exitRuntime = 2
)

// rootOptions 持有绑定到 root 命令 flags 的原始值（§3.2 表全部 8 项）。
type rootOptions struct {
	maxSizeMB    uint
	rotationTime time.Duration
	maxBackups   int
	maxAge       int
	compress     bool
	symlink      string
	verbose      bool
	localTime    bool
}

// registerFlags 将 §3.2 的 8 个 flags 绑定到 fs：名称/短写/类型/默认值与
// 设计表逐项一致；usage 文案遵循 kubectl conventions（大写开头、完整句加
// 句号、含 0 值语义；默认值数字由 pflag 自动追加展示）。
func (o *rootOptions) registerFlags(fs *pflag.FlagSet) {
	fs.UintVarP(&o.maxSizeMB, "max-size", "s", 100,
		"Maximum size in MB of a single log file; 0 disables the size limit.")
	fs.DurationVarP(&o.rotationTime, "rotation-time", "t", 24*time.Hour,
		"Time-based rotation period; 0 disables time-based rotation.")
	fs.IntVarP(&o.maxBackups, "max-backups", "b", 0,
		"Maximum number of rotated log files to retain; 0 retains all files.")
	fs.IntVarP(&o.maxAge, "max-age", "a", 0,
		"Maximum number of days to retain rotated log files; 0 retains files of any age.")
	fs.BoolVarP(&o.compress, "compress", "z", false,
		"Compress rotated log files asynchronously with gzip.")
	fs.StringVarP(&o.symlink, "symlink", "l", "",
		"Maintain a symbolic link at this path pointing to the current log file.")
	fs.BoolVarP(&o.verbose, "verbose", "v", false,
		"Print rotation events to stderr.")
	fs.BoolVar(&o.localTime, "local-time", false,
		"Use local time instead of the default UTC for file names and rotation boundaries.")
}

// buildConfig 将解析后的 flag 值与位置参数 pattern 映射为 rotator.Config：
// MaxSize 经 bytesPerMB 由 MB 换算为 bytes，Now 保持 nil（生产时钟）。
// 刻意不做任何校验——rotator.New 是单一校验闸门（Addendum 2 §3.3，
// M1/M2/M3a/V1 全部由 rotator 层判定，cmd 层只负责错误→退出码映射）。
func (o *rootOptions) buildConfig(pattern string) rotator.Config {
	return rotator.Config{
		Pattern:      pattern,
		MaxSize:      int64(o.maxSizeMB) * bytesPerMB,
		RotationTime: o.rotationTime,
		MaxBackups:   o.maxBackups,
		MaxAge:       o.maxAge,
		Compress:     o.compress,
		Symlink:      o.symlink,
		Verbose:      o.verbose,
		LocalTime:    o.localTime,
	}
}

// pump 执行 §6.3 主读取循环：从 stdin 读一块写一块，直到 EOF 或 ctx 取消。
// D7 语义：ctx 检查是每轮 Read 之前的非阻塞 select-default——阻塞中的 Read
// 不响应 cancel（规范形态，禁止为此起 goroutine）。写入错误原样传播
// （rotator 已带 "rotatelogs: " 前缀）；EOF 经 errors.Is 判别（wrapped EOF
// 同样优雅结束，不依赖 bufio 返回裸 sentinel 的内部语义）；其余读取错误
// 包装为单行前缀错误。
func pump(ctx context.Context, stdin io.Reader, w io.Writer) error {
	// 直读不变量（OBS-N1）：buf 长度必须 ≥ bufio reader 的 bufsize（二者同为
	// readBufferSize），使每次 reader.Read(buf) 恒走 bufio 直读路径、其内部缓冲
	// 恒空；否则 bufio 转为内部缓冲模式后，ctx 取消时（D7 豁免语义，非阻塞
	// select-default 不即时响应）已从 stdin 消费但尚未写出的缓冲字节会被静默丢弃。
	// 改动任一侧尺寸或移除 bufio 包装前必须保持该不变量。
	reader := bufio.NewReaderSize(stdin, readBufferSize)
	buf := make([]byte, readBufferSize)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		n, err := reader.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf(rotator.MsgPrefix+"read stdin: %w", err)
		}
	}
}

// root 命令文案：Short 单行祈使无句号；Long 首句重复 Short，退出码为运维
// 契约不可裁剪；Example 每条命令前 # 注释、无 $ 前缀、2 空格缩进。
const (
	rootShort = `Rotate piped log data by time and size`

	rootLong = `Rotate piped log data by time and size.

Reads stdin and writes to log files that rotate by time and/or size.
The <filename-pattern> uses strftime placeholders (%Y, %m, %d, %H,
%M, %S); missing parent directories are created automatically.

Rotation triggers:
  --rotation-time elapses (default 24h; 0 disables)
  --max-size exceeded (default 100 MB; 0 disables)
  SIGHUP received (Unix only)

A pattern without time placeholders plus the implicit default
--rotation-time runs size-only with a one-line warning; an
explicitly passed --rotation-time with such a pattern is a
startup error.

Additional features: gzip compression (--compress), retention by
count (--max-backups) or age (--max-age), stable symlink (--symlink).
SIGTERM/SIGINT/EOF exit gracefully after flushing buffered data.
UTC by default; use --local-time for local time.

Buffered data is flushed to disk within 2s of idleness (300ms heartbeat),
so tail -f sees writes in near real time; bursts are batched automatically.

Exit codes: 0 success; 1 startup failure; 2 runtime I/O error.

Author: shengyanli1982
GitHub: https://github.com/shengyanli1982`

	rootExample = `  # Daily rotation with 100 MB size cap
  myapp 2>&1 | rotatelogs '/var/log/myapp/app-%Y-%m-%d.log'

  # Size-only: 50 MB files, no time rotation
  myapp 2>&1 | rotatelogs -t 0 -s 50 '/var/log/myapp/app.log'

  # Full setup: 12h rotation, compress, keep 30 backups / 7 days
  myapp 2>&1 | rotatelogs -s 200 -t 12h -b 30 -a 7 -zv \
    --symlink /var/log/myapp/current.log --local-time \
    '/var/log/myapp/app-%Y-%m-%d.log'`
)

// exitError 将底层错误与进程退出码（§3.5）绑定，供 RunE 在恰一行 stderr
// 输出后以 exitCodeOf 记录退出码（三段式第二段）。
type exitError struct {
	code int
	err  error
}

// Error 实现 error 接口：原样输出底层错误消息。rotator 层错误已自带
// "rotatelogs: " 前缀（Addendum 2 §1），此处禁止再叠加前缀。
func (e *exitError) Error() string { return e.err.Error() }

// Unwrap 暴露底层错误，保证 errors.Is/As 可穿透判别 sentinel。
func (e *exitError) Unwrap() error { return e.err }

// exitCodeOf 将应用错误映射为进程退出码（§3.5、裁定 3）：exitError 携带
// 自身 code；ErrInvalidConfig 属启动失败；未知裸错误防御性回退 exitStartup。
func exitCodeOf(err error) int {
	var ee *exitError
	switch {
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, rotator.ErrInvalidConfig):
		return exitStartup
	default:
		return exitStartup
	}
}

// app 承载 root 命令的运行时依赖：stdin/stderr 可注入，使 execute 成为
// 不触碰 os.Exit 的可测核心；exitCode 由 RunE 记录应用层错误的映射结果。
type app struct {
	opts     rootOptions
	stdin    io.Reader
	stderr   io.Writer
	exitCode int
}

// run 执行一次完整的轮转会话，逐行对应 §6.3 主循环伪代码：
// New → defer Close → setupSignals → pump。全部失败以 *exitError 携带
// §3.5 退出码返回：New 失败（配置校验/文件创建）→ exitStartup；pump 运行期
// 错误与 Close 时的 flush/close 失败 → exitRuntime（数据丢失不得静默）。
func (a *app) run(pattern string) (err error) {
	// --max-size 的 MB→bytes 换算溢出防护（配置闸门职责，P2-6）：回绕值会被
	// gte=0 误报为 "MaxSize must be >= 0" 或（正数回绕窗口）被静默接受。
	if a.opts.maxSizeMB > math.MaxInt64/bytesPerMB {
		return &exitError{code: exitStartup, err: fmt.Errorf(
			rotator.MsgPrefix+"--max-size %d MB exceeds the maximum convertible value of %d MB",
			a.opts.maxSizeMB, math.MaxInt64/bytesPerMB)}
	}
	r, cerr := rotator.New(a.opts.buildConfig(pattern))
	if cerr != nil {
		return &exitError{code: exitStartup, err: cerr}
	}
	defer func() {
		if clerr := r.Close(); clerr != nil && err == nil {
			err = &exitError{code: exitRuntime, err: clerr}
		}
	}()

	ctx, stop := setupSignals(&r.RotateFlag)
	defer stop()

	if perr := pump(ctx, a.stdin, r); perr != nil {
		return &exitError{code: exitRuntime, err: perr}
	}
	return nil
}

// downgradeImplicitRotationTime 执行 --rotation-time 的隐式默认意图降级
// （2026-11-16 行为变更，方案 B）：用户未显式传 -t（pflag 的 Changed 对
// 短写 -t 与长写 --rotation-time 均置位）而隐式默认 24h 与"pattern 无
// 时间占位符"矛盾、且 --max-size > 0（降级后仍有切分条件，不落入 M1
// 双零非法）时，把 RotationTime 归零并输出恰一行降级警告。显式传 -t
// 保持矛盾，交由 rotator.New 的 M2 闸门硬报错——fail-fast 只惩罚显式
// 矛盾，隐式默认值伪造的矛盾降级为 size-only。
// 注意：本函数是 flag 意图预处理而非校验——"校验入口唯一在
// rotator.New()"（Addendum 2 §3.3）的铁律不变，此处仅消解隐式默认与
// 用户真实意图的矛盾，让后续闸门看到与显式表达一致的配置。
// 警告不受 --verbose 门控（与运行期 rotation 事件的 verbose 语义不同），
// 必须让用户无条件下意识到"时间切分被静默关闭了"。
func (a *app) downgradeImplicitRotationTime(fs *pflag.FlagSet, pattern string) {
	if fs.Changed("rotation-time") {
		return
	}
	if a.opts.rotationTime <= 0 || rotator.HasTimePlaceholder(pattern) || a.opts.maxSizeMB == 0 {
		return
	}
	a.opts.rotationTime = 0
	fmt.Fprintf(a.stderr, rotator.MsgPrefix+"pattern '%s' contains no time placeholder; time-based rotation disabled (size-based only)\n", pattern)
}

// newRootCmd 构造 root 命令（单一命令、无子命令树，§3.1；禁止 viper）。
// 输出通道三段式（裁定 4）：SilenceUsage+SilenceErrors 令 cobra 全静默，
// 应用层错误由 RunE 恰一行输出至 stderr、退出码记入 app.exitCode 后返回 nil。
func newRootCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "rotatelogs [flags] <filename-pattern>",
		Short:                 rootShort,
		Long:                  rootLong,
		Example:               rootExample,
		Args:                  cobra.ExactArgs(1),
		DisableFlagsInUseLine: true,
		SilenceUsage:          true,
		SilenceErrors:         true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 意图预处理先于 run：降级警告必须在 New()（校验闸门）之前
			// 输出，用户先看到"已降级"再看到可能的后续错误。
			a.downgradeImplicitRotationTime(cmd.Flags(), args[0])
			if err := a.run(args[0]); err != nil {
				fmt.Fprintln(a.stderr, err)
				a.exitCode = exitCodeOf(err)
			}
			return nil
		},
	}
	a.opts.registerFlags(cmd.Flags())
	return cmd
}

// Execute 运行 root 命令并以映射后的退出码结束进程（§3.5）。
// main.go 唯一调用入口；os.Exit 封装于本函数内。
func Execute() {
	os.Exit(execute(os.Stdin, os.Stderr, os.Args[1:]))
}

// execute 是 Execute 的可测核心：以给定流与参数运行 root 命令，返回进程
// 退出码而不调用 os.Exit。三段式第三段：只有 cobra 层错误（未知 flag、
// 参数个数错、flag 值解析失败）会到达此处，补唯一一次带前缀的 stderr 输出
// 后按 exitStartup 退出；应用层错误已在 RunE 内完成输出与记码。
func execute(stdin io.Reader, stderr io.Writer, args []string) int {
	if args == nil {
		// cobra ExecuteC 在 c.args==nil 时回退 os.Args[1:]（command.go 的
		// "go test -v" workaround），测试与空参场景必须归一为空切片。
		args = []string{}
	}
	a := &app{stdin: stdin, stderr: stderr}
	cmd := newRootCmd(a)
	cmd.SetArgs(args)
	cmd.SetErr(stderr)

	if err := cmd.Execute(); err != nil {
		// flag.ErrHelp 通常已被 cobra ExecuteC 内部消化（打印 help 后返回
		// nil）；按裁定 4 仍防御性排除，保证错误输出通道唯一。
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, rotator.MsgPrefix, err)
		}
		return exitStartup
	}
	return a.exitCode
}

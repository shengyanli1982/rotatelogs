package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/shengyanli1982/rotatelogs/internal/rotator"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// errBoom 是测试注入的哨兵错误（写入/读取失败路径）。
var errBoom = errors.New("boom")

// failingWriter 让每次 Write 都失败，用于 pump 写入错误传播测试。
type failingWriter struct{ err error }

func (f failingWriter) Write(p []byte) (int, error) { return 0, f.err }

// failingReader 让每次 Read 都返回非 EOF 错误，用于 pump 读取错误包装测试。
type failingReader struct{ err error }

func (f failingReader) Read(p []byte) (int, error) { return 0, f.err }

// cancelAfterReader 交付第一块数据后立即 cancel；rest 块不应再被 pump 读取
// （D7：ctx 检查发生在每轮 Read 之前，非阻塞 select-default 形态）。
type cancelAfterReader struct {
	first  []byte
	rest   []byte
	sent   bool
	cancel context.CancelFunc
}

func (c *cancelAfterReader) Read(p []byte) (int, error) {
	if c.sent {
		return copy(p, c.rest), io.EOF
	}
	c.sent = true
	n := copy(p, c.first)
	c.cancel()
	return n, nil
}

// newTestFlagSet 构造仅含业务 flags 的独立 FlagSet（不含 cobra help flag）。
func newTestFlagSet(o *rootOptions) *pflag.FlagSet {
	fs := pflag.NewFlagSet("rotatelogs", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o.registerFlags(fs)
	return fs
}

// TestFlagDefinitions 逐项核对 §3.2 flags 表：名称/短写/类型/默认值，
// 并机器化校验 usage 文案规范（裁定 2：大写开头、完整句加句号）。
func TestFlagDefinitions(t *testing.T) {
	expected := []struct {
		name      string
		shorthand string
		typ       string
		defValue  string
	}{
		{"max-size", "s", "uint", "100"},
		{"rotation-time", "t", "duration", "24h0m0s"},
		{"max-backups", "b", "int", "0"},
		{"max-age", "a", "int", "0"},
		{"compress", "z", "bool", "false"},
		{"symlink", "l", "string", ""},
		{"verbose", "v", "bool", "false"},
		{"local-time", "", "bool", "false"},
	}

	var o rootOptions
	fs := newTestFlagSet(&o)

	count := 0
	fs.VisitAll(func(*pflag.Flag) { count++ })
	require.Len(t, expected, count, "exactly the 8 flags of §3.2 must be registered")

	for _, e := range expected {
		f := fs.Lookup(e.name)
		require.NotNilf(t, f, "flag --%s must exist", e.name)
		require.Equalf(t, e.shorthand, f.Shorthand, "flag --%s shorthand", e.name)
		require.Equalf(t, e.typ, f.Value.Type(), "flag --%s type", e.name)
		require.Equalf(t, e.defValue, f.DefValue, "flag --%s default value", e.name)

		require.NotEmptyf(t, f.Usage, "flag --%s usage must not be empty", e.name)
		first := rune(f.Usage[0])
		require.Truef(t, unicode.IsUpper(first),
			"flag --%s usage must start uppercase (kubectl conventions), got %q", e.name, f.Usage)
		require.Truef(t, strings.HasSuffix(f.Usage, "."),
			"flag --%s usage must end with a period, got %q", e.name, f.Usage)
	}
}

// TestBuildConfigDefaults 验证默认 flags → Config 映射（runbook Step 4.2）。
func TestBuildConfigDefaults(t *testing.T) {
	var o rootOptions
	fs := newTestFlagSet(&o)
	require.NoError(t, fs.Parse(nil))

	const pattern = "/var/log/app/app-%Y-%m-%d.log"
	cfg := o.buildConfig(pattern)

	require.Equal(t, pattern, cfg.Pattern)
	require.EqualValues(t, 100*bytesPerMB, cfg.MaxSize)
	require.Equal(t, 24*time.Hour, cfg.RotationTime)
	require.Zero(t, cfg.MaxBackups)
	require.Zero(t, cfg.MaxAge)
	require.False(t, cfg.Compress)
	require.Empty(t, cfg.Symlink)
	require.False(t, cfg.Verbose)
	require.False(t, cfg.LocalTime)
	require.Nil(t, cfg.Now, "production config must not inject a test clock")
}

// TestBuildConfigAllFlags 验证全部 flags 显式设置后的映射与 MB→bytes 精确换算。
func TestBuildConfigAllFlags(t *testing.T) {
	var o rootOptions
	fs := newTestFlagSet(&o)
	require.NoError(t, fs.Parse([]string{
		"-s", "200",
		"-t", "12h",
		"-b", "30",
		"-a", "7",
		"-z",
		"-l", "/var/log/app/current.log",
		"-v",
		"--local-time",
	}))

	cfg := o.buildConfig("app-%Y%m%d.log")

	require.EqualValues(t, int64(200)*1024*1024, cfg.MaxSize, "MB→bytes conversion must be exact")
	require.Equal(t, 12*time.Hour, cfg.RotationTime)
	require.Equal(t, 30, cfg.MaxBackups)
	require.Equal(t, 7, cfg.MaxAge)
	require.True(t, cfg.Compress)
	require.Equal(t, "/var/log/app/current.log", cfg.Symlink)
	require.True(t, cfg.Verbose)
	require.True(t, cfg.LocalTime)
}

// TestPumpDeliversAllBytesAndStopsAtEOF 验证数据透传字节一致（跨多轮 Read，
// 10KiB > readBufferSize）且 EOF 正常返回 nil（§4.1 原封不动、§4.7）。
func TestPumpDeliversAllBytesAndStopsAtEOF(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef\n"), 640) // 10240 bytes
	require.Greater(t, len(data), readBufferSize, "payload must span multiple reads")

	var got bytes.Buffer
	require.NoError(t, pump(context.Background(), bytes.NewReader(data), &got))
	require.Equal(t, data, got.Bytes(), "pump must deliver every byte unmodified and in order")
}

// TestPumpWritesThroughRealRotator 用真实 rotator（TempDir，不 mock 文件系统）
// 验证 pump → Rotator.Write → 文件内容端到端一致（runbook Step 4.2 用例）。
func TestPumpWritesThroughRealRotator(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "app-%Y%m%d.log")
	r, err := rotator.New(rotator.Config{Pattern: pattern, MaxSize: bytesPerMB})
	require.NoError(t, err)

	data := bytes.Repeat([]byte("line\n"), 2000) // 10000 bytes
	require.NoError(t, pump(context.Background(), bytes.NewReader(data), r))
	require.NoError(t, r.Close())

	name := filepath.Join(dir, time.Now().UTC().Format("app-20060102.log"))
	content, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, data, content)
}

// TestPumpPropagatesWriteError 验证写入错误原样向上传播（不吞、不重试）。
func TestPumpPropagatesWriteError(t *testing.T) {
	err := pump(context.Background(), strings.NewReader("data"), failingWriter{errBoom})
	require.ErrorIs(t, err, errBoom)
}

// TestPumpWrapsReadError 验证非 EOF 读取错误被包装为带前缀的单行错误，
// 且底层错误可经 errors.Is 判别（运行期 → exit 2 的映射入口）。
func TestPumpWrapsReadError(t *testing.T) {
	var got bytes.Buffer
	err := pump(context.Background(), failingReader{errBoom}, &got)
	require.ErrorIs(t, err, errBoom)
	require.ErrorContains(t, err, "rotatelogs: read stdin")
	require.Empty(t, got.Bytes())
}

// wrappedEOFReader 交付一块数据后返回包装过的 io.EOF（P2-2：pump 签名接受
// 任意 io.Reader，注入层返回 wrapped EOF 时不得误判为运行时错误）。
type wrappedEOFReader struct{ delivered bool }

func (r *wrappedEOFReader) Read(p []byte) (int, error) {
	if r.delivered {
		return 0, fmt.Errorf("upstream: %w", io.EOF)
	}
	r.delivered = true
	return copy(p, "data"), nil
}

// TestPumpTreatsWrappedEOFAsGracefulEnd 验证 pump 以 errors.Is 判别 EOF：
// wrapped io.EOF 同样优雅结束（返回 nil、exit 0 语义），而非包装成
// "read stdin" 运行期错误（exit 2）。
func TestPumpTreatsWrappedEOFAsGracefulEnd(t *testing.T) {
	var got bytes.Buffer
	err := pump(context.Background(), &wrappedEOFReader{}, &got)
	require.NoError(t, err, "wrapped io.EOF must end the pump gracefully")
	require.Equal(t, "data", got.String())
}

// TestPumpReturnsNilOnContextCancel 验证已 cancel 的 ctx → 立即返回 nil 不写入。
func TestPumpReturnsNilOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var got bytes.Buffer
	require.NoError(t, pump(ctx, strings.NewReader("data"), &got))
	require.Empty(t, got.Bytes())
}

// TestPumpStopsOnCancelBetweenReads 验证两轮 Read 之间的 ctx 检查生效：
// 第一块交付后 cancel，第二块必须被丢弃（D7 select-default 形态的行为面）。
func TestPumpStopsOnCancelBetweenReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := &cancelAfterReader{first: []byte("first"), rest: []byte("MUST-NOT-APPEAR"), cancel: cancel}
	var got bytes.Buffer
	require.NoError(t, pump(ctx, r, &got))
	require.Equal(t, []byte("first"), got.Bytes())
}

// TestPumpEmptyInput 验证空输入（立即 EOF）→ 返回 nil、零写入。
func TestPumpEmptyInput(t *testing.T) {
	var got bytes.Buffer
	require.NoError(t, pump(context.Background(), bytes.NewReader(nil), &got))
	require.Empty(t, got.Bytes())
}

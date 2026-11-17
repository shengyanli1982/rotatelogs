package rotator

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testMaxSize 供与大小切分语义无关的用例满足 M1 校验（Addendum 2 §3.2）：
// 足够大，任何测试写入量都不会触发大小切分。
const testMaxSize = 64 << 20

// testClock 返回可推进的测试时钟：指针用于测试内推进时间，闭包注入 Config.Now。
// 时间推进零 sleep（DESIGN §10.1）。
func testClock(start time.Time) (*time.Time, func() time.Time) {
	now := start
	return &now, func() time.Time { return now }
}

// readFile 读取测试产物文件，失败即终止测试。
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// TestWritePreservesBytes 验证写入字节级一致且顺序不变（§14.1-1），含二进制字节。
func TestWritePreservesBytes(t *testing.T) {
	dir := t.TempDir()
	_, clock := testClock(time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC))

	r, err := New(Config{
		Pattern: filepath.Join(dir, "app-%Y%m%d.log"),
		MaxSize: testMaxSize,
		Now:     clock,
	})
	require.NoError(t, err)

	segments := [][]byte{
		[]byte("hello "),
		{0x00, 0xFF, 0x10, '\n'},
		[]byte("world\x00binary\xff"),
	}
	var want []byte
	for _, seg := range segments {
		n, err := r.Write(seg)
		require.NoError(t, err)
		assert.Equal(t, len(seg), n)
		want = append(want, seg...)
	}
	require.NoError(t, r.Close())

	got := readFile(t, filepath.Join(dir, "app-20261113.log"))
	assert.Equal(t, want, got)
}

// TestRotateByTime 验证 Now 注入跨小时边界触发切分（§14.1-2），
// 并断言 nextRotate 对齐自然整点边界而非"上次写入+周期"（D2）。
func TestRotateByTime(t *testing.T) {
	dir := t.TempDir()
	clockPtr, clock := testClock(time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC))

	r, err := New(Config{
		Pattern:      filepath.Join(dir, "app-%Y%m%dT%H.log"),
		RotationTime: time.Hour,
		Now:          clock,
	})
	require.NoError(t, err)
	defer r.Close()

	// D2: t0=14:30 → 边界对齐 15:00，而非 15:30。
	assert.True(t,
		time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC).Equal(r.nextRotate),
		"nextRotate should align to natural hour boundary 15:00, got %v", r.nextRotate)

	_, err = r.Write([]byte("data-A"))
	require.NoError(t, err)

	// 推进越过 15:00 边界后再写入。
	*clockPtr = time.Date(2026, 11, 13, 15, 0, 30, 0, time.UTC)
	_, err = r.Write([]byte("data-B"))
	require.NoError(t, err)
	require.NoError(t, r.Close())

	files, err := filepath.Glob(filepath.Join(dir, "app-*"))
	require.NoError(t, err)
	assert.Len(t, files, 2)
	assert.Equal(t, []byte("data-A"), readFile(t, filepath.Join(dir, "app-20261113T14.log")))
	assert.Equal(t, []byte("data-B"), readFile(t, filepath.Join(dir, "app-20261113T15.log")))
}

// TestRotateBySize 照抄 DESIGN §10.3 原例（MaxSize=10 单次写 15B → 2 文件），
// 追加 D1 断言：每文件 ≤ MaxSize、跨分片拼接还原 == 原输入（§14.1-3）。
func TestRotateBySize(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

	cfg := Config{
		Pattern:      filepath.Join(dir, "app-%Y%m%d.log"),
		MaxSize:      10, // 10 bytes
		RotationTime: 0,  // 禁用时间切分
		Now:          func() time.Time { return now },
	}

	r, err := New(cfg)
	require.NoError(t, err)
	defer r.Close()

	// 写入 15 字节，应触发切分
	n, err := r.Write([]byte("123456789012345"))
	require.NoError(t, err)
	require.Equal(t, 15, n)

	// 验证产生了 2 个文件
	files, err := filepath.Glob(filepath.Join(dir, "app-*"))
	require.NoError(t, err)
	assert.Len(t, files, 2)

	require.NoError(t, r.Close())

	// 每个文件 ≤ MaxSize
	for _, f := range files {
		st, err := os.Stat(f)
		require.NoError(t, err)
		assert.LessOrEqual(t, st.Size(), int64(10))
	}
	// 分片按序拼接还原原输入
	first := filepath.Join(dir, "app-20261113.log")
	second := first + ".1"
	assert.Equal(t, []byte("1234567890"), readFile(t, first))
	assert.Equal(t, []byte("12345"), readFile(t, second))
	assert.Equal(t, "123456789012345",
		string(readFile(t, first))+string(readFile(t, second)))
}

// TestRotateBySizeMultiChunk 验证连续多次 Write 跨 MaxSize 边界时（D1）：
// 每文件 ≤ MaxSize、总量拼接还原、文件数符合分片预期。
func TestRotateBySizeMultiChunk(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "app.log")

	r, err := New(Config{
		Pattern: path,
		MaxSize: 10,
		Now:     func() time.Time { return now },
	})
	require.NoError(t, err)
	defer r.Close()

	chunk := []byte("abcdefghijklmnopqrstuvwxy") // 25 bytes
	var want []byte
	for i := 0; i < 3; i++ {
		n, err := r.Write(chunk)
		require.NoError(t, err)
		require.Equal(t, len(chunk), n)
		want = append(want, chunk...)
	}
	require.NoError(t, r.Close())

	// 75 bytes / 10 = 8 个文件（app.log + .1 ... .7）
	names := []string{path}
	for i := 1; i <= 7; i++ {
		names = append(names, fmt.Sprintf("%s.%d", path, i))
	}
	var got []byte
	for _, name := range names {
		st, err := os.Stat(name)
		require.NoError(t, err, "expected file %s", name)
		assert.LessOrEqual(t, st.Size(), int64(10))
		got = append(got, readFile(t, name)...)
	}
	assert.Equal(t, want, got)

	files, err := filepath.Glob(path + "*")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(files), 7)
}

// TestRotateByFlag 验证 RotateFlag（SIGHUP 语义）触发切分且 flag 被消费复位（§14.1-4）。
func TestRotateByFlag(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)

	r, err := New(Config{
		Pattern: filepath.Join(dir, "app.log"),
		MaxSize: testMaxSize,
		Now:     func() time.Time { return now },
	})
	require.NoError(t, err)
	defer r.Close()

	_, err = r.Write([]byte("A"))
	require.NoError(t, err)

	r.RotateFlag.Store(true) // 模拟 SIGHUP
	_, err = r.Write([]byte("B"))
	require.NoError(t, err)

	// flag 已被 rotate 消费复位
	assert.False(t, r.RotateFlag.Load())

	require.NoError(t, r.Close())

	// 同周期内切分 → 第二个文件带 .1 后缀
	assert.Equal(t, []byte("A"), readFile(t, filepath.Join(dir, "app.log")))
	assert.Equal(t, []byte("B"), readFile(t, filepath.Join(dir, "app.log.1")))
}

// TestFilenameCollision 验证同一时间周期内连续切分时文件名追加序号 .1/.2（§14.1-11）。
func TestFilenameCollision(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)

	r, err := New(Config{
		Pattern:      filepath.Join(dir, "app-%Y%m%dT%H.log"),
		RotationTime: time.Hour,
		Now:          func() time.Time { return now },
	})
	require.NoError(t, err)
	defer r.Close()

	_, err = r.Write([]byte("1"))
	require.NoError(t, err)

	r.RotateFlag.Store(true)
	_, err = r.Write([]byte("2"))
	require.NoError(t, err)

	r.RotateFlag.Store(true)
	_, err = r.Write([]byte("3"))
	require.NoError(t, err)

	require.NoError(t, r.Close())

	base := filepath.Join(dir, "app-20261113T14.log")
	assert.Equal(t, []byte("1"), readFile(t, base))
	assert.Equal(t, []byte("2"), readFile(t, base+".1"))
	assert.Equal(t, []byte("3"), readFile(t, base+".2"))
}

// TestCloseFlush 验证 <64KB 数据先驻留 bufio 缓冲区，Close 后完整落盘（§4.1/§4.7）。
func TestCloseFlush(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "app.log")

	r, err := New(Config{Pattern: path, MaxSize: testMaxSize, Now: func() time.Time { return now }})
	require.NoError(t, err)

	data := []byte("buffered-but-not-flushed")
	_, err = r.Write(data)
	require.NoError(t, err)

	// 未 Close：数据在 64KB bufio 缓冲中，磁盘文件为空
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, st.Size())

	require.NoError(t, r.Close())
	assert.Equal(t, data, readFile(t, path))
}

// TestCloseIdempotent 验证 Close 幂等：二次调用返回 nil（§6.1）。
func TestCloseIdempotent(t *testing.T) {
	dir := t.TempDir()

	r, err := New(Config{Pattern: filepath.Join(dir, "app.log"), MaxSize: testMaxSize})
	require.NoError(t, err)

	require.NoError(t, r.Close())
	assert.NoError(t, r.Close())
}

// TestWriteAfterClose 验证 Close 后 Write 返回 error 且 n==0。
func TestWriteAfterClose(t *testing.T) {
	dir := t.TempDir()

	r, err := New(Config{Pattern: filepath.Join(dir, "app.log"), MaxSize: testMaxSize})
	require.NoError(t, err)
	require.NoError(t, r.Close())

	n, err := r.Write([]byte("x"))
	assert.Error(t, err)
	assert.Zero(t, n)
}

// errWriteFailed 与 errWriter 白盒构造 Close 的 Flush 失败路径。
var errWriteFailed = errors.New("write failed")

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errWriteFailed }

// TestCloseDoubleFailureSingleLine 钉住 GAP-P2-03：flush 与 close 同时失败
// （磁盘整体故障的现实场景）时，Close 返回的错误消息必须单行——errors.Join
// 以 \n 连接会产生两行 stderr，违反"恰一行"输出契约（README、裁定 4、
// requireStderrLines）。多 %w 保证两条底层错误链仍可 errors.Is 穿透。
func TestCloseDoubleFailureSingleLine(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "app.log"))
	require.NoError(t, err)
	require.NoError(t, f.Close()) // 预先关闭：Close 内的 file.Close 必然失败

	r := &Rotator{
		path: f.Name(),
		file: f,
		buf:  bufio.NewWriterSize(errWriter{}, WriteBufferSize),
	}
	_, err = r.buf.WriteString("buffered") // 缓冲有数据 → Flush 必然失败
	require.NoError(t, err)

	cerr := r.Close()
	require.Error(t, cerr)
	assert.NotContains(t, cerr.Error(), "\n", "double failure must produce exactly one line")
	assert.Contains(t, cerr.Error(), "flush")
	assert.Contains(t, cerr.Error(), "close")
	assert.ErrorIs(t, cerr, errWriteFailed, "flush error chain must stay unwrappable")
}

// TestCloseUpdatesSymlink 钉住 GAP-P2-01（§4.7 关停序列：flush → 关闭文件 →
// 更新 symlink → exit）：Close 须 best-effort 刷新 symlink——最后一次更新
// 因瞬态失败残留错误指向时，这是自愈机会；失败仅 verbose 警告，
// 不影响 Close 返回值（§4.5 symlink 失败可容忍）。
func TestCloseUpdatesSymlink(t *testing.T) {
	skipWindowsSymlink(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "current.log")

	r, err := New(Config{
		Pattern: filepath.Join(dir, "app.log"),
		MaxSize: testMaxSize,
		Symlink: link,
		Now:     func() time.Time { return cleanupNow },
	})
	require.NoError(t, err)
	_, err = r.Write([]byte("data"))
	require.NoError(t, err)

	// 模拟瞬态失败残留：symlink 指向不存在的目标。
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink("stale-target", link))

	require.NoError(t, r.Close())

	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "app.log", got, "Close must best-effort refresh the symlink (§4.7)")
}

// TestNewRejectsNoTimePlaceholder 验证 D8（经 M2 并入 Validate，Addendum 2 §1/§3.2）：
// RotationTime>0 且 pattern 无时间占位符 → ErrInvalidConfig；
// RotationTime=0 时同 pattern 允许（固定文件名 + 序号模式，MaxSize 满足 M1）。
func TestNewRejectsNoTimePlaceholder(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "app.log")

	_, err := New(Config{Pattern: pattern, RotationTime: time.Hour})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))

	r, err := New(Config{Pattern: pattern, MaxSize: testMaxSize})
	require.NoError(t, err)
	require.NoError(t, r.Close())
}

// TestNewCreatesParentDirs 验证 pattern 多级不存在目录被自动创建（§4.2 步骤5）。
func TestNewCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	nested := filepath.Join(dir, "a", "b", "c")

	r, err := New(Config{
		Pattern: filepath.Join(nested, "app-%Y%m%d.log"),
		MaxSize: testMaxSize,
		Now:     func() time.Time { return now },
	})
	require.NoError(t, err)
	defer r.Close()

	st, err := os.Stat(nested)
	require.NoError(t, err)
	assert.True(t, st.IsDir())

	_, err = r.Write([]byte("nested"))
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, []byte("nested"), readFile(t, filepath.Join(nested, "app-20261113.log")))
}

// TestNewAppendExisting 验证 D3：初始文件已存在时追加打开（非覆盖、非产生 .1）。
func TestNewAppendExisting(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "app-20261113.log")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0644))

	r, err := New(Config{
		Pattern: filepath.Join(dir, "app-%Y%m%d.log"),
		MaxSize: testMaxSize,
		Now:     func() time.Time { return now },
	})
	require.NoError(t, err)

	_, err = r.Write([]byte("new"))
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, []byte("oldnew"), readFile(t, path))
	files, err := filepath.Glob(filepath.Join(dir, "app-*"))
	require.NoError(t, err)
	assert.Len(t, files, 1)
}

// TestNewAppendExistingCountsSize 验证 D3 的 Stat 初始化 size 与 D1 分片联动：
// 已有 8B + MaxSize=10，写 4B → 前 2B 留在原文件，后 2B 切入 .1。
func TestNewAppendExistingCountsSize(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte("12345678"), 0644))

	r, err := New(Config{
		Pattern: path,
		MaxSize: 10,
		Now:     func() time.Time { return now },
	})
	require.NoError(t, err)
	defer r.Close()

	assert.Equal(t, int64(8), r.size)

	_, err = r.Write([]byte("abcd"))
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, []byte("12345678ab"), readFile(t, path))
	assert.Equal(t, []byte("cd"), readFile(t, path+".1"))
}

// TestBoundaryAfterAlignment 验证 D2 对齐语义：24h 周期 → 午夜切分；
// 恰在边界 → 下一周期；RotationTime<=0 → 零值（禁用）。
func TestBoundaryAfterAlignment(t *testing.T) {
	r := &Rotator{cfg: Config{RotationTime: 24 * time.Hour}}
	now := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)

	assert.True(t,
		time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC).Equal(r.boundaryAfter(now)),
		"24h period should align to next midnight, got %v", r.boundaryAfter(now))

	atBoundary := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	assert.True(t,
		time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC).Equal(r.boundaryAfter(atBoundary)),
		"exactly at boundary should advance to next period, got %v", r.boundaryAfter(atBoundary))

	disabled := &Rotator{}
	assert.True(t, disabled.boundaryAfter(now).IsZero())
}

// TestBoundaryAfterRecursion36h 验证 DA-1 修复（2026-11-13 审计）：d > 24h 时
// 边界按周期递推锚定——首边界仍按 D2 公式（启动日午夜 + d）保持启动语义连续，
// 此后每一边界 = 上一计划边界 + d 严格递推。旧实现每日重新午夜锚定，
// 36h 配置在首边界后退化为 24h 稳态周期（Jan-3 12:00 而非 Jan-4 00:00）。
func TestBoundaryAfterRecursion36h(t *testing.T) {
	r := &Rotator{cfg: Config{RotationTime: 36 * time.Hour}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 首边界：D2 公式 = Jan-1 午夜 + 36h = Jan-2 12:00。
	first := r.boundaryAfter(start)
	assert.True(t,
		time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC).Equal(first),
		"first boundary must keep D2 startup semantics (midnight + 36h), got %v", first)

	// 第二边界：首边界 + 36h = Jan-4 00:00，而非重新午夜锚定的 Jan-3 12:00。
	r.nextRotate = first
	second := r.boundaryAfter(first)
	assert.True(t,
		time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC).Equal(second),
		"second boundary must be first + 36h (Jan-4 00:00), got %v", second)

	// 第三边界：继续 + 36h = Jan-5 12:00。
	r.nextRotate = second
	third := r.boundaryAfter(second)
	assert.True(t,
		time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC).Equal(third),
		"third boundary must be second + 36h (Jan-5 12:00), got %v", third)

	// 迟到切分（边界已过 6h 才发生写入）：跳过已错过的边界，落在递推序列上
	// 严格晚于 t 的最小计划边界（Jan-4 00:00）。
	r.nextRotate = first
	late := r.boundaryAfter(time.Date(2026, 1, 2, 18, 0, 0, 0, time.UTC))
	assert.True(t,
		time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC).Equal(late),
		"late rotation must land on the strict schedule (Jan-4 00:00), got %v", late)
}

// TestBoundaryAfterRecursion25h 验证 25h（>24h 非整数倍）周期递推：
// 旧实现从 t=Jan-2 01:00 重新午夜锚定给出 Jan-3 01:00（仅 +24h），
// 修复后应为 Jan-2 01:00 + 25h = Jan-3 02:00。
func TestBoundaryAfterRecursion25h(t *testing.T) {
	r := &Rotator{cfg: Config{RotationTime: 25 * time.Hour}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	first := r.boundaryAfter(start)
	assert.True(t,
		time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC).Equal(first),
		"first boundary must be midnight + 25h (Jan-2 01:00), got %v", first)

	r.nextRotate = first
	second := r.boundaryAfter(first)
	assert.True(t,
		time.Date(2026, 1, 3, 2, 0, 0, 0, time.UTC).Equal(second),
		"second boundary must be first + 25h (Jan-3 02:00), got %v", second)
}

// TestBoundaryAfter48hControl 钉住 48h（24h 整数倍）对照组：递推锚定与旧
// 午夜锚定算法结果必须一致（准点与迟到两种切分时刻），DA-1 修复不得改变
// 24h 整数倍周期的既有行为。
func TestBoundaryAfter48hControl(t *testing.T) {
	r := &Rotator{cfg: Config{RotationTime: 48 * time.Hour}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	first := r.boundaryAfter(start)
	assert.True(t,
		time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC).Equal(first),
		"first boundary must be Jan-3 00:00, got %v", first)

	// 准点切分：新旧算法一致 = Jan-5 00:00。
	r.nextRotate = first
	onTime := r.boundaryAfter(first)
	assert.True(t,
		time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).Equal(onTime),
		"on-time rotation must give Jan-5 00:00 (same as legacy algorithm), got %v", onTime)

	// 迟到切分（Jan-3 06:00）：旧算法 base=Jan-3 午夜 → Jan-5 00:00；
	// 递推算法同样必须给出 Jan-5 00:00。
	r.nextRotate = first
	late := r.boundaryAfter(time.Date(2026, 1, 3, 6, 0, 0, 0, time.UTC))
	assert.True(t,
		time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).Equal(late),
		"late rotation must give Jan-5 00:00 (same as legacy algorithm), got %v", late)
}

// TestBoundaryAfterEarlyAndExactBoundary 验证 d > 24h 递推的两个边界行为：
// 恰落边界（t == 上一计划边界）→ 递推下一周期；提前切分（size/SIGHUP 在
// t < 上一计划边界时触发）→ 保持即将到来的计划边界不漂移（严格网格语义）。
func TestBoundaryAfterEarlyAndExactBoundary(t *testing.T) {
	r := &Rotator{cfg: Config{RotationTime: 36 * time.Hour}}
	prev := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	r.nextRotate = prev

	// 恰落边界：t == prev → prev + 36h。
	exact := r.boundaryAfter(prev)
	assert.True(t,
		time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC).Equal(exact),
		"t == prev must advance to prev + 36h, got %v", exact)

	// 提前切分：t < prev → 返回 prev（计划边界不因 size/SIGHUP 切分而顺延）。
	early := r.boundaryAfter(time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC))
	assert.True(t, prev.Equal(early),
		"early rotation must keep the upcoming scheduled boundary %v, got %v", prev, early)
}

// TestBoundaryAfter24hIgnoresRecursionState 钉住 DA-1 修复的行为边界：
// d <= 24h 时即使 nextRotate 已有状态，也必须维持 D2 自然边界对齐
// （午夜锚定），不得进入周期递推分支——24h/12h/1h 既有语义零变化。
func TestBoundaryAfter24hIgnoresRecursionState(t *testing.T) {
	r := &Rotator{cfg: Config{RotationTime: 24 * time.Hour}}
	// 人为构造非午夜的状态边界（递推分支若误启用将返回 Jan-3 12:00）。
	r.nextRotate = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

	got := r.boundaryAfter(time.Date(2026, 1, 2, 13, 0, 0, 0, time.UTC))
	assert.True(t,
		time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC).Equal(got),
		"d <= 24h must stay midnight-anchored (Jan-3 00:00), got %v", got)
}

// TestRotate36hPeriodNoDegradation 是 /tmp/rl-audit/tmpmod/boundary_test.go
// TestBoundaryDegradation36h 的入仓版（2026-11-13 审计 DA-1 复现基准）：
// 36h 配置 Jan-1 00:00 启动 → 首边界 Jan-2 12:00 切分 → Jan-3 12:00
// （仅 +24h，旧实现的退化点）不得切分 → Jan-4 00:00（+36h）再次切分。
func TestRotate36hPeriodNoDegradation(t *testing.T) {
	dir := t.TempDir()
	clockPtr, clock := testClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	r, err := New(Config{
		Pattern:      filepath.Join(dir, "app-%Y%m%d%H%M.log"),
		RotationTime: 36 * time.Hour,
		Now:          clock,
	})
	require.NoError(t, err)
	defer r.Close()

	// 首边界 = Jan-1 00:00 + 36h = Jan-2 12:00（D2 启动语义连续）。
	assert.True(t,
		time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC).Equal(r.nextRotate),
		"first boundary must be Jan-2 12:00, got %v", r.nextRotate)

	_, err = r.Write([]byte("gen1"))
	require.NoError(t, err)

	// 到达首边界，切分。
	*clockPtr = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	_, err = r.Write([]byte("gen2"))
	require.NoError(t, err)
	assert.True(t,
		time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC).Equal(r.nextRotate),
		"second boundary must be first + 36h (Jan-4 00:00), got %v", r.nextRotate)

	// DA-1 退化点：Jan-3 12:00（距上一切分仅 +24h）不得切分。
	*clockPtr = time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	_, err = r.Write([]byte("gen3"))
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "app-202601031200.log"))
	assert.True(t, os.IsNotExist(statErr),
		"must not rotate at Jan-3 12:00 (+24h): 36h period degradation (DA-1)")

	// Jan-4 00:00（首边界 +36h）再次切分。
	*clockPtr = time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	_, err = r.Write([]byte("gen4"))
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, []byte("gen1"), readFile(t, filepath.Join(dir, "app-202601010000.log")))
	assert.Equal(t, []byte("gen2gen3"), readFile(t, filepath.Join(dir, "app-202601021200.log")))
	assert.Equal(t, []byte("gen4"), readFile(t, filepath.Join(dir, "app-202601040000.log")))
}

// TestFormatNameKeepsLiteralDigits 钉住 Format(layout) 缺陷的修复：Go time layout 会把
// pattern 中字面数字（1/2/3/4/5/15/01/02/2006 等）解释为时间 token（如 /data2/ → /data13/），
// 因此文件名生成必须对原始 pattern 做 token 级替换——仅 §3.3 六个占位符参与格式化，
// 其余字符（含数字与字面量 "2006"）保持原样（真 strftime 语义）。
func TestFormatNameKeepsLiteralDigits(t *testing.T) {
	now := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)

	r := &Rotator{cfg: Config{Pattern: "/data2/logs15/app-2006-%Y%m%d.log"}}
	assert.Equal(t, "/data2/logs15/app-2006-20261113.log", r.formatName(now))

	// 未知占位符保持原样（与 parsePattern 语义一致）
	r2 := &Rotator{cfg: Config{Pattern: "app-%Q-%H.log"}}
	assert.Equal(t, "app-%Q-14.log", r2.formatName(now))
}

// TestNewFailsWhenDirUncreatable 验证父目录不可创建时 New 返回 error（错误向上传播，不吞）。
// MaxSize 满足 M1，确保失败来自 mkdir 而非配置校验（防静默失真）。
func TestNewFailsWhenDirUncreatable(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0644))

	_, err := New(Config{Pattern: filepath.Join(blocker, "sub", "app.log"), MaxSize: testMaxSize})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrInvalidConfig),
		"failure must come from mkdir, not config validation")
}

// skipWindowsSymlink 在 Windows 上跳过 symlink 测试（需开发者模式/管理员权限，§7.1）。
func skipWindowsSymlink(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink on Windows requires Developer Mode or administrator privileges")
	}
}

// TestSymlinkOnStartAndUpdate 验证 §14.1-8：启动时创建 symlink 指向初始文件，
// 每次切分后更新指向新活跃文件；同目录时使用相对路径（§4.5/决策 5）。
func TestSymlinkOnStartAndUpdate(t *testing.T) {
	skipWindowsSymlink(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "current.log")

	r, err := New(Config{
		Pattern: filepath.Join(dir, "app.log"),
		MaxSize: testMaxSize,
		Symlink: link,
		Now:     func() time.Time { return cleanupNow },
	})
	require.NoError(t, err)

	// 启动时 symlink 已创建，同目录 → 相对路径
	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "app.log", got)

	_, err = r.Write([]byte("A"))
	require.NoError(t, err)
	r.RotateFlag.Store(true)
	_, err = r.Write([]byte("B"))
	require.NoError(t, err)

	// 切分后 symlink 指向新活跃文件
	got, err = os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "app.log.1", got)

	require.NoError(t, r.Close())
	// 解引用 link 读到的是活跃文件内容
	assert.Equal(t, []byte("B"), readFile(t, link))
}

// TestSymlinkCrossDirectoryAbsolute 验证 §4.5：link 与目标文件跨目录时
// symlink 使用绝对路径（决策 5），且链接可正常解析。
func TestSymlinkCrossDirectoryAbsolute(t *testing.T) {
	skipWindowsSymlink(t)
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	linkDir := filepath.Join(dir, "links")
	require.NoError(t, os.MkdirAll(linkDir, dirPerm))
	link := filepath.Join(linkDir, "current.log")
	target := filepath.Join(logDir, "app.log")

	r, err := New(Config{
		Pattern: target,
		MaxSize: testMaxSize,
		Symlink: link,
		Now:     func() time.Time { return cleanupNow },
	})
	require.NoError(t, err)
	defer r.Close()

	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got), "cross-directory symlink must use absolute path")
	wantAbs, err := filepath.Abs(target)
	require.NoError(t, err)
	assert.Equal(t, wantAbs, got)
	_, err = os.Stat(link)
	assert.NoError(t, err, "link must resolve to the target file")
}

// TestUpdateSymlinkRejectsNonSymlink 验证 M3b：link 槽位已被真实文件/目录占用时
// 拒绝替换并返回 error（防误删用户文件）；已存在的 symlink 允许原子替换；
// 槽位空闲时正常创建。
func TestUpdateSymlinkRejectsNonSymlink(t *testing.T) {
	skipWindowsSymlink(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(target, []byte("log"), filePerm))

	// 真实文件：拒绝替换，内容与类型不变
	realFile := filepath.Join(dir, "real.log")
	require.NoError(t, os.WriteFile(realFile, []byte("user data"), filePerm))
	assert.Error(t, updateSymlink(target, realFile))
	assert.Equal(t, []byte("user data"), readFile(t, realFile))
	st, err := os.Lstat(realFile)
	require.NoError(t, err)
	assert.True(t, st.Mode()&os.ModeSymlink == 0)

	// 目录：拒绝替换
	subDir := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(subDir, dirPerm))
	assert.Error(t, updateSymlink(target, subDir))
	st, err = os.Lstat(subDir)
	require.NoError(t, err)
	assert.True(t, st.IsDir())

	// 已存在的 symlink：允许替换并指向新目标
	link := filepath.Join(dir, "current.log")
	require.NoError(t, os.Symlink("stale-target", link))
	require.NoError(t, updateSymlink(target, link))
	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "app.log", got)

	// 槽位空闲：正常创建
	fresh := filepath.Join(dir, "fresh.log")
	require.NoError(t, updateSymlink(target, fresh))
	got, err = os.Readlink(fresh)
	require.NoError(t, err)
	assert.Equal(t, "app.log", got)
}

// TestUpdateSymlinkRejectsNonSymlinkTmpSlot 钉住 tmp 槽位防护（2026-11-16
// 审计 P1-B）：link+".tmp" 临时槽位被真实文件占用时拒绝清理（对齐 M3b
// checkSymlinkSlot 防御哲学）——旧实现无条件 os.Remove(tmp) 会解链该文件
// （碰撞形态下恰为活跃日志文件，写入进入孤儿 inode）。已被 symlink 占用
// 的 tmp 槽位允许清理重建（原子替换流程的一部分）。
func TestUpdateSymlinkRejectsNonSymlinkTmpSlot(t *testing.T) {
	skipWindowsSymlink(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "app.log")
	link := filepath.Join(dir, "current.log")

	// tmp 槽位被真实文件占用：拒绝，内容与类型不变
	tmpSlot := link + ".tmp"
	require.NoError(t, os.WriteFile(tmpSlot, []byte("user data"), filePerm))
	assert.Error(t, updateSymlink(target, link))
	assert.Equal(t, []byte("user data"), readFile(t, tmpSlot))
	st, err := os.Lstat(tmpSlot)
	require.NoError(t, err)
	assert.True(t, st.Mode()&os.ModeSymlink == 0)

	// tmp 槽位被既有 symlink 占用：允许清理重建，link 正常更新
	require.NoError(t, os.Remove(tmpSlot)) // 清理上一段的占位真实文件
	require.NoError(t, os.Symlink("stale-target", tmpSlot))
	require.NoError(t, updateSymlink(target, link))
	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "app.log", got)
}

// TestRotateSymlinkTmpCollisionKeepsActiveFile 钉住链 B（2026-11-16 审计
// P1-B 日期字面量形态）：symlink 名含未来日期字面量时，M3a 启动检查无法
// 预见碰撞（初始文件名不同），rotate 后新活跃文件恰命中 symlink+".tmp"
// 槽位。防护生效时 updateSymlink 拒绝解链（降级为警告），活跃文件保持
// 真实文件、Close 后数据完整，Write/rotate 全程不静默丢失。
func TestRotateSymlinkTmpCollisionKeepsActiveFile(t *testing.T) {
	skipWindowsSymlink(t)
	dir := t.TempDir()
	// 启动日 2026-11-15：初始文件 app-2026-11-15.tmp ≠ symlink+".tmp"
	// （M3a 放行，碰撞在日期推进后才可观察）
	clockPtr, clock := testClock(time.Date(2026, 11, 15, 23, 0, 0, 0, time.UTC))

	r, err := New(Config{
		Pattern:      filepath.Join(dir, "app-%Y-%m-%d.tmp"),
		RotationTime: 24 * time.Hour,
		Symlink:      filepath.Join(dir, "app-2026-11-16"), // 含明日日期字面量
		Now:          clock,
	})
	require.NoError(t, err, "day-1 start must not collide (M3a correctly passes)")

	_, err = r.Write([]byte("day1"))
	require.NoError(t, err)

	// 2026-11-16 首次切分：新活跃文件 app-2026-11-16.tmp == symlink+".tmp"
	*clockPtr = time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	r.RotateFlag.Store(true)
	_, err = r.Write([]byte("day2"))
	require.NoError(t, err, "rotate must complete despite tmp-slot rejection")
	require.NoError(t, r.Close())

	active := filepath.Join(dir, "app-2026-11-16.tmp")
	st, statErr := os.Lstat(active)
	require.NoError(t, statErr, "active file must never be unlinked")
	assert.True(t, st.Mode()&os.ModeSymlink == 0, "active file must remain a real file")
	assert.Equal(t, []byte("day2"), readFile(t, active), "no silent loss of current period")
	assert.Equal(t, []byte("day1"), readFile(t, filepath.Join(dir, "app-2026-11-15.tmp")))
}

// TestSymlinkFailureDoesNotBreak 验证 §4.5/§4.8：symlink 更新失败（M3b 拒绝
// 替换已存在的真实文件）仅降级为警告，New/Write/rotate 主流程全部正常，
// 用户文件原封不动。
func TestSymlinkFailureDoesNotBreak(t *testing.T) {
	skipWindowsSymlink(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "current.log")
	require.NoError(t, os.WriteFile(blocker, []byte("user data"), filePerm))

	r, err := New(Config{
		Pattern: filepath.Join(dir, "app.log"),
		MaxSize: testMaxSize,
		Symlink: blocker,
		Now:     func() time.Time { return cleanupNow },
	})
	require.NoError(t, err, "symlink failure must not block New")

	_, err = r.Write([]byte("A"))
	require.NoError(t, err)
	r.RotateFlag.Store(true)
	_, err = r.Write([]byte("B"))
	require.NoError(t, err, "symlink failure must not break rotate/Write")
	require.NoError(t, r.Close())

	assert.Equal(t, []byte("user data"), readFile(t, blocker))
	st, err := os.Lstat(blocker)
	require.NoError(t, err)
	assert.True(t, st.Mode()&os.ModeSymlink == 0)
	assert.Equal(t, []byte("A"), readFile(t, filepath.Join(dir, "app.log")))
	assert.Equal(t, []byte("B"), readFile(t, filepath.Join(dir, "app.log.1")))
}

// TestRotateTriggersCleanupCompress 端到端组合：Compress+MaxBackups 同开、
// 时间推进多次切分后归档全部为 .gz 且数量 ≤ MaxBackups，活跃文件保持明文
// （§14.1-5/7）。每次切分后 compressWg.Wait() 消除 skip-if-busy 时序；
// 断言对 cleanup(同步)与压缩(异步)的任意交错成立：新归档名恒排序最末，
// MaxBackups 只会裁剪更旧的归档，源文件→.gz 等量交换不改变计数上界。
func TestRotateTriggersCleanupCompress(t *testing.T) {
	dir := t.TempDir()
	clockPtr, clock := testClock(time.Date(2026, 11, 13, 14, 0, 0, 0, time.UTC))

	r, err := New(Config{
		Pattern:      filepath.Join(dir, "app-%Y%m%dT%H.log"),
		RotationTime: time.Hour,
		Compress:     true,
		MaxBackups:   2,
		Now:          clock,
	})
	require.NoError(t, err)

	segments := []struct {
		hour int
		data string
	}{{14, "A"}, {15, "B"}, {16, "C"}, {17, "D"}}
	for i, seg := range segments {
		if i > 0 {
			*clockPtr = time.Date(2026, 11, 13, seg.hour, 0, 30, 0, time.UTC)
		}
		_, err = r.Write([]byte(seg.data))
		require.NoError(t, err)
		r.compressWg.Wait()
	}
	require.NoError(t, r.Close())

	active := "app-20261113T17.log"
	entries := listNames(t, dir)
	assert.Contains(t, entries, active)
	assert.LessOrEqual(t, len(entries), 3, "backups must be capped at MaxBackups")
	for _, name := range entries {
		if name == active {
			continue
		}
		assert.True(t, strings.HasSuffix(name, ".gz"), "archive %s must be compressed", name)
		_ = gunzipFile(t, filepath.Join(dir, name)) // .gz 必须完整可解压
	}
	// 最近一次切分的归档必然存活（名字排序最末，永不被 MaxBackups 裁剪）
	assert.Equal(t, []byte("C"), gunzipFile(t, filepath.Join(dir, "app-20261113T16.log.gz")))
	assert.Equal(t, []byte("D"), readFile(t, filepath.Join(dir, active)))
}

// ============================================================================
// 心跳 idle flush（law 策略适配，DESIGN §4.1 addendum，2026-11-16）
// 假时钟注入零 sleep；心跳 aging 用 r.now()（注入优先），冻结时钟下永不老化。
// ============================================================================

// TestHeartbeatIdleFlush 钉住时间驱动 flush：假时钟冻结时心跳 aging 不刷
// （数据不可见）；推进 now ≥ FlushIdleTimeout 后 maybeFlushIdleLocked 将缓冲
// 落盘，成功后双时钟重置为 now（law 同款）。
func TestHeartbeatIdleFlush(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)
	clockPtr, clock := testClock(start)
	path := filepath.Join(dir, "app.log")

	r, err := New(Config{Pattern: path, MaxSize: testMaxSize, Now: clock})
	require.NoError(t, err)
	defer r.Close()

	_, err = r.Write([]byte("hello"))
	require.NoError(t, err)

	// 假时钟冻结：now-executeAt == 0 < FlushIdleTimeout → 不刷，文件不可见。
	r.mu.Lock()
	require.NoError(t, r.maybeFlushIdleLocked(r.now()))
	r.mu.Unlock()
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, st.Size(), "frozen clock must not age the idle window")

	// 推进到静默阈值 → flush 生效，双时钟重置为 now。
	*clockPtr = start.Add(FlushIdleTimeout)
	r.mu.Lock()
	require.NoError(t, r.maybeFlushIdleLocked(r.now()))
	assert.True(t, r.executeAt.Equal(*clockPtr), "executeAt must reset to now after idle flush")
	assert.True(t, r.lastFlushAt.Equal(*clockPtr), "lastFlushAt must reset to now after idle flush")
	r.mu.Unlock()
	assert.Equal(t, []byte("hello"), readFile(t, path))
	require.NoError(t, r.Close())
}

// TestIdleFlushDualClockStarvationBound 钉住第二时钟（lastFlushAt）的饥饿上界：
// 连续写入使 executeAt 恒新（静默分支永不满足），但 lastFlushAt 陈旧
// ≥ FlushIdleTimeout → 仍必须 flush（连续写入不得无限推迟 flush，law 同款）。
func TestIdleFlushDualClockStarvationBound(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)
	clockPtr, clock := testClock(start)
	path := filepath.Join(dir, "app.log")

	r, err := New(Config{Pattern: path, MaxSize: testMaxSize, Now: clock})
	require.NoError(t, err)
	defer r.Close()

	_, err = r.Write([]byte("x"))
	require.NoError(t, err)

	// 推进一个 idleTimeout 后再写：executeAt 刷新为 t1（静默分支差值=0），
	// lastFlushAt 停留在 t0（已陈旧整整一个 idleTimeout）。
	*clockPtr = start.Add(FlushIdleTimeout)
	_, err = r.Write([]byte("y"))
	require.NoError(t, err)

	r.mu.Lock()
	assert.True(t, r.executeAt.Equal(*clockPtr), "executeAt must be refreshed by the latest Write")
	require.NoError(t, r.maybeFlushIdleLocked(r.now()))
	r.mu.Unlock()

	assert.Equal(t, []byte("xy"), readFile(t, path),
		"stale lastFlushAt must trigger flush even while writes keep executeAt fresh")
	require.NoError(t, r.Close())
}

// TestIdleFlushSkipsEmptyBuffer 钉住 Buffered()==0 守卫：空缓冲即使双时钟
// 陈旧也不 flush、不重置时钟（no-op 语义，避免空转刷盘与时钟漂移）。
func TestIdleFlushSkipsEmptyBuffer(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)
	clockPtr, clock := testClock(start)
	path := filepath.Join(dir, "app.log")

	r, err := New(Config{Pattern: path, MaxSize: testMaxSize, Now: clock})
	require.NoError(t, err)
	defer r.Close()

	// 阶段 1：从未写入，缓冲为空 → 双时钟陈旧也不 flush、不重置。
	*clockPtr = start.Add(10 * FlushIdleTimeout)
	r.mu.Lock()
	require.NoError(t, r.maybeFlushIdleLocked(r.now()))
	assert.True(t, r.executeAt.Equal(start), "empty buffer must not reset executeAt")
	assert.True(t, r.lastFlushAt.Equal(start), "empty buffer must not reset lastFlushAt")
	r.mu.Unlock()
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, st.Size())

	// 阶段 2：写入 + Flush 排空缓冲后 → 同样 no-op。
	_, err = r.Write([]byte("z"))
	require.NoError(t, err)
	require.NoError(t, r.Flush())
	*clockPtr = clockPtr.Add(10 * FlushIdleTimeout)
	r.mu.Lock()
	drainedAt := r.lastFlushAt
	require.NoError(t, r.maybeFlushIdleLocked(r.now()))
	assert.True(t, r.lastFlushAt.Equal(drainedAt), "drained buffer must not reset lastFlushAt")
	r.mu.Unlock()
	require.NoError(t, r.Close())
	assert.Equal(t, []byte("z"), readFile(t, path))
}

// TestIdleFlushFailureKeepsClocks 钉住 law 同款失败语义：flush 失败不更新
// 任一时钟（下次心跳以原时钟重试）。白盒 errWriter 注入，无生产钩子。
// flusher 的 warn-and-continue 由 maybeFlushIdleLocked 错误返回 + warnf 组合
// 覆盖；bufio sticky error 经下一次 Write surfaced（既有错误传播路径）。
func TestIdleFlushFailureKeepsClocks(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "app.log"))
	require.NoError(t, err)
	defer f.Close()

	start := time.Date(2026, 11, 13, 14, 30, 0, 0, time.UTC)
	r := &Rotator{
		cfg:         Config{Now: func() time.Time { return start.Add(FlushIdleTimeout) }},
		path:        f.Name(),
		file:        f,
		buf:         bufio.NewWriterSize(errWriter{}, WriteBufferSize),
		executeAt:   start,
		lastFlushAt: start,
	}
	_, err = r.buf.WriteString("stuck")
	require.NoError(t, err)

	r.mu.Lock()
	err = r.maybeFlushIdleLocked(r.now())
	r.mu.Unlock()
	assert.ErrorIs(t, err, errWriteFailed)
	assert.True(t, r.executeAt.Equal(start), "failed flush must not reset executeAt")
	assert.True(t, r.lastFlushAt.Equal(start), "failed flush must not reset lastFlushAt")
}

// TestConcurrentWriteFlushClose 钉住新并发契约：Write/Flush/心跳 flusher
// 由 mu 保护可并发调用；总字节零丢失，-race 干净。真实时钟（Now=nil）让
// 心跳 flusher 真实参与竞争；零 sleep。
func TestConcurrentWriteFlushClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	r, err := New(Config{Pattern: path, MaxSize: testMaxSize})
	require.NoError(t, err)

	const writers, perWriter = 4, 300
	line := []byte("concurrent-line\n")
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, werr := r.Write(line); werr != nil {
					t.Error(werr)
					return
				}
			}
		}()
	}
	// 并发显式 Flush 与 Write/心跳竞争 mu，覆盖导出 Flush 路径。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if ferr := r.Flush(); ferr != nil {
				t.Error(ferr)
				return
			}
		}
	}()
	wg.Wait()
	require.NoError(t, r.Close())

	data := readFile(t, path)
	assert.Equal(t, writers*perWriter*len(line), len(data), "concurrent writes must be zero-loss")
	assert.Equal(t, writers*perWriter, strings.Count(string(data), "concurrent-line\n"))
}

// TestCloseConcurrentWithWrite 钉住 Close 生命周期无死锁：早置 closed 使
// 并发 Write 立即 errClosed；stopFlusher 不持 mu（flusher 可能正在等 mu，
// 持 mu 等待会死锁）。Close 与 Write 并发调用，-race 干净。
func TestCloseConcurrentWithWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	r, err := New(Config{Pattern: path, MaxSize: testMaxSize})
	require.NoError(t, err)

	_, err = r.Write([]byte("seed"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, werr := r.Write([]byte("late"))
			if werr != nil {
				assert.ErrorIs(t, werr, errClosed, "post-Close Write must fail with errClosed only")
				return
			}
		}
	}()
	require.NoError(t, r.Close())
	wg.Wait()
	require.NoError(t, r.Close()) // 幂等

	// seed 必须 durable；late 写入要么全部落盘要么被 errClosed 拒绝，无撕裂。
	data := readFile(t, path)
	assert.True(t, strings.HasPrefix(string(data), "seed"), "pre-Close data must be durable")
}

// TestCloseStopsFlusherNoLeak 钉住 flusher 生命周期：Close 同步等待 flusher
// 退出（flusherWg 归零）、stopCh 已关闭（stopOnce 防重复 close panic）、
// 30 轮 New/Write/Close 循环无 goroutine 累积、终刷 durable。
func TestCloseStopsFlusherNoLeak(t *testing.T) {
	dir := t.TempDir()
	base := runtime.NumGoroutine()
	for i := 0; i < 30; i++ {
		path := filepath.Join(dir, fmt.Sprintf("app-%d.log", i))
		r, err := New(Config{Pattern: path, MaxSize: testMaxSize})
		require.NoError(t, err)
		_, err = r.Write([]byte("durable"))
		require.NoError(t, err)
		require.NoError(t, r.Close())
		require.NoError(t, r.Close()) // 幂等：二次 Close 不得 panic（stopOnce）
		select {
		case <-r.stopCh:
		default:
			t.Fatal("stopCh must be closed after Close")
		}
		assert.Equal(t, []byte("durable"), readFile(t, path), "Close final flush must be durable")
	}
	// 已退出 goroutine 的回收让渡调度（Gosched 非 sleep）；若泄漏，计数随轮数线性增长。
	for i := 0; i < 100 && runtime.NumGoroutine() > base; i++ {
		runtime.Gosched()
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), base, "flusher goroutines must not leak across New/Close cycles")
}

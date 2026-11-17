package rotator

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupNow 是清理测试的固定当前时间（Config.Now 注入，零 sleep）。
var cleanupNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// e2eNow 是端到端清理测试（文件 mtime 由真实机器时刻产生）的注入时钟：
// 必须晚于归档的真实 mtime，否则 DA-2 钳制会把全部归档视为"未来 mtime"
// 钳为同值、退化为文件名序删除，偏离测试钉住 ModTime 时序的本意。
// 取 cleanupNow+1 年的固定日期保持跨进程确定性（固定名 pattern 无时间
// 占位符，注入日期不会进入文件名）。
var e2eNow = cleanupNow.AddDate(1, 0, 0)

// touchFile 创建归档测试文件；mtime 非零值时用 Chtimes 设置修改时间。
func touchFile(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("x"), filePerm))
	if !mtime.IsZero() {
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}
}

// listNames 返回目录下全部条目的基名，供集合断言。
func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestCleanupMaxBackups 验证 MaxBackups 按 ModTime 升序删除超出数量的最旧归档
// （mtime 相同时按文件名回退，保证确定性），当前活跃文件不参与计数也不被
// 删除（§4.3/§14.1-5）。
func TestCleanupMaxBackups(t *testing.T) {
	dir := t.TempDir()
	for d := 1; d <= 5; d++ {
		touchFile(t, filepath.Join(dir, fmt.Sprintf("app-2026-11-%02d.log", d)), time.Time{})
	}
	active := filepath.Join(dir, "app-2026-11-13.log")
	touchFile(t, active, time.Time{})

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t,
		[]string{"app-2026-11-04.log", "app-2026-11-05.log", "app-2026-11-13.log"},
		listNames(t, dir))
}

// TestCleanupMaxAge 验证 MaxAge 删除修改时间超过 N 天的归档（§4.3/§14.1-6），
// 未过期文件与活跃文件保留。
func TestCleanupMaxAge(t *testing.T) {
	dir := t.TempDir()
	expired := cleanupNow.AddDate(0, 0, -4)
	touchFile(t, filepath.Join(dir, "app-2026-11-01.log"), expired)
	touchFile(t, filepath.Join(dir, "app-2026-11-02.log"), expired)
	touchFile(t, filepath.Join(dir, "app-2026-11-12.log"), cleanupNow)
	active := filepath.Join(dir, "app-2026-11-13.log")
	touchFile(t, active, expired) // 活跃文件即使 mtime 过期也不得删除

	r := &Rotator{cfg: Config{
		Pattern: filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxAge:  1,
		Now:     func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t,
		[]string{"app-2026-11-12.log", "app-2026-11-13.log"},
		listNames(t, dir))
}

// TestCleanupBothPolicies 验证 MaxAge 与 MaxBackups 同时生效时叠加取严（§4.3）：
// 先删过期，再对剩余按 ModTime 删超额（mtime 相同按文件名回退）。
func TestCleanupBothPolicies(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, filepath.Join(dir, "app-2026-11-01.log"), cleanupNow.AddDate(0, 0, -4)) // 过期删除
	touchFile(t, filepath.Join(dir, "app-2026-11-10.log"), cleanupNow)                   // 未过期但超额删除
	touchFile(t, filepath.Join(dir, "app-2026-11-11.log"), cleanupNow)                   // 保留
	active := filepath.Join(dir, "app-2026-11-13.log")
	touchFile(t, active, time.Time{})

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxAge:     1,
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t,
		[]string{"app-2026-11-11.log", "app-2026-11-13.log"},
		listNames(t, dir))
}

// TestCleanupIgnoresUnrelated 验证 glob 不匹配的文件（other.log / app-foo.log）
// 即使 mtime 过期也绝不误删（D4 逐 token 替换比 §4.3 示例更严格）。
func TestCleanupIgnoresUnrelated(t *testing.T) {
	dir := t.TempDir()
	ancient := cleanupNow.AddDate(0, 0, -10)
	touchFile(t, filepath.Join(dir, "other.log"), ancient)
	touchFile(t, filepath.Join(dir, "app-foo.log"), ancient)
	touchFile(t, filepath.Join(dir, "app-2026-11-12.log"), cleanupNow)
	active := filepath.Join(dir, "app-2026-11-13.log")
	touchFile(t, active, time.Time{})

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxAge:     1,
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t,
		[]string{"other.log", "app-foo.log", "app-2026-11-12.log", "app-2026-11-13.log"},
		listNames(t, dir))
}

// TestCleanupCoversGzAndSerial 验证 D4 变体集：压缩产物 .gz、冲突序号 .1
// 及序号+压缩组合 .2.gz 均纳入清理计数与删除，否则永不清理导致磁盘泄漏。
func TestCleanupCoversGzAndSerial(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, filepath.Join(dir, "app-2026-11-01.log.gz"), time.Time{})
	touchFile(t, filepath.Join(dir, "app-2026-11-02.log.1"), time.Time{})
	touchFile(t, filepath.Join(dir, "app-2026-11-03.log.2.gz"), time.Time{})
	touchFile(t, filepath.Join(dir, "app-2026-11-04.log"), time.Time{})
	active := filepath.Join(dir, "app-2026-11-13.log")
	touchFile(t, active, time.Time{})

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t,
		[]string{"app-2026-11-03.log.2.gz", "app-2026-11-04.log", "app-2026-11-13.log"},
		listNames(t, dir))
}

// TestCleanupNeverDeletesCurrent 验证固定文件名模式下当前活跃文件即使
// 名字最旧且 mtime 过期也绝不被删除（MaxAge/MaxBackups 双策略同时尝试）。
func TestCleanupNeverDeletesCurrent(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "app.log")
	touchFile(t, active, cleanupNow.AddDate(0, 0, -10))
	touchFile(t, filepath.Join(dir, "app.log.1"), cleanupNow)
	touchFile(t, filepath.Join(dir, "app.log.2"), cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxAge:     1,
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t, []string{"app.log", "app.log.2"}, listNames(t, dir))
}

// TestCleanupSkipsSymlinkPath 验证 M4：glob 匹配结果跳过 cfg.Symlink 本身
// （绝对化比较），防止 MaxBackups 把 symlink 当最旧归档误删。
func TestCleanupSkipsSymlinkPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink on Windows requires Developer Mode or administrator privileges")
	}
	dir := t.TempDir()
	active := filepath.Join(dir, "app-2026-11-13.log")
	touchFile(t, active, time.Time{})
	// symlink 命名匹配 glob——若无 M4 保护将被纳入 MaxBackups 计数，
	// 挤占保留名额导致真实归档被超额删除。
	link := filepath.Join(dir, "app-2026-01-01.log")
	require.NoError(t, os.Symlink("app-2026-11-13.log", link))
	touchFile(t, filepath.Join(dir, "app-2026-11-10.log"), cleanupNow)
	touchFile(t, filepath.Join(dir, "app-2026-11-11.log"), cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxBackups: 2,
		Symlink:    link,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	st, err := os.Lstat(link)
	require.NoError(t, err, "symlink must survive cleanup (M4)")
	assert.True(t, st.Mode()&os.ModeSymlink != 0, "path must still be a symlink")
	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, "app-2026-11-13.log", got)
	assert.ElementsMatch(t,
		[]string{"app-2026-01-01.log", "app-2026-11-10.log", "app-2026-11-11.log", "app-2026-11-13.log"},
		listNames(t, dir))
}

// TestCleanupFailureDoesNotAffectWrite 验证 §4.3/§4.8：cleanup 内部失败
// （glob 命中悬空 symlink → Stat 失败）仅降级为警告，经 rotate 挂点触发时
// 绝不传播进 Write 返回值，日志内容完整。
func TestCleanupFailureDoesNotAffectWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink on Windows requires Developer Mode or administrator privileges")
	}
	dir := t.TempDir()
	// 匹配 glob 的悬空 symlink：cleanup 的 MaxAge Stat 必然失败（真实失败路径）
	dangling := filepath.Join(dir, "app-2026-01-01.log")
	require.NoError(t, os.Symlink("nonexistent-target", dangling))

	r, err := New(Config{
		Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxSize:    testMaxSize,
		MaxAge:     1,
		MaxBackups: 5,
		Now:        func() time.Time { return cleanupNow },
	})
	require.NoError(t, err)

	_, err = r.Write([]byte("A"))
	require.NoError(t, err)
	r.RotateFlag.Store(true)
	_, err = r.Write([]byte("B"))
	require.NoError(t, err, "cleanup failure must not propagate into Write")
	require.NoError(t, r.Close())

	// 失败仅警告：悬空 symlink 原样保留，两个日志文件内容完整
	st, lerr := os.Lstat(dangling)
	require.NoError(t, lerr)
	assert.True(t, st.Mode()&os.ModeSymlink != 0)
	assert.Equal(t, []byte("A"), readFile(t, filepath.Join(dir, "app-2026-11-13.log")))
	assert.Equal(t, []byte("B"), readFile(t, filepath.Join(dir, "app-2026-11-13.log.1")))
}

// TestCleanupMaxBackupsKeepsNewestByModTime 钉住 P0-1 场景 A（序号 ≥.10）：
// 字典序 app.log.1 < app.log.10 < app.log.11 < app.log.2 与 mtime 时间序不一致，
// 按名字排序会把时间序最新的 .10/.11 当"最旧"误删。§4.3（2026-11-13 修订）
// 要求按 ModTime 排序保留最新 N 个。
func TestCleanupMaxBackupsKeepsNewestByModTime(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, filepath.Join(dir, "app.log.1"), cleanupNow.Add(-5*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.2"), cleanupNow.Add(-4*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.9"), cleanupNow.Add(-3*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.10"), cleanupNow.Add(-2*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.11"), cleanupNow.Add(-1*time.Hour))
	active := filepath.Join(dir, "app.log.12")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	// §14.1-5 补强语义：保留集合必须是 mtime 最新的 2 个归档（.10/.11），
	// 而非仅断言"数量 ≤ 2"。
	assert.ElementsMatch(t,
		[]string{"app.log.10", "app.log.11", "app.log.12"},
		listNames(t, dir))
}

// TestCleanupMaxBackupsSerialReuseKeepsNewest 钉住 P0-1 场景 B（固定名+序号重用）：
// app.log 被上一轮清理删除后经 uniqueName 重用、携带次新数据，其字典序最小
// 但 mtime 并非最旧——按名字排序会误删，按 ModTime 排序必须保留。
func TestCleanupMaxBackupsSerialReuseKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, filepath.Join(dir, "app.log.5"), cleanupNow.Add(-4*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.6"), cleanupNow.Add(-3*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.7"), cleanupNow.Add(-2*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log"), cleanupNow.Add(-1*time.Hour)) // 序号重用：次新数据
	active := filepath.Join(dir, "app.log.8")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxBackups: 3,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t,
		[]string{"app.log", "app.log.6", "app.log.7", "app.log.8"},
		listNames(t, dir))
}

// TestCleanupMaxBackupsEndToEndKeepsNewestByModTime 钉住 §14.1-5 端到端：
// 固定文件名 + MaxBackups=2 连续大小切分 6 轮（含删除后的序号重用），
// 每轮被删的都是 mtime 最旧者，最终保留的归档集合 = mtime 最新的 2 个
// 且内容与写入序一致（旧版按名字排序在末轮会误删最新的 app.log(W5)）。
// 注入时钟用 e2eNow（晚于真实文件 mtime）：文件由真实 Write 产生、mtime
// 为机器当前时刻，若注入 cleanupNow 则全部归档被 DA-2 钳制为同值、退化
// 文件名序，偏离本测试钉住 ModTime 时序的意图。
func TestCleanupMaxBackupsEndToEndKeepsNewestByModTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	r, err := New(Config{
		Pattern:    path,
		MaxSize:    10,
		MaxBackups: 2,
		Now:        func() time.Time { return e2eNow },
	})
	require.NoError(t, err)

	// 6 轮 × 10B：第 2~6 轮写入前各触发一次切分（共 5 次），归档推进形态：
	// app.log(W1) .1(W2) .2(W3) .3(W4) app.log(W5,重用) .1(W6,重用,活跃)。
	for i := 1; i <= 6; i++ {
		_, err := r.Write([]byte(fmt.Sprintf("W%dxxxxxxxx", i)))
		require.NoError(t, err)
	}
	require.NoError(t, r.Close())

	// 切分 4/5/6 的 cleanup 各删 mtime 最旧一份（W1/W2/W3），
	// 最终保留 app.log(W5)、app.log.3(W4) 两个最新归档 + 活跃 app.log.1(W6)。
	assert.ElementsMatch(t, []string{"app.log", "app.log.1", "app.log.3"}, listNames(t, dir))
	assert.Equal(t, "W5xxxxxxxx", string(readFile(t, path)), "次新归档必须存活")
	assert.Equal(t, "W4xxxxxxxx", string(readFile(t, path+".3")))
	assert.Equal(t, "W6xxxxxxxx", string(readFile(t, path+".1")), "活跃文件")
}

// TestCleanupSkipsCompressInFlight 钉住 P1-2：压缩在途时，源文件与其精确 .gz
// 目标产物不得被同轮 cleanup 删除（§4.2 步骤 8/9 的交错窗口）。白盒直置
// compressBusy+compressing 构造在途状态，零时序依赖；在途文件即使 mtime
// 较旧、数量超额也必须存活。在途归档占用一个保留名额（源→.gz 等量交换），
// 已完成归档保留 MaxBackups-1 个：最旧的 app.log.2 被删、app.log.3 保留。
func TestCleanupSkipsCompressInFlight(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log.1") // 压缩源（在途）
	touchFile(t, src, cleanupNow.Add(-2*time.Hour))
	halfGz := src + gzSuffix // 在途 .gz 产物（createGzFile 已定名）
	touchFile(t, halfGz, cleanupNow.Add(-2*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.2"), cleanupNow.Add(-4*time.Hour)) // 最旧，应删
	touchFile(t, filepath.Join(dir, "app.log.3"), cleanupNow.Add(-1*time.Hour)) // 最新，保留
	active := filepath.Join(dir, "app.log.4")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	// 白盒构造压缩在途状态（等价于 compressAsync 发布精确记录后、goroutine 完成前）。
	r.compressBusy.Store(true)
	r.compressing.Store(&compressRecord{src: filepath.Clean(src), gz: filepath.Clean(halfGz)})
	r.cleanup()

	assert.ElementsMatch(t,
		[]string{"app.log.1", "app.log.1.gz", "app.log.3", "app.log.4"},
		listNames(t, dir),
		"在途源文件与半成品 .gz 必须存活；最旧的非在途归档 app.log.2 被删除")
}

// archiveCount 统计 dir 下归档文件数（排除活跃文件 activeBase，§14.1-5 口径：
// .gz 与未压缩归档均计入）。
func archiveCount(t *testing.T, dir, activeBase string) int {
	t.Helper()
	n := 0
	for _, name := range listNames(t, dir) {
		if name != activeBase {
			n++
		}
	}
	return n
}

// TestCleanupMaxBackupsBoundWithCompress12Rounds 钉住 NEW-P1-1（§14.1-5）：
// 固定名 pattern + Compress + MaxBackups=2 连续 12 轮"写入→切分→等待压缩完成"，
// 归档文件数（.gz + 未压缩归档，不含活跃文件）必须始终 ≤ MaxBackups。
// 在途排除按 ".N.gz" 名字家族整体豁免时，家族豁免面覆盖已完成历史归档且
// createGzFile 序号探测使家族每轮 +1 → cleanup 永久空转、归档 0→7 单调增长
// （复审实证），本测试必 RED；排除集精确为 {src, gzExact} 后收敛 GREEN。
// 白盒 compressWg.Wait() 同步，零 sleep、零时序依赖。
func TestCleanupMaxBackupsBoundWithCompress12Rounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	r, err := New(Config{
		Pattern:    path,
		MaxSize:    10,
		MaxBackups: 2,
		Compress:   true,
		Now:        func() time.Time { return cleanupNow },
	})
	require.NoError(t, err)

	// 首个 10B 写满初始活跃文件；其后每次 10B 写入在入口触发一次切分，
	// 循环 12 次 = 12 轮切分（与复现实证的 12 轮口径一致）。
	_, err = r.Write([]byte("W00xxxxxxx"))
	require.NoError(t, err)
	for i := 1; i <= 12; i++ {
		_, err = r.Write([]byte(fmt.Sprintf("W%02dxxxxxx", i)))
		require.NoError(t, err)
		r.compressWg.Wait() // 等待本轮压缩完成后再断言（收敛态）
		assert.LessOrEqualf(t, archiveCount(t, dir, filepath.Base(r.path)), 2,
			"round %d: 归档数不得超过 MaxBackups（§14.1-5）", i)
	}
	require.NoError(t, r.Close())
	assert.LessOrEqual(t, archiveCount(t, dir, filepath.Base(r.path)), 2,
		"末轮归档数不得超过 MaxBackups")
}

// TestCleanupEvictsCompletedGzWhenSrcInFlight 钉住 NEW-P1-1 单元级语义：
// 压缩在途（src=app.log、精确目标 app.log.9.gz）时，已完成的历史 .gz 归档
// （app.log.gz/app.log.1.gz）不得因与在途源同属 ".N.gz" 名字家族而被整体
// 豁免——它们是正常归档，必须参与 MaxBackups 淘汰；排除集精确为
// {在途源, 其精确 .gz 目标} 两元素。家族豁免下本测试 RED（历史归档全部
// 存活、一个不删）。
func TestCleanupEvictsCompletedGzWhenSrcInFlight(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log") // 在途压缩源
	touchFile(t, src, cleanupNow.Add(-30*time.Minute))
	exactGz := filepath.Join(dir, "app.log.9.gz") // 在途精确目标（序号探测结果）
	touchFile(t, exactGz, cleanupNow.Add(-30*time.Minute))
	touchFile(t, filepath.Join(dir, "app.log.gz"), cleanupNow.Add(-4*time.Hour))   // 已完成历史归档，最旧应删
	touchFile(t, filepath.Join(dir, "app.log.1.gz"), cleanupNow.Add(-3*time.Hour)) // 已完成历史归档，次旧应删
	touchFile(t, filepath.Join(dir, "app.log.2"), cleanupNow.Add(-1*time.Hour))    // 已完成未压缩归档，最新保留
	active := filepath.Join(dir, "app.log.3")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	// 白盒构造压缩在途状态（等价于 compressAsync 发布精确记录后、goroutine 未完成）。
	r.compressBusy.Store(true)
	r.compressing.Store(&compressRecord{src: filepath.Clean(src), gz: filepath.Clean(exactGz)})
	r.cleanup()

	// keep = MaxBackups-1 = 1（在途源占一个名额）：历史 .gz 家族不再豁免后，
	// 可淘汰集 = {app.log.gz, app.log.1.gz, app.log.2}，删最旧两个、留 app.log.2；
	// 在途对 {app.log, app.log.9.gz} 即使形态属 ".N.gz" 家族也精确存活。
	assert.ElementsMatch(t,
		[]string{"app.log", "app.log.9.gz", "app.log.2", "app.log.3"},
		listNames(t, dir),
		"已完成历史 .gz 必须参与淘汰；仅 {在途源, 精确 gz 目标} 被跳过")
}

// TestCompressingRecordLifecycle 钉住在途记录的写入/清除时机：compressAsync
// 返回后（goroutine 启动前）记录已写入，压缩完成 Wait 返回后记录被清空；
// busy skip 路径不得覆盖既有在途记录。
func TestCompressingRecordLifecycle(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(src, []byte("data"), filePerm))

	r := &Rotator{}
	// skip 路径（busy 被白盒占用）：不写入、不覆盖在途记录
	r.compressBusy.Store(true)
	r.compressAsync(src)
	assert.Nil(t, r.compressing.Load(), "skip path must not record the in-flight path")
	r.compressBusy.Store(false)

	r.compressAsync(src)
	r.compressWg.Wait()
	assert.Nil(t, r.compressing.Load(), "record must be cleared after compression completes")
	assert.FileExists(t, src+gzSuffix)
}

// TestCleanupClockRewindDeletesClampedFirst 钉住 DA-2（2026-11-13 审计）：
// 时钟 step 回拨（NTP 校正/VM 挂起恢复）后，时钟快进期产生的归档携带未来
// mtime（mtime > now）。未钳制时排序视其为"最新"、MaxBackups 反而删除真实
// 最新归档（保留语义反转）。修复语义：未来 mtime 钳制为 now 参与排序，
// 钳制后 mtime 相同时被钳制者优先删除——真实当期新文件幸存。
func TestCleanupClockRewindDeletesClampedFirst(t *testing.T) {
	dir := t.TempDir()
	// 时钟快进期归档：真实数据陈旧，但 mtime 落在未来（回拨产物）
	touchFile(t, filepath.Join(dir, "app.log.1"), cleanupNow.Add(2*time.Hour))
	// 回拨后的真实最新归档：mtime = now
	touchFile(t, filepath.Join(dir, "app.log.2"), cleanupNow)
	active := filepath.Join(dir, "app.log.3")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t, []string{"app.log.2", "app.log.3"}, listNames(t, dir),
		"被钳制者（未来 mtime）必须优先删除，真实最新归档不得被误删")
}

// TestCleanupClockRewindMaxAgeCountsClamped 钉住 DA-2 的 MaxAge 维度：未来
// mtime 文件按钳制后 mtime（=now）参与 cutoff 判定与后续计数，不再因
// mtime > cutoff 恒成立而"永生"逃逸——配合 MaxBackups 验证其可被正常淘汰；
// 真实过期文件仍由 MaxAge 删除，真实当期文件幸存。
func TestCleanupClockRewindMaxAgeCountsClamped(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, filepath.Join(dir, "app.log.1"), cleanupNow.Add(2*time.Hour))  // 未来 mtime
	touchFile(t, filepath.Join(dir, "app.log.2"), cleanupNow)                   // 真实最新
	touchFile(t, filepath.Join(dir, "app.log.3"), cleanupNow.AddDate(0, 0, -5)) // 真实过期
	active := filepath.Join(dir, "app.log.4")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxAge:     1,
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t, []string{"app.log.2", "app.log.4"}, listNames(t, dir),
		"过期者被 MaxAge 删除；未来 mtime 者钳制后参与计数、被 MaxBackups 优先淘汰；真实最新幸存")
}

// mkdirAll 创建 cronolog 形态测试所需的日期目录层级。
func mkdirAll(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, dirPerm))
}

// TestCleanupCronologCrossDirectoryMaxBackups 钉住 D4 跨目录 glob 语义
// （Step 0，阶段 C 优化计划）：pattern 目录部分含 strftime 占位符
// （%Y/%m/app-%d.log）时，归档分散在多个日期目录，MaxBackups 必须跨目录
// 统一按 ModTime 淘汰最旧者（即使它位于与其他归档不同的目录），且日期
// 目录中不匹配 D4 变体的第三方文件绝不误删。本测试与其余 cronolog 用例
// 共同构成 cleanup 双路径改造（平面目录快路径 vs 跨目录 glob 回退路径）
// 的等价性锚点：改造前后行为必须逐项一致。
func TestCleanupCronologCrossDirectoryMaxBackups(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2026", "10"))
	mkdirAll(t, filepath.Join(dir, "2026", "11"))
	// 跨两个日期目录的归档，mtime 严格递减（最旧者在 10 月目录）
	touchFile(t, filepath.Join(dir, "2026/10/app-28.log"), cleanupNow.Add(-5*time.Hour))
	touchFile(t, filepath.Join(dir, "2026/10/app-29.log"), cleanupNow.Add(-4*time.Hour))
	touchFile(t, filepath.Join(dir, "2026/11/app-01.log"), cleanupNow.Add(-3*time.Hour))
	// 日期目录内的第三方文件：不匹配 app-*.log 变体集，即使 mtime 最旧也不得误删
	touchFile(t, filepath.Join(dir, "2026/10/access.log"), cleanupNow.Add(-48*time.Hour))
	touchFile(t, filepath.Join(dir, "2026/11/notes.txt"), cleanupNow.Add(-48*time.Hour))
	active := filepath.Join(dir, "2026/11/app-13.log")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "%Y", "%m", "app-%d.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	// 跨目录统一淘汰：最旧的 10/app-28.log 被删（尽管位于独立目录）；
	// 保留 mtime 最新的 2 个归档 + 活跃文件；第三方文件原样保留。
	assert.NoFileExists(t, filepath.Join(dir, "2026/10/app-28.log"))
	assert.FileExists(t, filepath.Join(dir, "2026/10/app-29.log"))
	assert.FileExists(t, filepath.Join(dir, "2026/11/app-01.log"))
	assert.FileExists(t, active)
	assert.ElementsMatch(t, []string{"access.log", "app-29.log"}, listNames(t, filepath.Join(dir, "2026/10")))
	assert.ElementsMatch(t, []string{"notes.txt", "app-01.log", "app-13.log"}, listNames(t, filepath.Join(dir, "2026/11")))
}

// TestCleanupCronologMaxAgeAcrossDirs 钉住 cronolog 形态的 MaxAge 维度：
// 过期判定与删除跨日期目录生效（陈旧年月目录中的归档同样被清理），
// 未过期归档与活跃文件保留——活跃文件即使 mtime 过期也绝不被删除
// （与平面目录 TestCleanupMaxAge/TestCleanupNeverDeletesCurrent 语义对齐）。
func TestCleanupCronologMaxAgeAcrossDirs(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2025", "12"))
	mkdirAll(t, filepath.Join(dir, "2026", "01"))
	mkdirAll(t, filepath.Join(dir, "2026", "11"))
	touchFile(t, filepath.Join(dir, "2025/12/app-20.log"), cleanupNow.AddDate(0, 0, -10)) // 过期
	touchFile(t, filepath.Join(dir, "2026/01/app-05.log"), cleanupNow.AddDate(0, 0, -8))  // 过期
	touchFile(t, filepath.Join(dir, "2026/11/app-12.log"), cleanupNow.Add(-12*time.Hour)) // 未过期
	active := filepath.Join(dir, "2026/11/app-13.log")
	touchFile(t, active, cleanupNow.AddDate(0, 0, -10)) // 活跃文件 mtime 过期也不得删除

	r := &Rotator{cfg: Config{
		Pattern: filepath.Join(dir, "%Y", "%m", "app-%d.log"),
		MaxAge:  1,
		Now:     func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.NoFileExists(t, filepath.Join(dir, "2025/12/app-20.log"))
	assert.NoFileExists(t, filepath.Join(dir, "2026/01/app-05.log"))
	assert.FileExists(t, filepath.Join(dir, "2026/11/app-12.log"))
	assert.FileExists(t, active)
}

// TestCleanupCronologInFlightExclusion 钉住 cronolog 形态与压缩在途排除
// （P1-2/NEW-P1-1）的交互：在途源文件与其精确 .gz 目标（序号探测产物
// app-12.log.3.gz，位于日期目录内）即使 mtime 较旧也必须存活；在途对
// 占用一个保留名额（keep--），已完成归档仅保留 MaxBackups-1 个——最旧的
// 已完成归档（位于另一日期目录）被正常淘汰，不因跨目录形态逃逸。
func TestCleanupCronologInFlightExclusion(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2026", "10"))
	mkdirAll(t, filepath.Join(dir, "2026", "11"))
	src := filepath.Join(dir, "2026/11/app-12.log")         // 在途压缩源
	halfGz := filepath.Join(dir, "2026/11/app-12.log.3.gz") // 在途精确 .gz 目标
	touchFile(t, src, cleanupNow.Add(-2*time.Hour))
	touchFile(t, halfGz, cleanupNow.Add(-2*time.Hour))
	touchFile(t, filepath.Join(dir, "2026/10/app-30.log"), cleanupNow.Add(-4*time.Hour)) // 最旧已完成，应删
	touchFile(t, filepath.Join(dir, "2026/11/app-10.log"), cleanupNow.Add(-3*time.Hour)) // 次新已完成，保留
	active := filepath.Join(dir, "2026/11/app-13.log")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "%Y", "%m", "app-%d.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	// 白盒构造压缩在途状态（等价于 compressAsync 发布精确记录后、goroutine 完成前）。
	r.compressBusy.Store(true)
	r.compressing.Store(&compressRecord{src: filepath.Clean(src), gz: filepath.Clean(halfGz)})
	r.cleanup()

	assert.FileExists(t, src, "在途源文件必须存活（P1-2）")
	assert.FileExists(t, halfGz, "在途精确 .gz 目标必须存活（NEW-P1-1）")
	assert.NoFileExists(t, filepath.Join(dir, "2026/10/app-30.log"),
		"keep-- 后最旧的已完成归档必须被跨目录淘汰")
	assert.FileExists(t, filepath.Join(dir, "2026/11/app-10.log"))
	assert.FileExists(t, active)
}

// TestCleanupCronologCoversGzAndSerial 钉住 cronolog 形态的 D4 变体集：
// 压缩产物 .gz、冲突序号 .N 及序号+压缩组合 .N.gz 分散在日期目录中时
// 同样纳入清理计数与淘汰（否则跨目录形态下压缩产物永不清理导致磁盘泄漏），
// 与平面目录 TestCleanupCoversGzAndSerial 语义对齐。
func TestCleanupCronologCoversGzAndSerial(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2026", "10"))
	mkdirAll(t, filepath.Join(dir, "2026", "11"))
	touchFile(t, filepath.Join(dir, "2026/10/app-01.log.gz"), cleanupNow.Add(-8*time.Hour)) // 最旧，应删
	touchFile(t, filepath.Join(dir, "2026/10/app-02.log.1"), cleanupNow.Add(-7*time.Hour))  // 次旧，应删
	touchFile(t, filepath.Join(dir, "2026/11/app-03.log.2.gz"), cleanupNow.Add(-6*time.Hour))
	touchFile(t, filepath.Join(dir, "2026/11/app-04.log"), cleanupNow.Add(-5*time.Hour))
	active := filepath.Join(dir, "2026/11/app-13.log")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "%Y", "%m", "app-%d.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.ElementsMatch(t, []string{"app-03.log.2.gz", "app-04.log", "app-13.log"},
		listNames(t, filepath.Join(dir, "2026/11")))
	assert.NoDirExists(t, filepath.Join(dir, "2026/10"),
		".gz 与序号变体必须跨目录纳入淘汰，否则磁盘泄漏；目录清空后随 N3-B 回收")
}

// TestCleanupCronologPrunesEmptyDateDirs 钉住 N3-B（2026-11-15 审计）：
// cronolog 嵌套形态（%Y/%m/%d）下 MaxAge 删除过期归档后，由此清空的日期
// 目录链（日→月→年）被 best-effort 逐级向上回收；仍有存活归档的日期目录
// 及其祖先链保留（ENOTEMPTY 天然保护）。
func TestCleanupCronologPrunesEmptyDateDirs(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "logs") // pattern 静态字面量前缀（回收边界）
	expired := cleanupNow.AddDate(0, 0, -10)
	// 2025/12/25 整链过期：删除后 25→12→2025 应全部回收
	mkdirAll(t, filepath.Join(base, "2025", "12", "25"))
	touchFile(t, filepath.Join(base, "2025/12/25/access.log"), expired)
	// 2026/10 下两个日目录全部过期（回收至 10），2026/11/12 未过期（保留）
	mkdirAll(t, filepath.Join(base, "2026", "10", "01"))
	mkdirAll(t, filepath.Join(base, "2026", "10", "02"))
	mkdirAll(t, filepath.Join(base, "2026", "11", "12"))
	touchFile(t, filepath.Join(base, "2026/10/01/access.log"), expired)
	touchFile(t, filepath.Join(base, "2026/10/02/access.log"), expired)
	touchFile(t, filepath.Join(base, "2026/11/12/access.log"), cleanupNow)
	active := filepath.Join(base, "2026/11/13/access.log")
	mkdirAll(t, filepath.Dir(active))
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern: filepath.Join(base, "%Y", "%m", "%d", "access.log"),
		MaxAge:  1,
		Now:     func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.NoFileExists(t, filepath.Join(base, "2025/12/25/access.log"))
	assert.NoDirExists(t, filepath.Join(base, "2025"), "空年份链 25→12→2025 应被逐级回收")
	assert.NoDirExists(t, filepath.Join(base, "2026/10"), "空月份链 01/02→10 应被回收")
	assert.DirExists(t, filepath.Join(base, "2026"), "仍含存活归档的年份目录不得回收")
	assert.FileExists(t, filepath.Join(base, "2026/11/12/access.log"))
	assert.FileExists(t, active)
	assert.DirExists(t, base)
}

// TestCleanupCronologPruneStopsAtNonEmptyDir 钉住 N3-B 的安全边界：回收仅限
// 空目录——含第三方文件的日期目录原样保留（ENOTEMPTY 即停），活跃文件所在
// 目录天然非空同样受保护，祖先链不受影响。
func TestCleanupCronologPruneStopsAtNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2026", "10", "01"))
	touchFile(t, filepath.Join(dir, "2026/10/01/access.log"), cleanupNow.AddDate(0, 0, -10))
	// 第三方文件：不匹配 D4 变体集，所在目录因此非空
	touchFile(t, filepath.Join(dir, "2026/10/01/keep.txt"), cleanupNow.AddDate(0, 0, -10))
	active := filepath.Join(dir, "2026/11/13/access.log")
	mkdirAll(t, filepath.Dir(active))
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern: filepath.Join(dir, "%Y", "%m", "%d", "access.log"),
		MaxAge:  1,
		Now:     func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	assert.NoFileExists(t, filepath.Join(dir, "2026/10/01/access.log"))
	assert.FileExists(t, filepath.Join(dir, "2026/10/01/keep.txt"))
	assert.DirExists(t, filepath.Join(dir, "2026/10/01"), "非空目录不得回收")
	assert.DirExists(t, filepath.Join(dir, "2026/10"))
	assert.DirExists(t, filepath.Join(dir, "2026/11/13"))
}

// TestCleanupCronologPruneStopsAtLiteralPrefix 钉住 N3-B 的边界契约：即使
// 静态字面量前缀下的整条占位符目录链全部清空，前缀本身及其祖先也绝不删除
// （白盒：活跃文件未落盘，前缀目录真实变空——若无边界检查将被 os.Remove 删除）。
func TestCleanupCronologPruneStopsAtLiteralPrefix(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "logs")
	mkdirAll(t, filepath.Join(base, "2026", "10", "01"))
	touchFile(t, filepath.Join(base, "2026/10/01/access.log"), cleanupNow.AddDate(0, 0, -10))

	r := &Rotator{cfg: Config{
		Pattern: filepath.Join(base, "%Y", "%m", "%d", "access.log"),
		MaxAge:  1,
		Now:     func() time.Time { return cleanupNow },
	}, path: filepath.Join(base, "2026/11/13/access.log")}
	r.cleanup()

	assert.NoFileExists(t, filepath.Join(base, "2026/10/01/access.log"))
	assert.NoDirExists(t, filepath.Join(base, "2026"), "占位符目录链应全部回收")
	assert.DirExists(t, base, "字面量前缀本身即使已空也不得删除")
	assert.DirExists(t, dir)
}

// TestCleanupCronologDanglingSymlinkExcluded 钉住 cronolog 回退路径的
// Stat 失败排除语义（C1 双路径改造的等价性锚点之一）：跨目录 glob 命中的
// 悬空 symlink 不得被删除、不得参与保留计数——与平面目录
// TestCleanupFailureDoesNotAffectWrite 的悬空链接语义在两种路径下逐项一致
// （旧实现在排序/过期阶段 Stat 失败排除；C1 元数据贯通后在匹配阶段排除，
// 净效果相同：警告、不删、不计数）。
func TestCleanupCronologDanglingSymlinkExcluded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink on Windows requires Developer Mode or administrator privileges")
	}
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2026", "11"))
	dangling := filepath.Join(dir, "2026/11/app-01.log")
	require.NoError(t, os.Symlink("nonexistent-target", dangling))
	touchFile(t, filepath.Join(dir, "2026/11/app-10.log"), cleanupNow.Add(-3*time.Hour))
	touchFile(t, filepath.Join(dir, "2026/11/app-11.log"), cleanupNow.Add(-2*time.Hour))
	active := filepath.Join(dir, "2026/11/app-13.log")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "%Y", "%m", "app-%d.log"),
		MaxAge:     1,
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	// 悬空 symlink 不删也不计数：可计数候选 = {app-10, app-11}，keep=1
	// → 仅淘汰最旧的 app-10。若悬空链接被误计数占位，将连带多删一个。
	st, err := os.Lstat(dangling)
	require.NoError(t, err, "悬空 symlink 必须存活（无法确认时序的文件不删）")
	assert.True(t, st.Mode()&os.ModeSymlink != 0)
	assert.NoFileExists(t, filepath.Join(dir, "2026/11/app-10.log"))
	assert.FileExists(t, filepath.Join(dir, "2026/11/app-11.log"))
	assert.FileExists(t, active)
}

// TestCleanupClockRewindTieBreakDeterministic 钉住 DA-2 tie-break 确定性：
// 多个未来 mtime 归档钳制为同值 now 后，删除顺序按文件名升序回退（可重复）；
// 非钳制（真实当期）文件恒排在全部被钳制者之后、优先保留。
func TestCleanupClockRewindTieBreakDeterministic(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, filepath.Join(dir, "app.log.1"), cleanupNow.Add(3*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.2"), cleanupNow.Add(1*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.3"), cleanupNow.Add(2*time.Hour))
	touchFile(t, filepath.Join(dir, "app.log.4"), cleanupNow) // 真实当期
	active := filepath.Join(dir, "app.log.5")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app.log"),
		MaxBackups: 2,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	// 被钳制者按名升序 .1/.2/.3，真实当期 .4 恒居其后；
	// keep=2 → 删 .1/.2，保留 .3（被钳制者中最后）与 .4。
	assert.ElementsMatch(t, []string{"app.log.3", "app.log.4", "app.log.5"}, listNames(t, dir),
		"被钳制文件间删除顺序按文件名升序确定；非钳制文件优先保留")
}

// TestCleanupFlatSymlinkTargetMTime 钉住平面目录快路径（C1）的 symlink
// 跟随语义：匹配 D4 变体、非 cfg.Symlink 槽位的 symlink 是合法候选，其
// 排序 mtime 取 os.Stat 跟随后的目标值（与旧实现排序/过期阶段 Stat 语义
// 一致）；mtime 相同按文件名升序 tie-break，链接名（05）先于目标名（12），
// os.Remove 删除的是链接本身、目标文件幸存。
func TestCleanupFlatSymlinkTargetMTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink on Windows requires Developer Mode or administrator privileges")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "app-2026-11-12.log")
	touchFile(t, real, cleanupNow.Add(-time.Hour))
	link := filepath.Join(dir, "app-2026-11-05.log")
	require.NoError(t, os.Symlink("app-2026-11-12.log", link))
	active := filepath.Join(dir, "app-2026-11-13.log")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	_, err := os.Lstat(link)
	assert.True(t, os.IsNotExist(err), "symlink 候选按目标 mtime 参与淘汰，本例中被删除的是链接本身")
	assert.FileExists(t, real, "目标文件必须幸存")
	assert.FileExists(t, active)
}

// TestCleanupCronologSymlinkTargetMTime 钉住 cronolog 回退路径（C1）的
// symlink 跟随语义，与平面路径 TestCleanupFlatSymlinkTargetMTime 逐项一致：
// 候选 mtime 取 Stat 跟随后的目标值，tie-break 后删除链接、目标幸存。
func TestCleanupCronologSymlinkTargetMTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink on Windows requires Developer Mode or administrator privileges")
	}
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2026", "11"))
	real := filepath.Join(dir, "2026/11/app-12.log")
	touchFile(t, real, cleanupNow.Add(-time.Hour))
	link := filepath.Join(dir, "2026/11/app-05.log")
	require.NoError(t, os.Symlink("app-12.log", link))
	active := filepath.Join(dir, "2026/11/app-13.log")
	touchFile(t, active, cleanupNow)

	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(dir, "%Y", "%m", "app-%d.log"),
		MaxBackups: 1,
		Now:        func() time.Time { return cleanupNow },
	}, path: active}
	r.cleanup()

	_, err := os.Lstat(link)
	assert.True(t, os.IsNotExist(err), "跨目录 symlink 候选按目标 mtime 参与淘汰，删除的是链接本身")
	assert.FileExists(t, real, "目标文件必须幸存")
	assert.FileExists(t, active)
}

// TestCleanupMissingDirNoPanic 钉住快路径的 ReadDir 失败降级（§4.8）：
// pattern 目录不存在时 cleanup 仅警告并返回空候选集（与旧实现 Glob 对
// 不存在目录返回空匹配的净效果一致），不 panic、不误创建目录、不影响主流程。
func TestCleanupMissingDirNoPanic(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	r := &Rotator{cfg: Config{
		Pattern:    filepath.Join(missing, "app-%Y-%m-%d.log"),
		MaxAge:     1,
		MaxBackups: 5,
		Verbose:    true,
		Now:        func() time.Time { return cleanupNow },
	}, path: filepath.Join(missing, "app-2026-11-13.log")}
	r.cleanup() // 不得 panic
	assert.NoDirExists(t, missing, "cleanup 不得创建目录")
}

// TestCleanupBadPatternWarnsNoDelete 钉住坏模式（未闭合字符类）在双路径下
// 的降级语义：仅 Verbose 警告、按未命中处理、绝不误删任何文件。M5 已在
// 启动校验拒绝此类 pattern，本测试以白盒配置钉住运行期兜底行为
// （平面预检告警分支 + 跨目录 Glob 报错分支）。
func TestCleanupBadPatternWarnsNoDelete(t *testing.T) {
	t.Run("flat", func(t *testing.T) {
		dir := t.TempDir()
		touchFile(t, filepath.Join(dir, "app-x.log"), cleanupNow.AddDate(0, 0, -10))
		r := &Rotator{cfg: Config{
			Pattern:    filepath.Join(dir, "app-[.log"),
			MaxAge:     1,
			MaxBackups: 1,
			Verbose:    true,
			Now:        func() time.Time { return cleanupNow },
		}, path: filepath.Join(dir, "active.log")}
		r.cleanup()
		assert.FileExists(t, filepath.Join(dir, "app-x.log"), "坏模式不得命中任何文件")
	})
	t.Run("cronolog", func(t *testing.T) {
		dir := t.TempDir()
		mkdirAll(t, filepath.Join(dir, "2026", "11"))
		touchFile(t, filepath.Join(dir, "2026/11/app-x.log"), cleanupNow.AddDate(0, 0, -10))
		r := &Rotator{cfg: Config{
			Pattern:    filepath.Join(dir, "%Y", "%m", "app-[.log"),
			MaxAge:     1,
			MaxBackups: 1,
			Verbose:    true,
			Now:        func() time.Time { return cleanupNow },
		}, path: filepath.Join(dir, "2026/11/active.log")}
		r.cleanup()
		assert.FileExists(t, filepath.Join(dir, "2026/11/app-x.log"), "坏模式不得命中任何文件")
	})
}

// sortArchivesByName 返回按 name 升序排列的副本（C-V2 双路径等价性断言用）：
// glob 路径输出为逐层字典序、walk 路径为栈展开序，原始序不同；生产语义由
// sortByModTime 的 (mod, clamped, name) 全序决定（与候选输入序无关），等价
// 性断言因此按 name 排序后逐位对比。
func sortArchivesByName(files []archiveFile) []archiveFile {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b archiveFile) int { return strings.Compare(a.name, b.name) })
	return sorted
}

// cronologEquivSeed 在 base 下构建 C-V2 等价性测试的全形态日期树
// （pattern = base/%Y/static/%m/app-%d.log，comps = [*, static, *]）：
//   - 叶层：基础/.gz/.N/.N.gz 四变体、悬空 symlink（app-07，排除）、
//     symlink→文件（app-06，mtime 取目标）、symlink→目录（app-08，mtime 取
//     目标目录——与 glob 路径 Lstat→statFollow 语义一致地纳入候选）、与基名
//     变体同名的目录（app-09.log/，IsDir 排除）、活跃文件（app-13，精确
//     路径排除）、非匹配第三方文件（notes.txt/other.log）；
//   - 中间层：symlink→目录（2027→2026，镜像展开：同批文件以两条路径各成
//     候选，含活跃文件的镜像路径——排除集按精确路径比较，镜像不受豁免，
//     双路径一致）、symlink→文件（2028，不下降）、悬空 symlink（2029，不
//     下降）、普通文件占段（2030/README.txt，段位匹配但不下降）；
//   - 结构：空日期目录（static/02）、非匹配字面量段（dynamic/）、跨目录
//     同名归档（app-05.log 三处）、纯字面量目录段（static）与 token 段
//     （%Y/%m）混合、多段字面量根前缀（base/logs）；
//   - app-04.log.2.gz 钉未来 mtime（> now）锻炼 DA-2 钳制路径。
//
// 返回活跃文件路径、app-05.log 的 mtime（symlink app-06 目标值锚点）与
// 2020/static 目录的钉死 mtime（symlink app-08 目标值锚点）。
func cronologEquivSeed(t *testing.T, base string) (active string, app05Mtime, staticDirMtime time.Time) {
	t.Helper()
	mk := func(ps ...string) string {
		return filepath.Join(append([]string{base}, ps...)...)
	}
	mkdirAll(t, mk("2020", "static", "01"))
	mkdirAll(t, mk("2020", "static", "02")) // 空日期目录：走树访问但零候选
	mkdirAll(t, mk("2020", "static", "03"))
	mkdirAll(t, mk("2020", "dynamic"))                    // 非匹配字面量段：不下降
	mkdirAll(t, mk("2020", "static", "03", "app-09.log")) // 基名变体同名的目录：IsDir 排除
	mkdirAll(t, mk("2026", "static", "11"))
	app05Mtime = cleanupNow.Add(-5 * time.Hour)
	touchFile(t, mk("2020", "static", "01", "app-01.log"), cleanupNow.Add(-9*time.Hour))
	touchFile(t, mk("2020", "static", "01", "app-02.log.gz"), cleanupNow.Add(-8*time.Hour))
	touchFile(t, mk("2020", "static", "01", "app-03.log.7"), cleanupNow.Add(-7*time.Hour))
	touchFile(t, mk("2020", "static", "01", "app-04.log.2.gz"), cleanupNow.Add(2*time.Hour)) // 未来 mtime：DA-2 钳制
	touchFile(t, mk("2020", "static", "01", "notes.txt"), cleanupNow.Add(-48*time.Hour))     // 非匹配第三方
	touchFile(t, mk("2020", "static", "03", "app-05.log"), cleanupNow.Add(-6*time.Hour))
	touchFile(t, mk("2026", "static", "11", "app-05.log"), app05Mtime) // 跨目录同名
	// 叶层 symlink 三态。
	require.NoError(t, os.Symlink("app-05.log", mk("2026", "static", "11", "app-06.log")))
	require.NoError(t, os.Symlink("nonexistent-target", mk("2026", "static", "11", "app-07.log")))
	require.NoError(t, os.Symlink("../../../2020/static", mk("2026", "static", "11", "app-08.log")))
	touchFile(t, mk("2026", "static", "11", "other.log"), cleanupNow.Add(-48*time.Hour)) // 非匹配第三方
	active = mk("2026", "static", "11", "app-13.log")
	touchFile(t, active, cleanupNow)
	// 中间层四态。
	require.NoError(t, os.Symlink("2026", mk("2027")))                      // symlink→目录：镜像展开
	require.NoError(t, os.Symlink("2026/static/11/app-05.log", mk("2028"))) // symlink→文件：不下降
	require.NoError(t, os.Symlink("nonexistent", mk("2029")))               // 悬空：不下降
	touchFile(t, mk("2030"), cleanupNow.Add(-48*time.Hour))                 // 普通文件占段：不下降
	touchFile(t, mk("README.txt"), cleanupNow.Add(-48*time.Hour))           // 根层文件：段位匹配但不下降
	// 钉死 symlink app-08 的目标目录 mtime（目录 mtime 会随内容增删漂移且
	// 真实创建时刻晚于注入时钟——不钉死则恒走 DA-2 钳制路径）。
	staticDirMtime = cleanupNow.Add(-10 * time.Hour)
	require.NoError(t, os.Chtimes(mk("2020", "static"), staticDirMtime, staticDirMtime))
	return active, app05Mtime, staticDirMtime
}

// TestCleanupCronologWalkGlobEquivalence 钉住 C-V2（2026-11-16 性能优化）：
// cronolog 形态的生产分发自 3×filepath.Glob 回退路径（matchArchivesGlob）
// 切换为多目录单遍扫描（matchArchivesWalk）后，两路径在同一全形态日期树上
// 的输出必须逐位等价（排序后按 name/mod/clamped 逐元素对比）——
// matchArchivesGlob 保留为冻结 oracle（Unix 侧；Windows 上兼任生产路径，
// 见 cleanup.go 方案 B）。子测试覆盖三种排除配置（bare /
// M4 symlink 槽位 / P1-2 在途压缩对）、pruneEmptyDirs 空日期目录回收
// 回归断言（N3-B 行为不被 walk 破坏）与 root="." / root="/" 形态子测试
// （2026-11-16 审查 P3-3：相对路径 pattern 与绝对根形态——现有 cronolog
// 测试此前全部为绝对路径+字面前缀，两退化形态由本轮补齐）。
func TestCleanupCronologWalkGlobEquivalence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink on Windows requires Developer Mode or administrator privileges")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "logs")
	active, app05Mtime, staticDirMtime := cronologEquivSeed(t, base)
	pattern := filepath.Join(base, "%Y", "static", "%m", "app-%d.log")
	wantAll := []string{
		filepath.Join(base, "2020", "static", "01", "app-01.log"),
		filepath.Join(base, "2020", "static", "01", "app-02.log.gz"),
		filepath.Join(base, "2020", "static", "01", "app-03.log.7"),
		filepath.Join(base, "2020", "static", "01", "app-04.log.2.gz"),
		filepath.Join(base, "2020", "static", "03", "app-05.log"),
		filepath.Join(base, "2026", "static", "11", "app-05.log"),
		filepath.Join(base, "2026", "static", "11", "app-06.log"),
		filepath.Join(base, "2026", "static", "11", "app-08.log"),
		filepath.Join(base, "2027", "static", "11", "app-05.log"),
		filepath.Join(base, "2027", "static", "11", "app-06.log"),
		filepath.Join(base, "2027", "static", "11", "app-08.log"),
		filepath.Join(base, "2027", "static", "11", "app-13.log"), // 活跃文件的 symlink 镜像路径：排除集按精确路径比较，不受豁免
	}
	cases := []struct {
		name     string
		symlink  string
		rec      *compressRecord
		excluded []string
	}{
		{"bare", "", nil, nil},
		{"symlink-skip",
			filepath.Join(base, "2026", "static", "11", "app-06.log"), nil,
			[]string{filepath.Join(base, "2026", "static", "11", "app-06.log")}},
		{"in-flight", "", &compressRecord{
			src: filepath.Clean(filepath.Join(base, "2020", "static", "01", "app-01.log")),
			gz:  filepath.Clean(filepath.Join(base, "2020", "static", "01", "app-02.log.gz")),
		}, []string{
			filepath.Join(base, "2020", "static", "01", "app-01.log"),
			filepath.Join(base, "2020", "static", "01", "app-02.log.gz"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Rotator{cfg: Config{Pattern: pattern, Symlink: tc.symlink}, path: active}
			skipLink := r.symlinkAbs()
			globSide := r.matchArchivesGlob(filepath.Clean(active), skipLink, tc.rec, cleanupNow)
			walkSide := r.matchArchivesWalk(filepath.Clean(active), skipLink, tc.rec, cleanupNow)

			// 核心断言：排序后逐位对比（name/mod/clamped 全等——任何
			// 语义分歧在此显形）。
			g, w := sortArchivesByName(globSide), sortArchivesByName(walkSide)
			require.Equalf(t, len(g), len(w), "双路径候选数不一致（glob=%d walk=%d）", len(g), len(w))
			for i := range g {
				require.Equalf(t, g[i], w[i], "候选 %d（%s）双路径不等价", i, g[i].name)
			}

			// 形态锚点（防双路径同时漂移）：bare 期望集合扣除本例排除集。
			excluded := make(map[string]bool, len(tc.excluded))
			for _, p := range tc.excluded {
				excluded[p] = true
			}
			want := make([]string, 0, len(wantAll))
			for _, p := range wantAll {
				if !excluded[p] {
					want = append(want, p)
				}
			}
			got := make([]string, len(w))
			for i, af := range w {
				got[i] = af.name
			}
			require.Equal(t, want, got)

			// 语义 spot-check（walk 侧；oracle 等价断言已连带钉住 glob 侧）。
			// presence 守卫：被本例排除的候选不检查（排除语义已由 want
			// 集合断言钉死）。
			byName := make(map[string]archiveFile, len(w))
			for _, af := range w {
				byName[af.name] = af
			}
			if app04, ok := byName[filepath.Join(base, "2020", "static", "01", "app-04.log.2.gz")]; ok {
				// DA-2：未来 mtime 归档钳制为 now 且 clamped=true。
				require.True(t, app04.clamped, "未来 mtime 归档必须被钳制（DA-2）")
				require.True(t, app04.mod.Equal(cleanupNow), "钳制后 mtime 必须为 now（DA-2）")
			}
			if app06, ok := byName[filepath.Join(base, "2026", "static", "11", "app-06.log")]; ok {
				// symlink→文件：mtime 取跟随目标值。
				require.True(t, app06.mod.Equal(app05Mtime), "symlink→文件 候选 mtime 必须取目标文件值")
			}
			if app08, ok := byName[filepath.Join(base, "2026", "static", "11", "app-08.log")]; ok {
				// symlink→目录：mtime 取目标目录值（叶层候选语义与文件一致）。
				require.True(t, app08.mod.Equal(staticDirMtime), "symlink→目录 候选 mtime 必须取目标目录值")
				require.False(t, app08.clamped, "钉死的目标目录 mtime 早于 now，不应触发钳制")
			}
		})
	}

	// pruneEmptyDirs 回归（N3-B，验收判据 6）：删除后空日期目录链回收不被
	// walk 破坏——独立树，MaxAge 删除过期归档后 25→12→2025 整链回收。
	t.Run("prune-empty-dirs", func(t *testing.T) {
		pdir := t.TempDir()
		pbase := filepath.Join(pdir, "logs")
		expired := cleanupNow.AddDate(0, 0, -10)
		mkdirAll(t, filepath.Join(pbase, "2025", "12", "25"))
		touchFile(t, filepath.Join(pbase, "2025", "12", "25", "access.log"), expired)
		pactive := filepath.Join(pbase, "2026", "11", "13", "access.log")
		mkdirAll(t, filepath.Dir(pactive))
		touchFile(t, pactive, cleanupNow)

		r := &Rotator{cfg: Config{
			Pattern: filepath.Join(pbase, "%Y", "%m", "%d", "access.log"),
			MaxAge:  1,
			Now:     func() time.Time { return cleanupNow },
		}, path: pactive}
		r.cleanup()

		assert.NoFileExists(t, filepath.Join(pbase, "2025", "12", "25", "access.log"))
		assert.NoDirExists(t, filepath.Join(pbase, "2025"), "删除后空日期目录链 25→12→2025 必须回收（N3-B）")
		assert.DirExists(t, pbase)
		assert.FileExists(t, pactive)
	})

	// root="." 形态（2026-11-16 审查 P3-3）：相对路径 cronolog pattern（目录
	// 部分无字面量前缀）——splitWalkShape 退化为 root="."，与 Glob
	// cleanGlobPath("")→"." 特例同底座；双侧输出均为相对路径串，Join 清洗
	// （walk 侧 Join(".", seg) 与 Glob 侧 Join(dir, name)）必须对称归一、
	// 逐位一致。t.Chdir 使相对 pattern 以临时目录为 CWD 底座（包内测试零
	// t.Parallel，进程级 chdir 安全，子测试结束自动恢复）。
	t.Run("root-dot-relative-pattern", func(t *testing.T) {
		t.Chdir(t.TempDir())
		root, comps := splitWalkShape("%Y/static/%m/app-%d.log")
		require.Equal(t, ".", root)
		require.Equal(t, []string{"*", "static", "*"}, comps)

		mkdirAll(t, filepath.Join("2020", "static", "01"))
		mkdirAll(t, filepath.Join("2020", "static", "03"))
		mkdirAll(t, filepath.Join("2026", "static", "11"))
		touchFile(t, filepath.Join("2020", "static", "01", "app-01.log"), cleanupNow.Add(-9*time.Hour))
		touchFile(t, filepath.Join("2020", "static", "01", "app-02.log.gz"), cleanupNow.Add(-8*time.Hour))
		touchFile(t, filepath.Join("2020", "static", "03", "app-05.log"), cleanupNow.Add(-6*time.Hour))
		touchFile(t, filepath.Join("2026", "static", "11", "app-05.log"), cleanupNow.Add(-5*time.Hour))
		active := filepath.Join("2026", "static", "11", "app-13.log")
		touchFile(t, active, cleanupNow)

		r := &Rotator{cfg: Config{Pattern: "%Y/static/%m/app-%d.log"}, path: active}
		globSide := r.matchArchivesGlob(filepath.Clean(active), "", nil, cleanupNow)
		walkSide := r.matchArchivesWalk(filepath.Clean(active), "", nil, cleanupNow)
		g, w := sortArchivesByName(globSide), sortArchivesByName(walkSide)
		require.Equalf(t, len(g), len(w), "双路径候选数不一致（glob=%d walk=%d）", len(g), len(w))
		for i := range g {
			require.Equalf(t, g[i], w[i], "候选 %d（%s）双路径不等价", i, g[i].name)
		}
		want := []string{
			filepath.Join("2020", "static", "01", "app-01.log"),
			filepath.Join("2020", "static", "01", "app-02.log.gz"),
			filepath.Join("2020", "static", "03", "app-05.log"),
			filepath.Join("2026", "static", "11", "app-05.log"),
		}
		got := make([]string, len(w))
		for i, af := range w {
			got[i] = af.name
		}
		require.Equal(t, want, got) // 活跃文件按相对路径精确排除（app-13.log 不在候选内）
	})

	// root="/" 形态（绝对 pattern 的首个目录段即含 token，字面量前缀为空）
	// → splitWalkShape 退化为 root="/"，与 Glob cleanGlobPath("/")→"/" 特例
	// 同底座。comp[0] 取唯一字面量+token 段（真实根目录下不存在匹配条目）：
	// 确定性零候选下钉住 root="/" 的 open+Readdir 路径形态与双侧一致（不深
	// 扫根目录、不依赖机器状态；splitWalkShape 断言直接钉住该分支的形状
	// 解析，防退化为 ""/"" 等畸形根）。
	t.Run("root-slash-absolute-form", func(t *testing.T) {
		pattern := "/zz-rotatelogs-equiv-%Y/static/%m/app-%d.log"
		root, comps := splitWalkShape(pattern)
		require.Equal(t, "/", root)
		require.Equal(t, []string{"zz-rotatelogs-equiv-*", "static", "*"}, comps)

		r := &Rotator{cfg: Config{Pattern: pattern}}
		globSide := r.matchArchivesGlob("", "", nil, cleanupNow)
		walkSide := r.matchArchivesWalk("", "", nil, cleanupNow)
		require.Equal(t, sortArchivesByName(globSide), sortArchivesByName(walkSide), "root=\"/\" 形态双路径输出必须逐位一致")
		require.Empty(t, globSide)
		require.Empty(t, walkSide)
	})
}

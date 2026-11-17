package rotator

import (
	"compress/gzip"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gunzipFile 解压 .gz 文件全部内容（§14.1-7：压缩产物必须可 gunzip 还原）。
func gunzipFile(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()
	data, err := io.ReadAll(gz)
	require.NoError(t, err)
	return data
}

// TestCompressAsyncNormal 验证正常压缩：产物 gunzip 还原与原文一致、
// 压缩成功后源文件被删除（§4.4/§14.1-7）。
func TestCompressAsyncNormal(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "app.log")
	data := []byte("compress me \x00\xff binary tail")
	require.NoError(t, os.WriteFile(old, data, filePerm))

	r := &Rotator{}
	r.compressAsync(old)
	r.compressWg.Wait()

	_, statErr := os.Stat(old)
	assert.True(t, os.IsNotExist(statErr), "source must be removed after successful compression")
	assert.Equal(t, data, gunzipFile(t, old+".gz"))
	assert.False(t, r.compressBusy.Load(), "busy flag must reset after completion")
}

// TestCompressSkipWhenBusy 验证 skip-if-busy（§4.4：上次压缩未完成时跳过、不排队）。
// 白盒直置 compressBusy 保证确定性，无时序 flake。
func TestCompressSkipWhenBusy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), filePerm))

	r := &Rotator{}
	r.compressBusy.Store(true)
	r.compressAsync(path)
	r.compressWg.Wait() // 未启动 goroutine，立即返回

	_, statErr := os.Stat(path + ".gz")
	assert.True(t, os.IsNotExist(statErr), "must not compress when busy")
	assert.Equal(t, []byte("data"), readFile(t, path))
	assert.True(t, r.compressBusy.Load(), "skip path must not reset the busy flag")
}

// TestCompressMissingFileNoPanic 验证压缩不存在的文件：goroutine 不 panic、
// 不留残缺 .gz、busy 标志复位，失败被降级为警告。
func TestCompressMissingFileNoPanic(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nonexistent.log")

	r := &Rotator{}
	r.compressAsync(missing)
	r.compressWg.Wait()

	assert.False(t, r.compressBusy.Load())
	_, statErr := os.Stat(missing + ".gz")
	assert.True(t, os.IsNotExist(statErr), "no partial .gz for missing source")
}

// TestCompressBusyResetAfterFailure 验证失败后压缩能力可恢复：
// busy 标志经 defer 复位，下一次切分的压缩正常执行。
func TestCompressBusyResetAfterFailure(t *testing.T) {
	dir := t.TempDir()
	r := &Rotator{}
	r.compressAsync(filepath.Join(dir, "nonexistent.log"))
	r.compressWg.Wait()
	require.False(t, r.compressBusy.Load(), "busy flag must reset after failure")

	path := filepath.Join(dir, "app.log")
	data := []byte("second attempt")
	require.NoError(t, os.WriteFile(path, data, filePerm))
	r.compressAsync(path)
	r.compressWg.Wait()
	assert.Equal(t, data, gunzipFile(t, path+".gz"))
}

// TestCloseWaitsCompression 验证 §6.1：Close 必须等待异步压缩 goroutine 完成——
// 返回时 5MB 旧文件的 .gz 已完整可还原且源文件已删除。
func TestCloseWaitsCompression(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 5<<20)
	rand.New(rand.NewSource(42)).Read(data) // 不可压缩随机数据，确保压缩耗时可观测

	r, err := New(Config{
		Pattern:  filepath.Join(dir, "app.log"),
		MaxSize:  testMaxSize,
		Compress: true,
		Now:      func() time.Time { return cleanupNow },
	})
	require.NoError(t, err)

	_, err = r.Write(data)
	require.NoError(t, err)

	r.RotateFlag.Store(true)
	_, err = r.Write([]byte("tail"))
	require.NoError(t, err)

	require.NoError(t, r.Close())

	old := filepath.Join(dir, "app.log")
	_, statErr := os.Stat(old)
	assert.True(t, os.IsNotExist(statErr), "Close must not return before compression finished")
	assert.Equal(t, data, gunzipFile(t, old+".gz"))
	assert.Equal(t, []byte("tail"), readFile(t, filepath.Join(dir, "app.log.1")))
}

// TestCompressNeverTouchesActiveFile 验证 §4.4：禁止压缩当前活跃文件——
// 未发生切分时 Compress=true 也不产生任何 .gz，活跃文件保持明文。
func TestCompressNeverTouchesActiveFile(t *testing.T) {
	dir := t.TempDir()
	r, err := New(Config{
		Pattern:  filepath.Join(dir, "app.log"),
		MaxSize:  testMaxSize,
		Compress: true,
		Now:      func() time.Time { return cleanupNow },
	})
	require.NoError(t, err)

	_, err = r.Write([]byte("active data"))
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, []byte("active data"), readFile(t, filepath.Join(dir, "app.log")))
	_, statErr := os.Stat(filepath.Join(dir, "app.log.gz"))
	assert.True(t, os.IsNotExist(statErr), "active file must never be compressed")
}

// writeGzFile 写入真实 gzip 内容的 .gz 文件（构造既有压缩归档）。
func writeGzFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	_, err = gz.Write(data)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
}

// TestCompressDoesNotOverwriteExistingGz 钉住固定文件名+compress 合法组合
// （addendum）下的名字重用场景：压缩删除源文件后，下一次切分会重用同名，
// 再次压缩时 path.gz 已存在——绝不允许静默截断覆盖旧归档（§1.4 可靠原则），
// 必须按序号探测新产物名（.N.gz 形式可被 D4 清理变体匹配）。
func TestCompressDoesNotOverwriteExistingGz(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// 既有归档 app.log.gz（上一轮压缩产物）
	writeGzFile(t, path+".gz", []byte("old archive"))
	// 同名源文件再次出现（名字被重用）
	require.NoError(t, os.WriteFile(path, []byte("new data"), filePerm))

	r := &Rotator{}
	r.compressAsync(path)
	r.compressWg.Wait()

	assert.Equal(t, []byte("old archive"), gunzipFile(t, path+".gz"),
		"existing .gz archive must never be overwritten")
	assert.Equal(t, []byte("new data"), gunzipFile(t, path+".1.gz"),
		"collision must fall back to a serial .N.gz name")
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "source must be removed after success")
}

// TestCompressAsyncNameResolutionFailureRecovers 钉住定名阶段失败降级契约
// （NEW-P1-1 同步定名）：createGzFile 在无写权限目录下无法创建产物时，
// compressAsync 必须恢复 busy（压缩能力可恢复）、不发布在途记录、不留任何
// .gz、源文件完好，失败仅降级为警告（§4.8），不启动流式 goroutine。
func TestCompressAsyncNameResolutionFailureRecovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based write denial is not portable to Windows")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(sub, dirPerm))
	srcPath := filepath.Join(sub, "app.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), filePerm))
	require.NoError(t, os.Chmod(sub, 0o555)) // 只读目录：os.Open 可读取，O_CREATE 必失败
	t.Cleanup(func() { _ = os.Chmod(sub, dirPerm) })

	r := &Rotator{}
	r.compressAsync(srcPath)
	r.compressWg.Wait()

	assert.False(t, r.compressBusy.Load(), "busy must reset after name-resolution failure")
	assert.Nil(t, r.compressing.Load(), "failed name resolution must not publish a record")
	_, statErr := os.Stat(srcPath + gzSuffix)
	assert.True(t, os.IsNotExist(statErr), "no .gz when the target name cannot be created")
	assert.Equal(t, []byte("data"), readFile(t, srcPath), "source must stay intact")
}

// TestCompressStreamFailureRemovesPartialGz 钉住 compressStream 失败清理契约：
// 流式中途任一环节失败必须删除残缺 .gz（避免泄漏半截产物）、保留源文件
// 供下次压缩重试，并返回 error 供调用方降级为警告。白盒直调 + 已关闭 dst
// 构造确定性写失败，零时序依赖。
func TestCompressStreamFailureRemovesPartialGz(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), filePerm))
	dst, err := createGzFile(srcPath)
	require.NoError(t, err)
	require.NoError(t, dst.Close()) // 确定性构造流式写失败：dst 已关闭
	src, err := os.Open(srcPath)
	require.NoError(t, err)
	defer src.Close()

	err = compressStream(srcPath, src, dst)
	require.Error(t, err)
	_, statErr := os.Stat(srcPath + gzSuffix)
	assert.True(t, os.IsNotExist(statErr), "partial .gz must be removed on failure")
	assert.Equal(t, []byte("data"), readFile(t, srcPath), "source must survive for retry")
}

// TestCompressPanicRemovesPartialGz 钉住 panic 路径清理契约（2026-11-16 审计
// P2-B）：压缩流式中途 panic（io.Copy/gzip 内部异常）时，compressStream 的
// 错误路径清理被跳过，goroutine 的 recover defer 必须补 best-effort 清理
// 半成品 .gz——半截产物以正常归档名残留会成为清理候选（损坏的 gzip 流）。
// 经 ioCopy 注入点构造确定性 panic（零时序依赖）；断言：.gz 无残留、
// 源文件完好、busy/compressing 复位（压缩能力可恢复）。
func TestCompressPanicRemovesPartialGz(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), filePerm))

	old := ioCopy
	ioCopy = func(dst io.Writer, src io.Reader) (int64, error) {
		panic("injected compress panic")
	}
	defer func() { ioCopy = old }()

	r := &Rotator{}
	r.compressAsync(path)
	r.compressWg.Wait()

	_, statErr := os.Stat(path + gzSuffix)
	assert.True(t, os.IsNotExist(statErr), "panic path must remove the partial .gz")
	assert.Equal(t, []byte("data"), readFile(t, path), "source must survive for retry")
	assert.False(t, r.compressBusy.Load(), "busy flag must reset after panic recovery")
	assert.Nil(t, r.compressing.Load(), "in-flight record must be cleared after panic recovery")
}

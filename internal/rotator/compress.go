package rotator

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// compressRecord 是压缩在途记录的不可变小结构（NEW-P1-1）：src 为在途压缩
// 源文件，gz 为 createGzFile 确定的精确 .gz 目标名（序号探测结果，可为
// src.gz 或 src.N.gz）。经 atomic.Pointer 以 copy-on-write 语义整体发布：
// 每次 Store 均为新对象、发布后字段不再改写，读端（cleanup）无锁安全。
type compressRecord struct {
	src string
	gz  string
}

// ioCopy 是 io.Copy 的包级注入点（P2-B 测试）：生产恒等 io.Copy，仅测试
// 经替换注入 panic，钉住压缩中途不可恢复异常时半成品 .gz 的清理契约。
// 并发边界（2026-11-16 审查 P3-2）：包级 var 的测试替换依赖同包测试默认
// 串行——全仓测试零 t.Parallel()（internal/ 与 cmd/ 均无并行测试），
// swap→触发→restore 的窗口内不存在并发读取；未来引入并行测试时必须
// 约束本注入点的替换窗口（如 swap 系列专用串行门控），否则 swap 写入与
// 压缩 goroutine 的并发读取构成数据竞争。
var ioCopy = io.Copy

// compressAsync 异步压缩已切分的旧文件（§4.4）：skip-if-busy——已有压缩在途时
// 直接跳过本次、不排队，同一时刻最多 1 个压缩 goroutine（compressBusy CAS，D5）。
// 仅由 rotate 对已关闭的旧文件调用，天然不压缩当前活跃文件；
// 任何失败/panic 仅 Verbose 报告，禁止影响主写入流程（§4.8/§11）。
//
// 产物定名同步完成（NEW-P1-1）：打开源文件与 createGzFile 的 O_EXCL 序号探测
// 均为快速元数据操作，在启动流式 goroutine 前于本函数内执行，使精确
// {src, gz} 记录先于 goroutine 发布。由此 cleanup（rotate 步骤 9）读到的
// 在途记录恒为精确排除集，无需按 ".N.gz" 名字家族猜测豁免——家族豁免会
// 连带保护已完成的历史归档，固定名+压缩+MaxBackups 模式下 cleanup 永久
// 空转、归档数无界增长（违反 §14.1-5"最多 N 个"）。
func (r *Rotator) compressAsync(path string) {
	if !r.compressBusy.CompareAndSwap(false, true) {
		r.warnf("compress busy, skipping %s", path)
		return
	}
	src, dst, err := openCompressPair(path)
	if err != nil {
		r.compressBusy.Store(false)
		r.warnf("compress %s: %v", path, err)
		return
	}
	// 在途记录先于 goroutine 启动写入：rotate 内步骤 8→9 同 goroutine 顺序执行，
	// cleanup 读到的一定是完整精确状态（跳过 skip 路径——busy 时既有记录
	// 属于在途压缩，不得覆盖）。
	r.compressing.Store(&compressRecord{
		src: filepath.Clean(path),
		gz:  filepath.Clean(dst.Name()),
	})
	r.compressWg.Go(func() {
		defer r.compressBusy.Store(false)
		defer r.compressing.Store(nil)
		defer func() {
			if p := recover(); p != nil {
				// P2-B（2026-11-16 审计）：panic 路径兜底清理半成品 .gz——
				// compressStream 的错误路径清理（os.Remove(dst.Name())）被
				// panic 跳过，半截产物以正常归档名残留（损坏的 gzip 流成为
				// 清理候选）。此刻 dst 已由 defer 链先行 Close，按路径删除
				// 不受影响；best-effort：Remove 失败仅追加警告，不掩盖
				// panic 消息。成功路径不走本分支，完整产物不动。
				if rmErr := os.Remove(dst.Name()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
					r.warnf("compress panic cleanup %s: %v", dst.Name(), rmErr)
				}
				r.warnf("compress panic %s: %v", path, p)
			}
		}()
		defer src.Close()
		// OBS-N2：panic 路径兜底关闭 dst，避免 io.Copy/gz.Close 区间 panic 时
		// dst fd 泄漏。幂等——正常路径 compressStream 体内已 Close(dst)，此处重复
		// Close 返回 os.ErrClosed，刻意忽略（不传播、不 panic）。
		defer dst.Close()
		if err := compressStream(path, src, dst); err != nil {
			r.warnf("compress %s: %v", path, err)
		}
	})
}

// openCompressPair 打开源文件并以 createGzFile 原子创建不冲突的 .gz 产物
// （定名阶段，无数据 I/O）。任一环节失败时清理已打开句柄、不留半成品 .gz。
func openCompressPair(path string) (src, dst *os.File, err error) {
	src, err = os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	dst, err = createGzFile(path)
	if err != nil {
		src.Close()
		return nil, nil, err
	}
	return src, dst, nil
}

// compressStream 将 src 流式 gzip 写入已定名的 dst（固定内存，与文件大小
// 无关），全部成功后删除源文件 path；任一环节失败删除残缺 .gz 后返回 error。
func compressStream(path string, src, dst *os.File) error {
	gz := gzip.NewWriter(dst)
	_, err := ioCopy(gz, src)
	if err == nil {
		err = gz.Close()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst.Name()) // 尽力清除残缺产物，避免泄漏半截 .gz
		return err
	}
	return os.Remove(path)
}

// createGzFile 以 O_CREATE|O_EXCL 原子创建不冲突的 .gz 产物：path.gz 被占用
// （固定文件名模式下压缩删源后名字被重用）时按序号探测 path.1.gz、path.2.gz…，
// 绝不静默覆盖既有归档；".N.gz" 形式可被清理的 D4 ".[0-9]*" 变体匹配。
func createGzFile(path string) (*os.File, error) {
	for i := 0; ; i++ {
		candidate := path + gzSuffix
		if i > 0 {
			candidate = fmt.Sprintf("%s%s%d%s", path, seqSeparator, i, gzSuffix)
		}
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf(MsgPrefix+"create %s: %w", candidate, err)
		}
	}
}

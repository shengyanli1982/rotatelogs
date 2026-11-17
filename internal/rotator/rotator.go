// Package rotator 实现按时间/大小/信号驱动的日志轮转写入器（io.WriteCloser）：
// strftime 文件名生成（§3.3）、冲突序号追加（§4.2）、机会式清理（§4.3）、
// 异步 gzip 压缩（§4.4）与 symlink 维护（§4.5）。
//
// 并发契约（2026-11-16 心跳 idle flush 变更，DESIGN §4.1 addendum）：
// Write/Close/Flush 由 mu 保护，可并发调用。并发参与者：心跳 flusher
// goroutine（mu 下触碰 buf，law 式双时钟 idle flush）、异步压缩 goroutine
// （compressWg/compressBusy/compressing 同步，不触碰写入路径数据结构）与
// 写入 RotateFlag 的 SIGHUP 信号 goroutine（atomic）。写入生产者仍为单一
// （cmd 层 pump 读 stdin）：内核管道缓冲已是阶段 1 解耦层，不引入 law 的
// MPSC/BufferPool（单生产者下冗余，且异步队列洪峰下无界增长 → OOM），
// 只借其心跳 idle flush 策略；mu 仅防低频 flusher 竞争，开销可忽略。
package rotator

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 文件与目录创建权限（实现选型，DESIGN 未记载、非契约值）：filePerm 0644
// 是日志文件与其 .gz 产物的创建权限（New/uniqueName/createGzFile 三处
// O_CREATE 共用，跨 rotator.go 与 compress.go 必须一致）；dirPerm 0755 是
// MkdirAll 自动创建父目录的权限（New 与 rotate 共用）。
const (
	filePerm = 0644
	dirPerm  = 0755
)

// Rotator 实现 io.WriteCloser，管理日志文件的写入和轮转。
// 并发安全性：Write/Close/Flush 由 mu 保护，可并发调用（2026-11-16 心跳
// idle flush 变更，见包 doc 与 DESIGN §4.1 addendum）。
// Rotate 可以从信号处理 goroutine 通过 atomic flag 间接触发。
type Rotator struct {
	cfg Config
	// mu 保护 buf/file/size/path/closed/nextRotate 与 flush 双时钟。
	// Write 全程持锁（rotate 在持锁下调用，内部用 flushLocked，禁止重复加锁）；
	// 心跳 flusher 每 FlushHeartbeatInterval 短暂持锁。单生产者 + 低频 flusher，
	// 锁竞争可忽略；mutex 不分配，Write 保持 0-alloc（r3 防回归基线）。
	mu           sync.Mutex
	path         string        // 当前活跃文件路径（D5，供 symlink/cleanup/compress 消费）
	file         *os.File      // 当前活跃文件
	buf          *bufio.Writer // 写缓冲
	size         int64         // 当前文件已写入字节数
	nextRotate   time.Time     // 下一个时间切分点；RotationTime > 24h 时兼作周期递推锚点（DA-1）
	executeAt    time.Time     // 静默时钟：最后写入活动时间（Write 刷新，law 式）
	lastFlushAt  time.Time     // 饥饿上界时钟：最后成功 flush 时间（连续写入不得无限推迟 flush）
	RotateFlag   atomic.Bool   // SIGHUP 设置的强制切分标志（导出，供 cmd 层信号处理写入）
	compressBusy atomic.Bool   // 压缩并发标记（D5，skip-if-busy，ST-03 使用）
	// compressing 记录压缩在途的源文件与其精确 .gz 目标名（nil = 无在途压缩）。
	// compressAsync 在 CAS 成功、产物定名（createGzFile）后、goroutine 启动前
	// 同步发布不可变 *compressRecord（NEW-P1-1），goroutine 退出时清空；
	// cleanup 经单快照消费，精确排除 {src, gz} 两元素并预留一个保留名额，
	// 消除 §4.2 步骤 8（压缩）与步骤 9（清理）的同轮交错窗口（P1-2）。
	compressing atomic.Pointer[compressRecord]
	compressWg  sync.WaitGroup
	closed      bool
	// flusher 生命周期：stopCh 由 Close 经 stopOnce 关闭（幂等，防重复 close
	// panic）；flusherWg 同步等待 flusher 退出。stopFlusher 不得持 mu——flusher
	// 可能正在等 mu，持 mu 等待会死锁（Close 生命周期步骤 b）。
	stopCh    chan struct{}
	flusherWg sync.WaitGroup
	stopOnce  sync.Once
}

// New 创建 Rotator 并打开初始日志文件。
// 第一步执行 cfg.Validate()（单一校验闸门，Addendum 2 §3.3），随后做依赖
// 时间计算的 M3a 检查；两者失败均返回 ErrInvalidConfig 包装且先于任何
// 文件操作。初始文件无法创建时返回运行期 error。
// 初始文件以追加方式打开（D3），保证进程重启幂等，不会重启即产生 .1 文件。
func New(cfg Config) (*Rotator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	r := &Rotator{cfg: cfg}
	name := r.formatName(r.now())
	if err := checkSymlinkDistinct(cfg.Symlink, name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(name), dirPerm); err != nil {
		return nil, fmt.Errorf(MsgPrefix+"mkdir %s: %w", filepath.Dir(name), err)
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		return nil, fmt.Errorf(MsgPrefix+"open %s: %w", name, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf(MsgPrefix+"stat %s: %w", name, err)
	}

	r.file = f
	r.path = name
	r.buf = bufio.NewWriterSize(f, WriteBufferSize)
	r.size = st.Size()
	r.nextRotate = r.boundaryAfter(r.now())
	// 双时钟初始化：启动时缓冲为空（Buffered()==0 守卫兜底），时钟置为当前
	// 时间，避免零值时间使首次心跳的差值恒超阈值。
	r.executeAt, r.lastFlushAt = r.now(), r.now()
	// §4.5: 启动时创建/更新 symlink 指向当前文件；失败仅 verbose 警告，
	// 禁止阻断启动（§4.8）。Windows 降级提示经 symlinkPlatformNote 输出一次。
	if cfg.Symlink != "" {
		if cfg.Verbose && symlinkPlatformNote != "" {
			fmt.Fprintln(os.Stderr, symlinkPlatformNote)
		}
		if err := updateSymlink(r.path, cfg.Symlink); err != nil {
			r.warnf("symlink %s -> %s: %v", cfg.Symlink, r.path, err)
		}
	}
	// 心跳 flusher 仅在成功路径末尾启动（上方错误路径全部提前 return，
	// 不启动 goroutine，防泄漏）。
	r.stopCh = make(chan struct{})
	r.flusherWg.Add(1)
	go r.flushLoop()
	return r, nil
}

// Write 实现 io.Writer。写入前检查切分条件（时间/大小/RotateFlag，§4.2 OR 逻辑）。
// 单次 Write 跨越 MaxSize 边界时按边界分片（D1）：写满当前文件 → rotate → 继续写
// 剩余部分（可跨多个文件），保证每文件 ≤ MaxSize 且字节内容与顺序不变。
// 全程持 mu（与 Close/Flush/心跳 flusher 并发安全）；rotate 在持锁下调用。
func (r *Rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, errClosed
	}
	// 静默时钟：每次 Write 刷新最后写入活动（law 式）。mutex/time.Now 均不
	// 分配，Write 保持 0-alloc（r3 防回归基线）。
	r.executeAt = r.now()

	written := 0
	for len(p) > 0 {
		if r.needRotate() {
			if err := r.rotate(); err != nil {
				return written, err
			}
		}
		chunk := p
		if r.cfg.MaxSize > 0 && r.size+int64(len(p)) > r.cfg.MaxSize {
			// D1: 只写满当前文件剩余容量，下一轮循环经 needRotate 切分后续写。
			chunk = p[:r.cfg.MaxSize-r.size]
		}
		n, err := r.buf.Write(chunk)
		r.size += int64(n)
		written += n
		p = p[n:]
		if err != nil {
			return written, fmt.Errorf(MsgPrefix+"write %s: %w", r.path, err)
		}
	}
	return written, nil
}

// Close 实现 io.Closer。flush 缓冲区、关闭文件、best-effort 刷新 symlink、
// 等待压缩完成。Close 幂等：重复调用返回 nil。
// 生命周期严格顺序（防死锁，DESIGN §4.1 addendum）：a. 锁内置 closed（并发
// Write 立即 errClosed）→ b. stopFlusher（不持 mu）→ c. 锁内终刷 + 关文件
// → d. symlink best-effort + compressWg.Wait + 错误合并。
func (r *Rotator) Close() error {
	// a. 早置 closed：并发 Write 在锁内立即看到 errClosed，不再触碰缓冲。
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	// b. 停心跳 flusher：此步不得持 mu——flusher 可能正阻塞在 mu.Lock，
	// 持 mu 等待会死锁。flusher 见 stopCh 退出，退出时不持 mu。白盒构造的
	// Rotator（未经 New）stopCh==nil、无 flusher，跳过。
	if r.stopCh != nil {
		r.stopOnce.Do(func() { close(r.stopCh) })
		r.flusherWg.Wait()
	}

	// c. 终刷 + 关文件（锁内；flushLocked 不重复加锁）。
	r.mu.Lock()
	flushErr := r.flushLocked()
	closeErr := r.file.Close()
	r.mu.Unlock()
	// §4.7 关停序列步骤 3：best-effort 更新 symlink——最后一次更新因瞬态
	// 失败残留错误指向时，这里是自愈机会；失败仅 verbose 警告，
	// 不影响 Close 返回值（§4.5 symlink 失败可容忍）。
	if r.cfg.Symlink != "" {
		if err := updateSymlink(r.path, r.cfg.Symlink); err != nil {
			r.warnf("symlink %s -> %s: %v", r.cfg.Symlink, r.path, err)
		}
	}
	// §6.1: 等待异步压缩 goroutine 完成后再返回，保证 Close 后 .gz 产物完整。
	r.compressWg.Wait()

	switch {
	case flushErr != nil && closeErr != nil:
		// 双失败合并为单行：errors.Join 以 \n 连接会产生两行 stderr，
		// 违反"恰一行"输出契约（裁定 4）；多 %w 保持两条错误链均可
		// errors.Is 穿透。
		return fmt.Errorf(MsgPrefix+"flush %s: %w (close: %w)", r.path, flushErr, closeErr)
	case flushErr != nil:
		return fmt.Errorf(MsgPrefix+"flush %s: %w", r.path, flushErr)
	case closeErr != nil:
		return fmt.Errorf(MsgPrefix+"close %s: %w", r.path, closeErr)
	default:
		return nil
	}
}

// flushLocked 将缓冲刷入文件，成功后刷新 lastFlushAt（饥饿上界时钟）。
// 假定调用方持 mu；供 rotate 步骤 1、Close 终刷、心跳与 Flush 复用。
// 失败不更新时钟（law 同款）；bufio sticky error 由下一次 Write surfaced。
func (r *Rotator) flushLocked() error {
	if err := r.buf.Flush(); err != nil {
		return err
	}
	r.lastFlushAt = r.now()
	return nil
}

// maybeFlushIdleLocked 执行 law 式双时钟 idle 判定（假定调用方持 mu）：
// 缓冲非空且（静默期已到 OR flush 被饥饿）→ flush。executeAt 管最后写入
// 活动（静默期），lastFlushAt 管最后成功 flush（连续写入不得无限推迟 flush，
// 饥饿上界 = FlushIdleTimeout）。成功后双时钟重置为 now；失败不更新时钟。
// 空缓冲（Buffered()==0）no-op：不 flush、不重置时钟。
func (r *Rotator) maybeFlushIdleLocked(now time.Time) error {
	if r.buf.Buffered() == 0 {
		return nil
	}
	if now.Sub(r.executeAt) < FlushIdleTimeout && now.Sub(r.lastFlushAt) < FlushIdleTimeout {
		return nil
	}
	if err := r.flushLocked(); err != nil {
		return err
	}
	r.executeAt, r.lastFlushAt = now, now
	return nil
}

// Flush 立即将缓冲数据刷入文件（导出，mu 保护，与 Write/Close 并发安全）。
// 时间驱动可见性由心跳 flusher 承担（上界 FlushIdleTimeout）；本方法供需要
// 显式 flush 点的调用方使用。
func (r *Rotator) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.flushLocked(); err != nil {
		return fmt.Errorf(MsgPrefix+"flush %s: %w", r.path, err)
	}
	return nil
}

// flushLoop 是心跳 flusher 主循环（New 成功路径末尾启动，law 式心跳策略）：
// 每 FlushHeartbeatInterval 醒来一次，在 mu 下做双时钟 idle 判定。flush 失败
// 仅 verbose 警告并继续循环——bufio sticky error 由下一次 Write surfaced 传播
// 为 exitRuntime，禁止在 flusher 里 crash；失败不更新时钟，下次心跳原时钟重试。
func (r *Rotator) flushLoop() {
	defer r.flusherWg.Done()
	ticker := time.NewTicker(FlushHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()
			if !r.closed {
				if err := r.maybeFlushIdleLocked(r.now()); err != nil {
					r.warnf("idle flush %s: %v", r.path, err)
				}
			}
			r.mu.Unlock()
		}
	}
}

// warnf 在 Verbose 模式下输出附属操作（清理/压缩/symlink）的降级警告到 stderr。
// §4.8：此类失败仅警告、继续运行，禁止影响主写入流程。
func (r *Rotator) warnf(format string, args ...any) {
	if r.cfg.Verbose {
		fmt.Fprintf(os.Stderr, MsgPrefix+format+"\n", args...)
	}
}

// needRotate 报告任一触发条件是否满足（§4.2 OR 逻辑）。
func (r *Rotator) needRotate() bool {
	if r.cfg.RotationTime > 0 && !r.now().Before(r.nextRotate) {
		return true
	}
	if r.cfg.MaxSize > 0 && r.size >= r.cfg.MaxSize {
		return true
	}
	return r.RotateFlag.Load()
}

// rotate 执行切分：关闭旧文件、打开新文件、触发清理和压缩。
// 私有方法，由 Write 内部在条件满足时调用（调用方须持 mu，本方法不加锁）。
// 严格按 §4.2 步骤 1-10 顺序执行；
// 步骤 7-9（symlink/compress/cleanup）为附属操作，失败仅 verbose 警告，
// 禁止传播进 Write 返回值（§4.8）。
func (r *Rotator) rotate() error {
	// 消费 SIGHUP 标志：CAS(true,false) 严格对应"触发本次切分的标志"；
	// rotate 执行窗口内到达的 HUP 得以保留给下一次 Write（尾部无条件
	// Store(false) 会将其吞掉，窗口语义不严格）。
	r.RotateFlag.CompareAndSwap(true, false)
	// 步骤 1: Flush bufio.Writer 到文件（持锁下调用 flushLocked，不重复加锁；
	// 成功刷新 lastFlushAt——切分也是一次成功 flush 事件）。
	if err := r.flushLocked(); err != nil {
		return fmt.Errorf(MsgPrefix+"flush %s: %w", r.path, err)
	}
	oldPath := r.path
	// 步骤 2: 关闭当前文件句柄。
	if err := r.file.Close(); err != nil {
		return fmt.Errorf(MsgPrefix+"close %s: %w", oldPath, err)
	}
	// 步骤 3: 基于当前时间 + pattern 生成新文件名。
	name := r.formatName(r.now())
	// 步骤 5 先于步骤 4/6 执行：时间格式化目录（如 /%Y/%m/）跨期时是新路径，
	// O_EXCL 冲突探测要求父目录已存在。
	if err := os.MkdirAll(filepath.Dir(name), dirPerm); err != nil {
		return fmt.Errorf(MsgPrefix+"mkdir %s: %w", filepath.Dir(name), err)
	}
	// 步骤 4 + 6: 冲突追加序号探测并创建新文件，初始化新 bufio.Writer。
	f, err := uniqueName(name)
	if err != nil {
		return err
	}
	r.file = f
	// 步骤 6（C3，2026-11-14 性能优化）：复用既有 bufio.Writer 缓冲——步骤 1
	// 已 Flush，Reset 丢弃（空的）未刷数据并改指新文件，bufio 文档保证与
	// NewWriterSize 新建等价，消除每次 rotate 的 64KB 缓冲分配+清零/页错误。
	// 缓冲尺寸不变（New() 首建时以 NewWriterSize(f, WriteBufferSize) 分配，
	// buf 为 nil 时不可 Reset——初始化路径保持 NewWriterSize）。
	r.buf.Reset(f)
	r.path = f.Name()
	r.size = 0
	r.nextRotate = r.boundaryAfter(r.now())
	// 步骤 7: 更新 symlink 指向新活跃文件（§4.5）；失败（含 M3b 拒绝替换
	// 真实文件）仅 verbose 警告，禁止传播进 Write。
	if r.cfg.Symlink != "" {
		if err := updateSymlink(r.path, r.cfg.Symlink); err != nil {
			r.warnf("symlink %s -> %s: %v", r.cfg.Symlink, r.path, err)
		}
	}
	// 步骤 8: 异步压缩已关闭的旧文件（§4.4 skip-if-busy），失败仅 verbose 警告。
	if r.cfg.Compress {
		r.compressAsync(oldPath)
	}
	// 步骤 9: 机会式清理旧归档（§4.3 MaxAge/MaxBackups），失败仅 verbose 警告。
	if r.cfg.MaxBackups > 0 || r.cfg.MaxAge > 0 {
		r.cleanup()
	}
	// 步骤 10: verbose 模式输出切分事件到 stderr。
	if r.cfg.Verbose {
		fmt.Fprintf(os.Stderr, MsgPrefix+"rotated %s -> %s\n", oldPath, r.path)
	}
	return nil
}

// uniqueName 以 O_CREATE|O_EXCL 原子创建日志文件，文件名冲突时按 §4.2 步骤 4
// 追加序号（.1, .2, ...）继续探测，返回成功创建的文件句柄。
func uniqueName(name string) (*os.File, error) {
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s%s%d", name, seqSeparator, i)
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

// naturalBoundaryMaxPeriod 是自然边界对齐（D2）适用的最大周期（DESIGN
// §4.2 DA-1 修订注，2026-11-13 审计）：RotationTime ≤ 24h（含 24h 整数倍）
// 边界按当日午夜自然对齐（24h 周期恰为午夜切分）；> 24h 改为周期递推锚定
// （自上一计划边界严格 + 周期递推，size/SIGHUP 提前切分不顺延计划边界），
// 否则非 24h 整数倍周期的稳态会静默退化为 24h。24h 同时是 CLI
// --rotation-time 的默认值（cmd/root.go §3.2 表）——两者语义独立
// （算法分支阈值 ≠ 展示层默认值），刻意不共享常量。
const naturalBoundaryMaxPeriod = 24 * time.Hour

// boundaryAfter 计算 t 之后的下一个时间切分点。RotationTime <= 0 时返回零值（禁用）。
// d <= 24h：D2 自然边界对齐（如 24h 周期 → 午夜切分，而非"t + 周期"），行为不变。
// d > 24h：周期递推锚定（2026-11-13 审计 DA-1 修订），见函数内注释。
func (r *Rotator) boundaryAfter(t time.Time) time.Time {
	d := r.cfg.RotationTime
	if d <= 0 {
		return time.Time{}
	}
	// d > 24h：周期递推锚定（2026-11-13 审计 DA-1 修订）。锚点为上一计划
	// 边界（r.nextRotate，由 New/rotate 维护）：返回递推序列（锚点 + k×d）上
	// 严格晚于 t 的最小边界——准点/迟到切分按 k 递推（跳过已错过的边界），
	// size/SIGHUP 提前切分保持即将到来的计划边界不漂移。递推以绝对时长
	// Add（不随 DST 墙钟平移）。旧实现每日重新午夜锚定，d > 24h 且非 24h
	// 整数倍时稳态周期静默退化为 24h；d 为 24h 整数倍时两种算法结果一致。
	// New() 首次调用时 nextRotate 为零值，落入下方 D2 公式产生首边界
	// （启动日午夜 + d），保持启动语义连续。
	if d > naturalBoundaryMaxPeriod && !r.nextRotate.IsZero() {
		prev := r.nextRotate
		if prev.After(t) {
			return prev
		}
		k := int64(t.Sub(prev)/d) + 1
		return prev.Add(time.Duration(k) * d)
	}
	// d <= 24h（及 d > 24h 的首边界）：D2 自然边界对齐公式。
	base := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	n := int64(t.Sub(base) / d)
	next := base.Add(time.Duration(n+1) * d)
	if !next.After(t) {
		next = next.Add(d)
	}
	return next
}

// now 返回按配置时区归一化的当前时间（Config.Now 注入优先，供测试控制；
// 注入检查先行短路，避免测试路径每次浪费一次 time.Now syscall）。
func (r *Rotator) now() time.Time {
	var t time.Time
	if r.cfg.Now != nil {
		t = r.cfg.Now()
	} else {
		t = time.Now()
	}
	if r.cfg.LocalTime {
		return t.Local()
	}
	return t.UTC()
}

// formatName 按 pattern 生成 t 时刻的日志文件名（真 strftime 语义）：
// 仅 §3.3 六个占位符参与时间格式化，其余字符保持字面量。
//
// 禁止改为 time.Format(parsePattern(pattern)) 整体格式化——Go layout 会把路径中
// 的字面数字解释为时间 token（"1"月/"2"日/"15"时/"2006"年等），
// 如 /data2/ 会被格式化成 /data13/、字面量 "2006" 会被级联替换为年份。
func (r *Rotator) formatName(t time.Time) string {
	pattern := r.cfg.Pattern
	if !strings.ContainsRune(pattern, '%') {
		return pattern
	}
	var b strings.Builder
	b.Grow(len(pattern) + 8)
	for i := 0; i < len(pattern); {
		if pattern[i] == '%' && i+1 < len(pattern) {
			if layout, ok := strftimeReplacements[pattern[i:i+2]]; ok {
				b.WriteString(t.Format(layout))
				i += 2
				continue
			}
		}
		b.WriteByte(pattern[i])
		i++
	}
	return b.String()
}

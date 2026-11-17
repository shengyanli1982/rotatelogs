package rotator

import "time"

// WriteBufferSize 是写缓冲固定大小（DESIGN §4.1 契约值：64KB）。
// 容量驱动 flush 由 bufio 自满自动承担；时间驱动 flush 由心跳 flusher 承担
// （FlushHeartbeatInterval/FlushIdleTimeout，见下与 DESIGN §4.1 addendum）。
const WriteBufferSize = 64 * 1024

// FlushHeartbeatInterval 是心跳 flusher 的醒来间隔（law 式心跳 idle flush 策略，
// DESIGN §4.1 addendum）：每 300ms 醒来一次检查双时钟静默条件。值越小可见性
// 延迟越贴近 FlushIdleTimeout，代价是更频繁的 mu 竞争与空转唤醒；300ms 相对
// 2s 静默期足够细（每次心跳开销 = 一次锁 + 两次时间差比较，空缓冲时 no-op）。
const FlushHeartbeatInterval = 300 * time.Millisecond

// FlushIdleTimeout 是缓冲静默判定阈值（双时钟取 OR，law 式）：
// executeAt（最后写入活动）距今 ≥ 2s → 静默期已到，flush；
// lastFlushAt（最后成功 flush）距今 ≥ 2s → 连续写入不得无限推迟 flush（饥饿上界）。
// 可见性延迟上界 = FlushIdleTimeout（A′ 为纯心跳、非行级即时 flush，用户选定语义）；
// stdin 洪峰下 executeAt 恒新，批处理由 64KB bufio 自满承担，吞吐不退化。
const FlushIdleTimeout = 2 * time.Second

// 文件命名契约常量（Addendum 2 §2）：生成端（uniqueName/createGzFile）与
// 清理端（archiveGlobs 的 D4 变体 ".[0-9]*"/".gz"）必须共用同一组值，
// 任一侧改动都会破坏"归档可被清理"的闭环，故集中于此。
const (
	// gzSuffix 是压缩产物后缀（§4.4）。
	gzSuffix = ".gz"
	// seqSeparator 是文件名冲突序号分隔符（§4.2 步骤 4：app.log.1）。
	seqSeparator = "."
	// seqGlobSuffix 是清理端序号变体的 glob 数字尾部（D4 三变体之
	// ".[0-9]*" = seqSeparator + seqGlobSuffix，§4.3：尾部 * 同时覆盖
	// "序号+压缩"组合 app.log.1.gz）。与 gzSuffix/seqSeparator 同属生成端
	// ↔清理端闭环值组：序号产物由 uniqueName/createGzFile 以
	// seqSeparator+N 生成，清理端必须以同一分隔符引导的数字模式匹配，
	// 任一侧漂移都会使序号归档漏清理。archiveGlobs（全路径形态）与
	// archiveBaseGlobs（基名形态）两处构造必须逐字一致——双路径语义
	// 等价由清理等价性测试钉住，值级共享由此处集中保证。
	seqGlobSuffix = "[0-9]*"
)

// MsgPrefix 是全部错误值与 stderr 诊断输出的统一前缀（Addendum 2 §1 消息
// 约定，见 errors.go 集中处记载）：rotator 层错误自带本前缀，cmd 层自产
// 错误（stdin 读取失败、--max-size 溢出防护）补齐本前缀，两层合流后每条
// stderr 消息前缀恰出现一次（execute 测试 "prefix must appear exactly
// once" 钉住）。warnf 的 verbose 降级告警、切分事件与 symlink 平台提示
// 等非错误诊断输出同用本前缀。导出供 cmd 层共用（跨包契约）。
const MsgPrefix = "rotatelogs: "

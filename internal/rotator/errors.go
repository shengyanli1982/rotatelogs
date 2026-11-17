package rotator

import "errors"

// 全部错误 sentinel 集中于本文件（Addendum 2 §1）。
// 约定：消息自带 "rotatelogs: " 前缀；导出与否取决于 cmd 层是否需要
// errors.Is 判别；返回点直返 sentinel，%w 仅用于添加真实上下文增量
// （字段名、路径、底层 error）；运行期 IO 错误不设 sentinel。
var (
	// ErrInvalidConfig 是配置校验失败的总入口（Addendum 2 §1）：
	// validator 字段错误、跨字段规则（M1/M2/M5）与 M3a 均以 %w 包装本 sentinel，
	// 消息携带字段名与规则明细；cmd 层经 errors.Is 判别后 exit(1)。
	ErrInvalidConfig = errors.New(MsgPrefix + "invalid configuration")
	// errClosed 在 Close 之后的 Write 调用返回。
	errClosed = errors.New(MsgPrefix + "rotator is closed")
)

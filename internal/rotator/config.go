package rotator

import "time"

// Config 定义 Rotator 的全部配置项。
// 由 cmd 层从 cobra flags 解析后填充。
// validate tag 是字段级校验规则（Addendum 2 §3.1），由 Validate() 统一执行；
// Symlink 刻意不加 filepath tag——validator 该类 tag 平台相关，跨平台误报。
type Config struct {
	// Pattern 是文件名模板，使用 strftime 格式。
	// 必填。示例: "/var/log/app/app-%Y-%m-%d.log"
	Pattern string `validate:"required"`

	// MaxSize 是单个文件最大字节数。0 表示不限制。
	// 由 --max-size (MB) × 1024 × 1024 计算得出。
	MaxSize int64 `validate:"gte=0"`

	// RotationTime 是时间切分周期。0 表示禁用时间切分。
	RotationTime time.Duration `validate:"gte=0"`

	// MaxBackups 是保留的最大备份文件数。0 表示不删除。
	MaxBackups int `validate:"gte=0"`

	// MaxAge 是保留的最大天数。0 表示不删除。
	MaxAge int `validate:"gte=0"`

	// Compress 启用切分后异步 gzip 压缩。
	Compress bool

	// Symlink 是指向当前文件的符号链接路径。空字符串表示不维护。
	Symlink string

	// Verbose 启用切分事件输出到 stderr。
	Verbose bool

	// LocalTime 使用本地时间（默认 UTC）。
	LocalTime bool

	// Now 返回当前时间。仅用于测试注入，生产环境为 nil（使用 time.Now）。
	Now func() time.Time
}

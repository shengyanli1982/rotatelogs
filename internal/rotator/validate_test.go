package rotator

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestErrInvalidConfigMessage 钉住 sentinel 消息（Addendum 2 §1，cmd 层 exit(1) 判别入口）。
func TestErrInvalidConfigMessage(t *testing.T) {
	assert.Equal(t, "rotatelogs: invalid configuration", ErrInvalidConfig.Error())
}

// TestValidateFieldTagRules 验证字段级 tag 规则（Addendum 2 §3.1）：
// Pattern required；MaxSize/RotationTime/MaxBackups/MaxAge gte=0。
// 每条失败均须包装 ErrInvalidConfig 且消息含字段名。
func TestValidateFieldTagRules(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantField string
	}{
		{"Pattern required", func(c *Config) { c.Pattern = "" }, "Pattern"},
		{"MaxSize negative", func(c *Config) { c.MaxSize = -1 }, "MaxSize"},
		{"RotationTime negative", func(c *Config) { c.RotationTime = -time.Second }, "RotationTime"},
		{"MaxBackups negative", func(c *Config) { c.MaxBackups = -1 }, "MaxBackups"},
		{"MaxAge negative", func(c *Config) { c.MaxAge = -1 }, "MaxAge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Pattern: "app.log", MaxSize: 1024}
			tt.mutate(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidConfig),
				"error must wrap ErrInvalidConfig, got %v", err)
			assert.Contains(t, err.Error(), tt.wantField,
				"message must name the offending field")
		})
	}
}

// TestValidateReportsAllFieldErrors 验证多个字段同时非法时消息含全部字段名（分号连接）。
func TestValidateReportsAllFieldErrors(t *testing.T) {
	err := Config{Pattern: "", MaxSize: -1, MaxAge: -2}.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
	assert.Contains(t, err.Error(), "Pattern")
	assert.Contains(t, err.Error(), "MaxSize")
	assert.Contains(t, err.Error(), "MaxAge")
}

// TestValidateM1RequiresRotationCondition 验证 M1（Addendum 2 §3.2）：
// MaxSize==0 && RotationTime==0 → 至少一个切分条件必须启用。
func TestValidateM1RequiresRotationCondition(t *testing.T) {
	err := Config{Pattern: "app.log"}.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
	assert.Contains(t, err.Error(), "MaxSize")
	assert.Contains(t, err.Error(), "RotationTime")
}

// TestValidateM2RequiresTimePlaceholder 验证 M2（Addendum 2 §3.2）：
// RotationTime>0 且 Pattern 无时间占位符 → 失败（原 D8/errNoTimePlaceholder 语义并入）。
// 2026-11-16 意图区分降级变更：rotator 层 M2 硬报错语义不变（校验入口唯一），
// 错误文案追加修复提示（占位符示例与 --rotation-time 0 关闭方式）。
func TestValidateM2RequiresTimePlaceholder(t *testing.T) {
	err := Config{Pattern: "app.log", RotationTime: time.Hour}.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
	assert.Contains(t, err.Error(), "Pattern")
	assert.Contains(t, err.Error(), "no time placeholder")
	assert.Contains(t, err.Error(), "add a strftime placeholder (e.g. %Y-%m-%d)",
		"message must suggest the placeholder fix")
	assert.Contains(t, err.Error(), "disable time rotation with --rotation-time 0",
		"message must suggest disabling time rotation")
}

// TestValidateM5RejectsGlobMetachars 验证 M5（2026-11-13 审计 DA-3）：pattern
// 字面量部分（剔除 §3.3 六个 strftime token 后）含 * ? [ 任一 glob 元字符 →
// 启动拒绝。欠匹配面：字面量含 [...] 时被 D4 glob 解释为字符类，自家归档
// 永不命中 → 清理静默失效、磁盘无界增长；过度匹配面：字面量含 */? 时 glob
// 命中范围越界 → cleanup 误删同目录第三方文件。
func TestValidateM5RejectsGlobMetachars(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{"undermatch: char class", "x[1].log"},
		{"undermatch: unpaired bracket", "app[prod.log"},
		{"overmatch: star", "x*.log"},
		{"overmatch: question mark", "x?.log"},
		{"metachar beside time token", "app-%Y*.log"},
		{"metachar in dir pattern", "logs/%Y/%m/app[%d].log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{Pattern: tt.pattern, MaxSize: 1024}.Validate()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidConfig),
				"error must wrap ErrInvalidConfig, got %v", err)
			assert.Contains(t, err.Error(), "glob metacharacters",
				"message must state the rejection reason")
			assert.Contains(t, err.Error(), tt.pattern,
				"message must quote the offending pattern")
		})
	}
}

// TestValidateM5RejectsBackslashOnUnix 验证 M5 的 Unix 收紧（2026-11-17 审计
// R5-P1）：\ 在 Unix filepath.Match 中是转义元字符，而生成端 formatName 逐字节
// 字面量保留 \ 建文件/目录——生成↔清理语义分叉：自家归档永不命中清理
// （MaxBackups/MaxAge 静默失效、磁盘无界增长），且脱转义形状命中并删除同目录
// 第三方文件（app\.log 脱转义为 app.log）。四形态启动即拒绝。
func TestValidateM5RejectsBackslashOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`backslash is a path separator on Windows; M5 set stays *?[`)
	}
	tests := []struct {
		name       string
		pattern    string
		maxBackups int
	}{
		{"escaped dot in base name", `app\.log`, 0},
		{"windows-style separator misuse", `logs\app-%Y.log`, 0},
		// cronolog 目录段：MaxBackups>0 使 M6 前缀锚也满足（基名 app-%d.log
		// 以字面量开头），隔离 M5 单因。
		{"cronolog dir segment", `a\b/%Y/app-%d.log`, 3},
		// 收紧副作用钉住：反斜杠仅在目录部分且目录无 token——修前平面快路径
		// 按字面量开目录可运行，修后 Unix 启动拒绝（已批准的有意收紧）。
		{"dir-only backslash tightened", `/tmp/a\b/app-%Y.log`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Pattern: tt.pattern, MaxSize: 1024, MaxBackups: tt.maxBackups}
			err := cfg.Validate()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidConfig),
				"error must wrap ErrInvalidConfig, got %v", err)
			assert.Contains(t, err.Error(), "glob metacharacters",
				"message must state the rejection reason")
			assert.Contains(t, err.Error(), tt.pattern,
				"message must quote the offending pattern")
		})
	}
}

// TestM5GlobMetaCharsPlatformSet 钉住 M5 禁集的平台条件化（2026-11-17 审计
// R5-P1）：Unix 集合 *?[\ 与 filepath 内部 hasMeta 的 Unix 集合对齐（\ 是
// Match 转义元字符）；Windows 集合 *?[（\ 是路径分隔符，Match 禁用转义）。
// msgM5 从该集合派生（单一事实源），"glob metacharacters" 子串保持稳定。
func TestM5GlobMetaCharsPlatformSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		assert.Equal(t, "*?[", globMetaChars)
	} else {
		assert.Equal(t, `*?[\`, globMetaChars)
	}
	assert.Contains(t, msgM5, "glob metacharacters",
		"cmd/rotator assertion surface pins this substring")
	assert.Contains(t, msgM5, globMetaChars,
		"message charset must derive from globMetaChars")
}

// TestValidateM5AllowsBackslashSeparatorOnWindows 钉住平台条件化禁集的
// Windows 侧（2026-11-17 审计 R5-P1）：\ 在 Windows 是路径分隔符、
// filepath.Match 禁用转义，禁集保持 *?[，Windows 风格分隔符 pattern 正常
// 过 M5（不得把 Unix 收紧误伤到 Windows）。
func TestValidateM5AllowsBackslashSeparatorOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip(`backslash is a path separator only on Windows`)
	}
	tests := []struct {
		name       string
		pattern    string
		maxBackups int
	}{
		{"separator with time token", `logs\app-%Y.log`, 3},
		{"separator fixed name", `logs\app.log`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Pattern: tt.pattern, MaxSize: 1024, MaxBackups: tt.maxBackups}
			assert.NoError(t, cfg.Validate())
		})
	}
}

// TestValidateM6RejectsThinPatternWhenCleanupEnabled 验证 M6（2026-11-15 审计
// N1）：cleanup 启用（MaxBackups>0 || MaxAge>0）时，pattern 基名剔除 §3.3 token
// 后的字面量部分不含任何字母/数字 → 启动拒绝。cleanup 关闭时纯 token/薄字面量
// pattern 放行（条件化规则，区别于 M5 无条件拒绝，避免过度限制）。
func TestValidateM6RejectsThinPatternWhenCleanupEnabled(t *testing.T) {
	tests := []struct {
		name       string
		pattern    string
		maxBackups int
		maxAge     int
		wantErr    bool
	}{
		// 反例：cleanup 启用 + 基名无字面量锚 → 拒
		{"pure tokens + max-backups", "%Y%m%d%H%M%S", 1, 0, true},
		{"dash-only literal + max-age", "%Y-%m-%d", 0, 7, true},
		{"pure tokens + both cleanup", "%Y%m%d%H%M%S", 3, 7, true},
		// 反例（2026-11-16 审计 P1-A 收紧）：基名以 token 开头（仅后缀锚）→
		// 清理 glob 以 * 开头、左侧零收窄，*.log 形态仍误删同目录全部第三方
		// .log 文件——后缀锚对 glob 左侧零贡献 → 拒
		{"suffix-only anchor rejected", "%Y%m%d.log", 1, 0, true},
		{"suffix dot-log rejected", "%Y.log", 0, 7, true},
		// 正例：cleanup 关闭时纯 token/薄字面量放行（关键差异）
		{"pure tokens, cleanup disabled", "%Y%m%d%H%M%S", 0, 0, false},
		{"dash-only literal, cleanup disabled", "%Y-%m-%d", 0, 0, false},
		// 正例：基名含字面量锚 → 过（即便 cleanup 启用）
		{"literal prefix + cleanup", "app-%Y-%m-%d.log", 5, 0, false},
		{"cronolog base name + cleanup", "%Y/%m/%d/access.log", 5, 7, false},
		{"readme second-precision + cleanup", "logs/app-%Y%m%d-%H%M%S.log", 0, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Pattern:    tt.pattern,
				MaxSize:    1024,
				MaxBackups: tt.maxBackups,
				MaxAge:     tt.maxAge,
			}
			err := cfg.Validate()
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidConfig),
				"error must wrap ErrInvalidConfig, got %v", err)
			assert.Contains(t, err.Error(), "literal anchor",
				"message must state the M6 rejection reason")
			assert.Contains(t, err.Error(), tt.pattern,
				"message must quote the offending pattern")
		})
	}
}

// TestValidateM7RejectsMaxAgeAboveLimit 验证 M7（2026-11-15 审计 N2）：
// MaxAge > 365000 → 拒绝（AddDate 天数换算 int64 秒回绕致 cutoff 落未来、
// removeExpired 全删归档）。边界两侧：365000 过、365001 拒。
func TestValidateM7RejectsMaxAgeAboveLimit(t *testing.T) {
	// 上界值合法（pattern 含锚，不触发 M6）
	assert.NoError(t, Config{Pattern: "app.log", MaxSize: 1024, MaxAge: 365000}.Validate())

	// 越上界拒绝
	err := Config{Pattern: "app.log", MaxSize: 1024, MaxAge: 365001}.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig),
		"error must wrap ErrInvalidConfig, got %v", err)
	assert.Contains(t, err.Error(), "MaxAge",
		"message must name the offending field")
}

// TestNewRejectsGlobMetaPattern 验证 M5 在 New 的 Validate 闸门生效且先于
// 任何文件操作（Addendum 2 §3.3 单一入口）：目录内不得残留日志文件。
func TestNewRejectsGlobMetaPattern(t *testing.T) {
	dir := t.TempDir()
	_, err := New(Config{Pattern: filepath.Join(dir, "x*.log"), MaxSize: 1024})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "validation must run before any file is created")
}

// TestNewM5BackslashThirdPartySurvives 重放审计 R5-P1 的第三方删除面
// （2026-11-17，Unix only，修后语义）：目录内预创建第三方 app.log（mtime
// 10 天前，超 MaxAge=7），pattern app\.log 经 Unix Match 脱转义后恰命中
// app.log——修前过闸后清理可越界删除该第三方文件。修后 M5 必须在 New 第一步
// 拒绝：第三方文件幸存、目录零新增（校验先于任何文件操作）。
func TestNewM5BackslashThirdPartySurvives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`backslash is a path separator on Windows; no escape divergence`)
	}
	dir := t.TempDir()
	thirdParty := filepath.Join(dir, "app.log")
	touchFile(t, thirdParty, cleanupNow.AddDate(0, 0, -10))

	_, err := New(Config{
		Pattern: filepath.Join(dir, `app\.log`),
		MaxSize: 1,
		MaxAge:  7,
		Now:     func() time.Time { return cleanupNow },
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig),
		"error must wrap ErrInvalidConfig, got %v", err)
	assert.Contains(t, err.Error(), "glob metacharacters",
		"message must state the M5 rejection reason")

	data, readErr := os.ReadFile(thirdParty)
	require.NoError(t, readErr, "third-party file must survive the M5 gate")
	assert.Equal(t, "x", string(data), "third-party content must be intact")
	assert.Equal(t, []string{"app.log"}, listNames(t, dir),
		"validation must run before any file is created")
}

// TestValidateAcceptsLegalConfigs 验证合法配置零误报（Addendum 2 §3.4 +
// addendum-flag-validation "明确合法的组合"：-t 0 与 -s 0 单独启用均合法）。
func TestValidateAcceptsLegalConfigs(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"size only, fixed name", Config{Pattern: "app.log", MaxSize: 1024}},
		{"size only, time pattern", Config{Pattern: "app-%Y%m%d.log", MaxSize: 1024}},
		{"time only", Config{Pattern: "app-%Y%m%d.log", RotationTime: 24 * time.Hour}},
		{"size and time", Config{Pattern: "app-%Y%m%d.log", MaxSize: 1024, RotationTime: time.Hour}},
		{"zero-valued optional fields", Config{Pattern: "app.log", MaxSize: 1, MaxBackups: 0, MaxAge: 0}},
		// M5 正例（2026-11-13 审计 DA-3）：普通字面量字符（. - _ %）与
		// 单独的 ] 不是 glob 元字符，不得误伤。
		{"literal dot dash underscore", Config{Pattern: "app-v1_2.access-%Y.log", MaxSize: 1024}},
		{"literal percent", Config{Pattern: "100%.log", MaxSize: 1024}},
		{"lone closing bracket", Config{Pattern: "app]-%Y.log", MaxSize: 1024}},
		// M5 正例回归（2026-11-17 审计 R5-P1）：Unix 禁集补 \ 后常规 pattern
		// 不受影响（cleanup 启用，同时验证 M5/M6 无误伤）。
		{"r5-p1 plain fixed name + cleanup", Config{Pattern: "app.log", MaxSize: 1024, MaxBackups: 3}},
		{"r5-p1 daily pattern + cleanup", Config{Pattern: "app-%Y-%m-%d.log", MaxSize: 1024, MaxBackups: 3}},
		{"r5-p1 cronolog dirs + cleanup", Config{Pattern: "%Y/%m/%d/access.log", MaxSize: 1024, MaxBackups: 3}},
		{"full config", Config{
			Pattern:      "app-%Y.log",
			MaxSize:      1024,
			RotationTime: time.Hour,
			MaxBackups:   3,
			MaxAge:       7,
			Compress:     true,
			Symlink:      "current.log",
			Verbose:      true,
			LocalTime:    true,
			Now:          time.Now,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, tt.cfg.Validate())
		})
	}
}

// TestNewValidatesConfigBeforeFileOps 验证 New 第一步执行 cfg.Validate()
// （Addendum 2 §3.3）：M1 违规配置被拒绝且发生在任何文件操作之前。
func TestNewValidatesConfigBeforeFileOps(t *testing.T) {
	dir := t.TempDir()
	_, err := New(Config{Pattern: filepath.Join(dir, "app.log")}) // MaxSize==0 && RotationTime==0
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "validation must run before any file is created")
}

// TestNewRejectsSymlinkSameAsLogFile 验证 M3a（Addendum 2 §3.3）：
// Symlink 与初始日志文件路径经 filepath 绝对化后相同 → 拒绝启动。
func TestNewRejectsSymlinkSameAsLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// 直接相同
	_, err := New(Config{Pattern: path, MaxSize: 1024, Symlink: path})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
	assert.Contains(t, err.Error(), "Symlink")

	// 绝对化（Clean）后等价：dir/sub/../app.log == dir/app.log
	_, err = New(Config{
		Pattern: path,
		MaxSize: 1024,
		Symlink: filepath.Join(dir, "sub", "..", "app.log"),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))

	// 不同路径的 Symlink 合法（正例，不受 M3a 误伤）
	link := filepath.Join(dir, "current.log")
	r, err := New(Config{Pattern: path, MaxSize: 1024, Symlink: link})
	require.NoError(t, err)
	require.NoError(t, r.Close())
}

// TestNewRejectsSymlinkTmpSlotEqualToLogFile 钉住 M3a 扩展（2026-11-16 审计
// P1-B 链 A 固定名形态）：Symlink+".tmp" 临时槽位与初始日志文件路径相同 →
// 拒绝启动。否则 New 尾部 updateSymlink 的 os.Remove(tmp) 将无条件解链刚
// 创建的活跃文件，后续写入全部进入孤儿 inode、Close 后数据静默消失。
// 校验先于任何文件操作（MkdirAll/OpenFile），目录零残留。
func TestNewRejectsSymlinkTmpSlotEqualToLogFile(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "app.tmp")

	_, err := New(Config{
		Pattern: pattern,
		MaxSize: 1024,                      // size-only，M2 允许无 token（审计链 A 形态）
		Symlink: filepath.Join(dir, "app"), // app+".tmp" == 日志文件路径
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig),
		"error must wrap ErrInvalidConfig, got %v", err)
	assert.Contains(t, err.Error(), "Symlink",
		"message must name the offending field")

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "M3a must reject before any file is created")
}

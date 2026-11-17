package rotator

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-playground/validator/v10"
)

// struct-level 跨字段规则标识与明细消息（Addendum 2 §3.2）。
// 消息包含 struct 字段名，Validate 的返回文本可直接作为 cmd 层单行错误输出。
const (
	ruleM1 = "M1"
	msgM1  = "at least one of MaxSize or RotationTime must be > 0"

	ruleM2 = "M2"
	// msgM2 追加修复提示（2026-11-16 意图区分降级变更）：指明两条出路——
	// 补占位符或显式 --rotation-time 0，避免用户对着配置矛盾无从下手。
	msgM2 = "Pattern contains no time placeholder while RotationTime is enabled; add a strftime placeholder (e.g. %Y-%m-%d) or disable time rotation with --rotation-time 0"

	// ruleM5（2026-11-13 审计 DA-3）：pattern 字面量禁含 glob 元字符。
	// D4 归档匹配将 pattern（token → *）交给 filepath.Glob：字面量含 [ 时
	// 被解释为字符类（自家归档永不命中 → 清理静默失效、磁盘无界增长），
	// 含 * ? 时命中范围越界（误删同目录第三方文件）。启动即拒绝优于静默
	// 失效，与 M1/M2 同属配置闸门职责（P2-6 先例）。] 在 filepath.Match
	// 语义下是普通字面量，不禁止。
	// 2026-11-17 审计 R5-P1 补 \（Unix）：\ 在 Unix filepath.Match 中是转义
	// 元字符，而生成端 formatName 逐字节字面量保留 \ 建文件/目录——生成↔
	// 清理语义分叉：自家归档永不命中清理（MaxBackups/MaxAge 静默失效、磁盘
	// 无界增长），且脱转义形状命中并删除同目录第三方文件（如 app\.log 脱
	// 转义为 app.log，数据丢失）。Windows 不受影响：\ 是路径分隔符、Match
	// 禁用转义，禁集保持 *?[——平台条件化见 globMetaChars。
	// 收紧副作用（已批准）：反斜杠仅在目录段且目录无 token 的形态（如
	// /tmp/a\b/app-%Y.log，修前平面快路径按字面量开目录可正常运行）修后在
	// Unix 同样启动拒绝——与 M5 无条件拒绝哲学一致，不留静默分叉面。
	ruleM5 = "M5"

	// ruleM6（2026-11-15 审计 N1，M1-M5 编号已占用故新规则从 M6 起）：cleanup
	// 启用时 pattern 基名须有字面量前缀锚。D4 归档匹配将 pattern（token → *）
	// 交给 filepath.Glob 做"形状匹配"：基名字面量为空（%Y%m%d%H%M%S → ***）或
	// 仅剩标点（%Y-%m-%d → *-*-*）时 glob 退化为全目录/宽形状匹配，cleanup
	// 静默删除同目录第三方文件（数据丢失）。M5 仅封堵元字符路径，字面量缺失/过薄
	// 路径完全绕过闸门——M6 补齐该防护面。仅当 cleanup 启用（MaxBackups>0 || MaxAge>0）
	// 时触发：cleanup 关闭时无删除面，不限制纯 token pattern（条件化规则，是与
	// M5 无条件拒绝的关键差异，避免过度限制）。
	// 2026-11-16 审计 P1-A 收紧：判定维度从"字面量存在"改为"前缀锚存在"——
	// 基名以 token 开头（%Y.log、%Y%m%d.log）时剔除后 glob 以 * 开头，左侧
	// 零收窄（*.log 形态在日志目录近似全匹配），后缀锚对删除面无贡献；
	// 首字符为非 token 字面量（含非有效 token 的 '%'，如 %%、%x）时 glob
	// 左侧被字面量收窄，放行。仅取基名：cronolog 形态目录段 token 不参与
	// 基名形状匹配（%Y/%m/%d/access.log 的基名 access.log 全字面量，过）。
	ruleM6 = "M6"
	msgM6  = "Pattern base name must start with a literal prefix anchor when cleanup is enabled; a suffix-only literal anchor leaves the cleanup glob unanchored on the left"

	// ruleM7（2026-11-15 审计 N2）：MaxAge 安全上界。cleanup 的 cutoff 由
	// now.AddDate(0, 0, -MaxAge) 计算，AddDate 将天数换算为内部 int64 秒；
	// |days×86400| 超出 int64 秒范围（约 ±1.07×10^14 天）时结果回绕，cutoff 可
	// 落入未来 → removeExpired 判定全部归档过期而全删（"永久保留"意图反转为
	// "全删"）。maxAgeLimit=365000 天（≈1000 年）是覆盖任何真实保留策略的安全
	// 上包络。
	ruleM7 = "M7"
	msgM7  = "MaxAge must be <= 365000 days (~1000 years); larger values wrap the cutoff into the future and delete all archives"
)

// maxAgeLimit 是 MaxAge 的天数安全上界（M7）：≈1000 年，覆盖任何真实保留策略，
// 且保证 AddDate 的天→秒换算不超出 int64 表示范围而回绕。
const maxAgeLimit = 365000

// globMetaChars 是 M5 禁止出现在 pattern 字面量中的 glob 元字符集。
// 平台条件化（2026-11-17 审计 R5-P1）：Unix 集合 *?[\ 与 filepath 内部
// hasMeta 的 Unix 集合对齐——\ 在 Match 中是转义元字符、属"元"字符；
// Windows 集合 *?[ 不含 \（\ 是路径分隔符，Match 禁用转义）。分发形态
// 沿用 cleanup.go matchArchives 的 runtime.GOOS 先例，不新增 build-tag
// 生产文件（DESIGN §7.2 的 build-tag 文件清单保持稳定）。
var globMetaChars = func() string {
	if runtime.GOOS == "windows" {
		return "*?["
	}
	return `*?[\`
}()

// msgM5 从 globMetaChars 派生（2026-11-17 审计 R5-P1，单一事实源）：消息
// 中的字符集描述与平台条件化禁集自动同步，"glob metacharacters" 子串保持
// 稳定（rotator/cmd 测试断言面）。pattern 原文仍由 describeFieldError 附带。
var msgM5 = "Pattern literal must not contain glob metacharacters (" + globMetaChars + ")"

// tokenStripper 剔除 §3.3 六个 strftime token（替换为空串），得到 pattern
// 的字面量部分——M5 的元字符检测输入。token 互不为前缀，与 map 迭代顺序
// 无关（与 strftimeReplacer/globReplacer 同一构造模式）。
var tokenStripper = newTokenStripper()

func newTokenStripper() *strings.Replacer {
	pairs := make([]string, 0, 2*len(strftimeReplacements))
	for token := range strftimeReplacements {
		pairs = append(pairs, token, "")
	}
	return strings.NewReplacer(pairs...)
}

// hasGlobMetaLiteral 报告 pattern 的字面量部分（剔除时间占位符后）是否含
// globMetaChars 中任一字符（M5 判定）。
func hasGlobMetaLiteral(pattern string) bool {
	return strings.ContainsAny(tokenStripper.Replace(pattern), globMetaChars)
}

// hasLiteralAnchor 报告 pattern 基名（filepath.Base）首部是否存在非空字面量
// 前缀锚（M6 判定，2026-11-16 审计 P1-A 收紧）：基名首两个字符构成有效
// §3.3 strftime token（%Y/%m/%d/%H/%M/%S）即基名以 token 开头，剔除后
// 清理 glob 以 * 开头、左侧零收窄，返回 false；基名首字符为任何字面量
// （含非有效 token 的 '%'，如 %%、%x——它们不被 token 替换，glob 形态
// 保持字面量）返回 true。仅取基名：cronolog 形态 %Y/%m/%d/access.log 的
// 基名为 access.log，全字面量通过；目录分量不参与清理 glob 的基名形状
// 匹配。token 集以 strftimeReplacements 为准，不硬编码。
func hasLiteralAnchor(pattern string) bool {
	base := filepath.Base(pattern)
	if len(base) < 2 {
		return true // 长度不足一个 token（两字符），首部必为字面量
	}
	_, isToken := strftimeReplacements[base[:2]]
	return !isToken
}

// configValidator 是包级 validator 单例（Addendum 2 §3.3）：
// 注册期构建反射缓存，Config 规则只读不变，单例可并发复用。
var configValidator = func() *validator.Validate {
	v := validator.New()
	v.RegisterStructValidation(validateConfigStructLevel, Config{})
	return v
}()

// Validate 执行 Config 的全部启动校验（单一校验入口，Addendum 2 §3.3）：
// 字段级 tag（config.go）+ 跨字段规则 M1/M2/M5/M6/M7（struct-level）。
// 任何失败统一包装 ErrInvalidConfig，消息含字段名与规则明细（分号连接）。
// M3a 依赖 formatName(now) 时间计算，由 New() 调用 checkSymlinkDistinct 检查。
func (c Config) Validate() error {
	err := configValidator.Struct(c)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	details := make([]string, 0, len(verrs))
	for _, fe := range verrs {
		details = append(details, describeFieldError(fe))
	}
	return fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(details, "; "))
}

// validateConfigStructLevel 注册跨字段规则（Addendum 2 §3.2）：
// M1 至少一个切分条件启用；M2 时间切分要求 pattern 含时间占位符（原 D8）；
// M5 pattern 字面量禁含 glob 元字符（2026-11-13 审计 DA-3）；
// M6 cleanup 启用时 pattern 基名须有字面量前缀锚（2026-11-15 审计 N1，
// 2026-11-16 审计 P1-A 收紧为前缀判定）；
// M7 MaxAge 安全上界（2026-11-15 审计 N2）。
func validateConfigStructLevel(sl validator.StructLevel) {
	c := sl.Current().Interface().(Config)
	if c.MaxSize == 0 && c.RotationTime == 0 {
		sl.ReportError(c.MaxSize, "MaxSize", "MaxSize", ruleM1, "")
	}
	if c.RotationTime > 0 && !HasTimePlaceholder(c.Pattern) {
		sl.ReportError(c.Pattern, "Pattern", "Pattern", ruleM2, "")
	}
	if hasGlobMetaLiteral(c.Pattern) {
		sl.ReportError(c.Pattern, "Pattern", "Pattern", ruleM5, "")
	}
	if (c.MaxBackups > 0 || c.MaxAge > 0) && !hasLiteralAnchor(c.Pattern) {
		sl.ReportError(c.Pattern, "Pattern", "Pattern", ruleM6, "")
	}
	if c.MaxAge > maxAgeLimit {
		sl.ReportError(c.MaxAge, "MaxAge", "MaxAge", ruleM7, "")
	}
}

// describeFieldError 将单条 validator 字段错误转为可读明细（字段名 + 规则）。
func describeFieldError(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return fe.StructField() + " is required"
	case "gte":
		return fmt.Sprintf("%s must be >= %s", fe.StructField(), fe.Param())
	case ruleM1:
		return msgM1
	case ruleM2:
		return msgM2
	case ruleM5:
		// 附带 pattern 原文（M3a 同款风格），便于用户直接定位非法配置。
		// Value() 携带 ReportError 传入的字段值；Field() 是字段名（"Pattern"）。
		return fmt.Sprintf("%s: %v", msgM5, fe.Value())
	case ruleM6:
		// 附带 pattern 原文，便于用户直接定位无前缀锚基名（与 M5 同款风格）。
		return fmt.Sprintf("%s: %v", msgM6, fe.Value())
	case ruleM7:
		// 附带越界的 MaxAge 值，便于用户直接定位非法配置。
		return fmt.Sprintf("%s (got %v)", msgM7, fe.Value())
	default:
		return fmt.Sprintf("%s: failed %s rule", fe.StructField(), fe.Tag())
	}
}

// checkSymlinkDistinct 执行 M3a（Addendum 2 §3.3）：filepath 绝对化（含 Clean）后
// Symlink 不得与初始日志文件路径相同（否则 symlink 将自我覆盖、清理判定自指），
// Symlink+".tmp" 临时槽位亦不得与初始日志文件路径相同（2026-11-16 审计 P1-B
// 扩展，保持 M3 家族语义不新开编号）——否则 updateSymlink 的 os.Remove(tmp)
// 将解链刚创建的活跃文件，后续写入全部进入孤儿 inode、Close 后数据静默丢失。
// 平台语义（2026-11-16 审查 P3-1）：上述比较均为绝对路径字符串比较，平台
// 无关、Windows 上同样生效；但 Windows 的 symlink 降级实现（symlink_windows.go）
// 无 tmp 槽机制，".tmp" 碰撞形态在 Windows 不构成实际威胁——此处保守拒绝是
// 跨平台校验一致性的有意取向（代价仅为 Windows 上拒绝实际无害的配置）。
// 依赖时间计算（初始文件名 = formatName(now)），故在 New() 内调用而非
// Validate()；日期推进后才可观察的碰撞形态由 updateSymlink 内的 M3b 同款
// tmp 槽位防护兜底（纵深防御第二层）。
func checkSymlinkDistinct(symlink, initialFile string) error {
	if symlink == "" {
		return nil
	}
	absLink, err := filepath.Abs(symlink)
	if err != nil {
		return fmt.Errorf("%w: Symlink %s: %v", ErrInvalidConfig, symlink, err)
	}
	absFile, err := filepath.Abs(initialFile)
	if err != nil {
		return fmt.Errorf("%w: Pattern %s: %v", ErrInvalidConfig, initialFile, err)
	}
	if absLink == absFile {
		return fmt.Errorf("%w: Symlink path must differ from the log file path %s", ErrInvalidConfig, absFile)
	}
	if absLink+symlinkTmpSuffix == absFile {
		return fmt.Errorf("%w: Symlink path must differ from the log file path %s (its .tmp temp slot collides)", ErrInvalidConfig, absFile)
	}
	return nil
}

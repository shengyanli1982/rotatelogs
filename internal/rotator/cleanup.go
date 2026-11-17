package rotator

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// globReplacer 将 pattern 中的 §3.3 时间占位符逐 token 替换为通配符 *
// （D4：app-%Y-%m-%d.log → app-*-*-*.log，比 §4.3 示例 app-*.log 更严格，
// 避免误删字面量段数不同的同前缀文件）。
var globReplacer = newGlobReplacer()

func newGlobReplacer() *strings.Replacer {
	pairs := make([]string, 0, 2*len(strftimeReplacements))
	for token := range strftimeReplacements {
		pairs = append(pairs, token, "*")
	}
	return strings.NewReplacer(pairs...)
}

// archiveGlobs 返回 D4 匹配变体集：基础 glob、压缩产物 .gz、冲突序号 .[0-9]*。
// 序号变体的尾部 * 同时覆盖"序号+压缩"组合（如 app.log.1.gz），
// 否则压缩产物与序号文件永不清理导致磁盘泄漏。
func archiveGlobs(pattern string) []string {
	g := globReplacer.Replace(pattern)
	return []string{g, g + gzSuffix, g + seqSeparator + seqGlobSuffix}
}

// archiveBaseGlobs 返回 D4 变体集的基名形态（C1 平面目录快路径专用）：
// 目录部分为字面量时，全路径匹配退化为对固定目录内 entry 基名的匹配，
// 与 archiveGlobs 的 glob 语义逐变体等价（§3.3 token 不含路径分隔符，
// token 替换与取 Base 可交换；M5 已拒绝字面量中的 glob 元字符）。
func archiveBaseGlobs(pattern string) []string {
	g := globReplacer.Replace(filepath.Base(pattern))
	return []string{g, g + gzSuffix, g + seqSeparator + seqGlobSuffix}
}

// patternDirHasMeta 报告 pattern 目录部分经 token 替换后是否含 glob 元字符
// （cronolog 形态：目录含 strftime 占位符，如 %Y/%m/）。M5 已在启动时拒绝
// 字面量中的元字符，故 meta 只可能来自 token 替换；该判定与"目录含时间
// 占位符"等价，且对未经验证的白盒配置保守——任何 meta 都走 Glob 回退路径。
func patternDirHasMeta(dir string) bool {
	return strings.ContainsAny(globReplacer.Replace(dir), globMetaChars)
}

// archiveFile 是一个 cleanup 候选，携带匹配阶段一次获取的 mtime 快照
// （已按 cleanup 入口 now 完成 DA-2 钳制），贯通 removeExpired 过期判定
// 与 sortByModTime 排序——消除旧实现对同一批文件的第 2/3 轮 os.Stat（C1，
// 2026-11-14 性能优化）。路径惰性构造：快路径只存基名+目录（扫描热循环
// 零路径拼接分配），仅在真正删除/告警时经 path() 拼接；回退路径 name 即
// 完整路径（dir 为空）。排序 tie-break 按 name 比较：快路径同目录内基名
// 序与完整路径序等价（公共前缀），回退路径 name 本就是完整路径，与旧实现
// strings.Compare(path) 逐位一致。
type archiveFile struct {
	dir     string    // 快路径：所属目录；回退路径：空
	name    string    // 快路径：基名；回退路径：完整路径
	mod     time.Time // DA-2 钳制后的 mtime
	clamped bool      // 是否发生钳制（tie-break：被钳制者优先删除）
}

// path 返回候选的完整路径（快路径惰性拼接，仅删除/告警时调用）。
func (a archiveFile) path() string {
	if a.dir == "" {
		return a.name
	}
	return filepath.Join(a.dir, a.name)
}

// cleanup 执行机会式清理（§4.3）：仅在切分事件后顺带执行，零定时器/常驻 goroutine。
// 先按 MaxAge 删除过期归档，再对剩余文件按修改时间（ModTime）升序删除超额的最旧
// 部分、保留最新 MaxBackups 个（两者同时生效取更严格）。2026-11-13 L5 修订：
// 排序键由文件名改为 ModTime——固定名+序号模式下字典序与时间序不一致
// （app.log.10 < app.log.2；序号重用后 app.log 携带新数据），按名字排序会误删
// 较新归档。任何失败仅 Verbose 警告，禁止传播进主写入流程（§4.8）。
//
// 在途压缩快照每轮只读取一次（P1-2）：排除判定与保留名额预留必须基于同一
// 视图，否则"matchArchives 已按在途排除、evictExcess 再读时记录已清空"的
// 交错会同时丢失排除项与名额，导致该轮一个不删、归档数突破 MaxBackups 上界。
// 当前时间同样在入口单次快照（DA-2）：钳制判定与 MaxAge cutoff 必须基于
// 同一 now，与在途排除的单快照纪律一致。文件元数据（mtime）自 C1 起也只在
// matchArchives 阶段获取一次并贯通后续判定——与上述单快照纪律对齐，同时
// 消除同一批文件的重复 stat 轮次。
func (r *Rotator) cleanup() {
	rec := r.compressing.Load()
	now := r.now()
	files := r.matchArchives(rec, now)
	if r.cfg.MaxAge > 0 {
		files = r.removeExpired(files, now)
	}
	if r.cfg.MaxBackups > 0 {
		r.evictExcess(files, rec != nil)
	}
}

// clampModTime 执行 DA-2 时钟 step 回拨钳制（2026-11-13 审计修订）：
// mtime > now 的归档（时钟快进期产物——NTP step 校正/VM 挂起恢复后其
// mtime 落在未来）钳制为 now 参与排序与 MaxAge cutoff，返回钳制后时间与
// 是否发生钳制。不钳制则未来 mtime 归档恒冒充"最新"且恒不过期，回拨后
// 的清理会删除真实最新归档、保留陈旧归档（保留语义反转）。
func clampModTime(mod, now time.Time) (time.Time, bool) {
	if mod.After(now) {
		return now, true
	}
	return mod, false
}

// evictExcess 按 §4.3 保留修改时间最新的 MaxBackups 个归档、删除更旧者。
// 压缩在途的源文件不参与删除（matchArchives 已按同一快照排除，P1-2），但
// 占用一个保留名额：源→.gz 是等量交换且在途源恒为最新归档，占位后已完成
// 归档只保留 MaxBackups-1 个，压缩完成即收敛回 §14.1-5 的"最多 N 个"上界，
// 不放宽数量契约。inFlight 必须来自本轮 cleanup 的同一快照；mtime 的 DA-2
// 钳制已在 matchArchives 阶段按入口 now 快照完成，本函数不再触时钟。
func (r *Rotator) evictExcess(files []archiveFile, inFlight bool) {
	keep := r.cfg.MaxBackups
	if inFlight {
		keep--
	}
	candidates := sortByModTime(files)
	if n := len(candidates) - keep; n > 0 {
		for _, f := range candidates[:n] {
			p := f.path()
			if err := os.Remove(p); err != nil {
				r.warnf("cleanup remove %s: %v", p, err)
			} else {
				r.pruneEmptyDirs(p)
			}
		}
	}
}

// sortByModTime 将 files 按修改时间升序原地排列（§4.3 排序键）并返回：
// 列表前端最旧，供 MaxBackups 从前向后删除超额、尾部保留最新 N 个。
// mod 为 matchArchives 阶段的快照值（已按 DA-2 以 cleanup 入口单一 now
// 快照钳制）；钳制后 ModTime 相同时被钳制者排前优先删除（真实当期新文件
// 幸存），再同按 name 升序回退（快路径=同目录基名序≡完整路径序；回退
// 路径=完整路径序，与旧实现一致），保证结果确定。Stat/Lstat 失败的文件
// 已在 matchArchives 阶段警告并排除出删除候选（无法确认时序的文件不删）；
// C1 起排序消费贯通的元数据快照，不再对每个文件重复 os.Stat。
// 原地排序：files 为 cleanup 局部所有，排序零额外分配。
func sortByModTime(files []archiveFile) []archiveFile {
	slices.SortFunc(files, func(a, b archiveFile) int {
		if c := a.mod.Compare(b.mod); c != 0 {
			return c
		}
		if a.clamped != b.clamped {
			if a.clamped {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	return files
}

// removeExpired 删除修改时间早于 MaxAge 天前的归档，返回存活文件列表。
// cutoff 基于 cleanup 入口的单一 now 快照（DA-2）；未来 mtime 按钳制后
// 值（=now）参与判定——不因此提前过期（now > cutoff 恒成立属正确语义：
// 它们由 MaxBackups 淘汰或等真实时间追上后过期），也不再逃逸出候选计数。
// C1 起判定消费 matchArchives 阶段贯通的 mod 快照（与旧实现逐文件
// clampModTime(os.Stat) 的判定输入一致），不再追加第 3 轮 stat。
// 原地过滤（INFO-A，2026-11-15）：kept 复用 files 底层数组——files 为
// cleanup 局部所有（matchArchives 每轮新建），无别名逃逸；过滤保序，
// 后续 sortByModTime/evictExcess 消费语义不变，省去每轮 N×64B 拷贝分配。
func (r *Rotator) removeExpired(files []archiveFile, now time.Time) []archiveFile {
	cutoff := now.AddDate(0, 0, -r.cfg.MaxAge)
	kept := files[:0]
	for _, f := range files {
		if f.mod.Before(cutoff) {
			p := f.path()
			if err := os.Remove(p); err != nil {
				r.warnf("cleanup remove %s: %v", p, err)
			} else {
				r.pruneEmptyDirs(p)
			}
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// matchArchives 按 D4 变体集匹配归档文件并去重（C1 双路径 → C-V2 三路径，
// 2026-11-14/16 性能优化），剔除：当前活跃文件 r.path、cfg.Symlink 本身
// （M4：绝对化比较，防止误删 symlink）、压缩在途的 {源文件, 精确 .gz 目标}
// 对（P1-2/NEW-P1-1：§4.2 步骤 8/9 同轮交错窗口；rec 为本轮 cleanup 入口
// 读取的在途快照，与 evictExcess 的名额预留共用同一视图）、目录与
// Stat/Lstat 失败项。路径选择：pattern 目录部分含 glob meta（cronolog
// 形态，归档跨日期目录分布）→ Unix 走多目录单遍走树（matchArchivesWalk，
// C-V2）、Windows 回退冻结的 matchArchivesGlob（2026-11-16 审查 P1 方案 B，
// 见其 doc）；目录部分为字面量（生产主流平面目录形态，M5 保证字面量无
// *?[）→ 单遍扫描快路径（matchArchivesFlat）。Unix 两条生产路径与冻结的
// glob oracle（matchArchivesGlob，Unix 等价性测试 oracle + Windows 生产
// 路径双重角色）语义逐项一致（见各自 doc）。
func (r *Rotator) matchArchives(rec *compressRecord, now time.Time) []archiveFile {
	active := filepath.Clean(r.path)
	skipLink := r.symlinkAbs()
	dir := filepath.Dir(r.cfg.Pattern)
	if patternDirHasMeta(dir) {
		// 方案 B（2026-11-16 审查 P1，最小变更）：splitWalkShape 硬编码
		// Unix "/" 分隔符，Windows 上 filepath.Dir 经 Clean 归一后返回
		// "\" 路径 → 零切分 → cronolog 清理静默失效（零候选、零告警）。
		// 保守回退冻结的 matchArchivesGlob——filepath.Glob 原生支持
		// Windows 分隔符，是 C-V2 变更前的原生正确行为；Windows 走树
		// 性能从未被 profile。方案 A（平台感知走树）登记为 v2 候选，
		// 见 matchArchivesGlob doc。
		if runtime.GOOS == "windows" {
			return r.matchArchivesGlob(active, skipLink, rec, now)
		}
		return r.matchArchivesWalk(active, skipLink, rec, now)
	}
	return r.matchArchivesFlat(dir, active, skipLink, rec, now)
}

// candidateAccepted 报告候选 m 是否通过全部排除判定（活跃文件 / M4
// symlink 槽位 / P1-2 在途压缩对），回退路径使用；快路径以等价的名字级
// 预计算判定替代（见 matchArchivesFlat），避免逐候选拼接完整路径。
func (r *Rotator) candidateAccepted(m, active, skipLink string, rec *compressRecord) bool {
	if filepath.Clean(m) == active {
		return false
	}
	if skipLink != "" {
		if abs, err := filepath.Abs(m); err == nil && abs == skipLink {
			return false
		}
	}
	if rec != nil && compressInFlight(filepath.Clean(m), rec) {
		return false
	}
	return true
}

// matchArchivesFlat 是平面目录快路径（C1，生产主流形态：pattern 目录部分
// 为字面量）：os.Open(dir) 一次 + File.Readdir(-1)（infos 模式，内部经
// fstatat(dirfd, basename) 以 lstat 语义逐条目取 FileInfo，零路径拼接）
// 替代 3×filepath.Glob 的 3 轮目录 open+readdir+close 与后续 2~3N 轮
// 独立 stat；D4 三个变体以 filepath.Match 在内存中匹配基名（fnmatch CPU
// 占比可忽略——profile 实测 0.4%）。syscall 总量从 3×(open+readdir+close)
// + 2~3N 降至 1×(open+readdir+close) + N×fstatat。
//
// 排除判定按名字级预计算（与 candidateAccepted 的全路径比较数学等价：
// entry 基名不含分隔符，Clean(Join(dir,name))≡Join(Clean(dir),name)，
// 相等判定可分解为目录段+基名段独立比较），扫描热循环零字符串分配；
// 完整路径仅对被删除/告警的少数文件惰性构造（archiveFile.path）。
// 与回退路径的两处已证等价差异：① infos 模式对目录内全部条目 lstat
// （含不匹配者；lstat 成本低，换取省掉 2 轮完整目录扫描）；② readdir
// 与 lstat 间隙消失的文件被 Readdir 静默跳过（旧路径为 warn+排除，
// 净效果一致：不删、不计数）。ReadDir 失败仅警告（§4.8）并按已读到的
// 部分继续——候选缺失方向是"少删"（安全侧）。
func (r *Rotator) matchArchivesFlat(dir, active, skipLink string, rec *compressRecord, now time.Time) []archiveFile {
	variants := archiveBaseGlobs(r.cfg.Pattern)
	// 变体模式合法性预检一次（对齐回退路径 Glob 的坏模式告警语义；
	// M5 校验下生产不可达）。
	for _, v := range variants {
		if _, err := filepath.Match(v, ""); err != nil {
			r.warnf("cleanup match %s: %v", v, err)
		}
	}
	f, err := os.Open(dir)
	if err != nil {
		r.warnf("cleanup readdir %s: %v", dir, err)
		return nil
	}
	// 刻意使用已弃用的 File.Readdir（infos 模式）而非 File.ReadDir：
	// 本路径需要**每个 entry 的完整 FileInfo**（mtime 判定），Readdir 内部
	// 经 fstatat(dirfd, basename) 直接取 lstat 结果（3 alloc/文件：name
	// 字符串+syscall 路径转换+fileStat）；ReadDir 返回 DirEntry，其 Info()
	// 需再拼接 parent+"/"+name 全路径后走 Lstat（实测 6 alloc/文件，
	// Scan-N1000 allocs 6040 vs 3030）。弃用告示面向"只需名字/类型"的
	// 常规场景，本场景恰是其例外；os.Root 无 ReadDir（go1.26），双句柄
	// 替代（Readdirnames+Root.Lstat）分配相同但每轮 cleanup 多付一对
	// open/close（darwin 实测 ~60-90µs）。Go 1 兼容性承诺保障该 API 不移除。
	infos, err := f.Readdir(-1)
	if cerr := f.Close(); cerr != nil {
		r.warnf("cleanup readdir close %s: %v", dir, cerr)
	}
	if err != nil {
		r.warnf("cleanup readdir %s: %v", dir, err)
	}

	// 名字级排除基准预计算（等价性见函数 doc）。
	cleanDir := filepath.Clean(dir)
	activeBase := filepath.Base(active)
	activeHere := filepath.Dir(active) == cleanDir
	var skipBase, skipDir string
	if skipLink != "" {
		skipBase = filepath.Base(skipLink)
		skipDir = filepath.Dir(skipLink)
		if absDir, aerr := filepath.Abs(dir); aerr != nil || absDir != skipDir {
			// Abs 失败或目录不同：与旧实现"Abs(m)≠skipLink 不跳过"一致
			skipBase = ""
		}
	}
	var srcBase, gzBase string
	srcHere, gzHere := false, false
	if rec != nil {
		srcHere = filepath.Dir(rec.src) == cleanDir
		srcBase = filepath.Base(rec.src)
		gzHere = filepath.Dir(rec.gz) == cleanDir
		gzBase = filepath.Base(rec.gz)
	}

	files := make([]archiveFile, 0, len(infos))
	for _, fi := range infos {
		name := fi.Name()
		if !matchAnyBase(variants, name) {
			continue
		}
		// 目录排除：lstat 视角 IsDir（与旧路径 Lstat 后判 IsDir 一致）。
		// symlink 指向的目录不在此排除——lstat 视角它是链接，与旧路径
		// 行为一致地纳入候选（删除的是链接本身）。
		if fi.IsDir() {
			continue
		}
		if activeHere && name == activeBase {
			continue
		}
		if skipBase != "" && name == skipBase {
			continue
		}
		if rec != nil && ((srcHere && name == srcBase) || (gzHere && name == gzBase)) {
			continue
		}
		mod := fi.ModTime()
		if fi.Mode()&os.ModeSymlink != 0 {
			// symlink：跟随目标取 mtime（与旧实现排序/过期阶段 os.Stat
			// 语义一致）；悬空 symlink Stat 失败 → 警告并排除，保全
			// "无法确认时序的文件不删"语义。
			st, ok := r.statFollow(filepath.Join(dir, name))
			if !ok {
				continue
			}
			mod = st.ModTime()
		}
		clampedMod, clamped := clampModTime(mod, now)
		files = append(files, archiveFile{dir: dir, name: name, mod: clampedMod, clamped: clamped})
	}
	return files
}

// matchAnyBase 报告基名 name 是否命中任一 D4 基名变体。坏模式的 Match
// 错误已在 matchArchivesFlat 入口预检告警，此处按未命中处理。
func matchAnyBase(variants []string, name string) bool {
	for _, v := range variants {
		if ok, _ := filepath.Match(v, name); ok {
			return true
		}
	}
	return false
}

// statFollow 跟随 symlink 获取目标 FileInfo；失败（悬空链接等）仅警告，
// 调用方将候选排除出删除集。
func (r *Rotator) statFollow(m string) (os.FileInfo, bool) {
	fi, err := os.Stat(m)
	if err != nil {
		r.warnf("cleanup stat %s: %v", m, err)
		return nil, false
	}
	return fi, true
}

// walkMetaChars 是走树形状解析的段元字符集，与 filepath 内部 hasMeta 的
// Unix 集合 *?[\ 一致：\ 在 Match 中是转义符、属"元"字符——含 \ 的字面量
// 段必须与 Glob 一致地落入段模式（经 filepath.Match 转义语义匹配）而非
// 字面量前缀，否则该形态下双路径语义分叉。M5 已保证字面量段不含 *?[，
// 故该集合的实际来源为 token 替换派生的 * 与字面量残留的 \。
// 2026-11-17 审计 R5-P1：M5 禁集在 Unix 已补 \，"字面量残留的 \" 场景在
// Unix 生产不可达（启动即拒绝）；集合保留用于白盒保守判定，常量值不动。
const walkMetaChars = `*?[\`

// walkDir 是 matchArchivesWalk 显式栈的目录项：dir 为自 root 起逐段拼接的
// 完整路径（与 Glob 输出同构的排序键形态），depth 为已匹配的目录段数
// （depth == len(comps) 即叶层）。
type walkDir struct {
	dir   string
	depth int
}

// splitWalkShape 解析 pattern 的走树形状（C-V2）：root 为目录部分经 token
// 替换后首个含段元字符的路径段之前的字面量前缀（为空时相对路径用 "."、
// 绝对路径用 "/"——与 Glob 递归剥离至 meta-free 前缀后的 cleanGlobPath
// 底座一致），comps 为其后的全部目录段（token 段替换为 *、字面量段原样，
// 统一经 filepath.Match 匹配——与 Glob 逐层拆分的段模式逐位一致；M5
// 保证字面量段无 *?[，Unix 另禁 \，2026-11-17 审计 R5-P1）。token 不含
// 路径分隔符，故"整体替换再分段"与"逐段替换"等价（C1 已论证）。
func splitWalkShape(pattern string) (root string, comps []string) {
	repl := globReplacer.Replace(filepath.Dir(pattern))
	abs := strings.HasPrefix(repl, "/")
	rest := repl
	if abs {
		rest = repl[1:]
	}
	segs := strings.Split(rest, "/")
	meta := 0
	for ; meta < len(segs); meta++ {
		if strings.ContainsAny(segs[meta], walkMetaChars) {
			break
		}
	}
	prefix, comps := segs[:meta], segs[meta:]
	switch {
	case len(prefix) == 0:
		if abs {
			root = "/"
		} else {
			root = "."
		}
	case abs:
		root = "/" + strings.Join(prefix, "/")
	default:
		root = strings.Join(prefix, "/")
	}
	return root, comps
}

// matchComponent 报告中间层目录段候选 name 是否命中段模式 comp（token
// 替换后的 glob 形态；\ 保留 Match 转义语义——与 Glob 逐层 Match 一致）。
// 坏模式的 Match 错误已在 matchArchivesWalk 入口预检告警，此处按未命中
// 处理（matchAnyBase 同款容错）。
func matchComponent(comp, name string) bool {
	ok, _ := filepath.Match(comp, name)
	return ok
}

// matchArchivesWalk 是 cronolog 形态（pattern 目录部分含 strftime 占位符，
// 归档跨日期目录分布）的多目录单遍生产路径（C-V2，2026-11-16 性能优化；
// 仅 Unix——Windows 分发回退 matchArchivesGlob，方案 B 见其 doc）：
// 显式栈 DFS 一次覆盖整棵日期树——每目录恰好一次 open+Readdir(-1)+close，
// 机械复用 matchArchivesFlat 已投产的选型（fstatat(dirfd, basename) lstat
// 语义 FileInfo，3 alloc/条目），替代 3×filepath.Glob 的 3 轮全树遍历、
// 每轮前置的 os.Stat 存在性轮与逐候选 Lstat。叶层对每个条目做 D4 三变体
// 基名匹配（matchAnyBase），一次走树同时收割全部变体（重复命中天然去重：
// 每条目至多入栈一次，无 glob 路径 seen map 的必要）。
//
// 语义输出与 matchArchivesGlob 逐位等价（等价性测试以其为冻结 oracle）：
//   - 中间层：段模式匹配后按 IsDir 下降（lstat 视角）；symlink 段经
//     statFollow 跟随判 IsDir——对齐 Glob 前置 os.Stat(dir) 的走查行为，
//     指向目录则下降其内容、悬空/指向文件则不下降（Glob 侧为静默跳过，
//     本路径差异仅为告警）。悬空中间层 symlink 会经 statFollow 告警。
//   - 叶层：变体匹配 + IsDir 排除后，经 candidateAccepted 全路径比较
//     （active / M4 symlink 槽位 / P1-2 在途压缩对，与 glob 路径逐位一致）；
//     archiveFile.name 为完整路径（dir 为空），排序键形态与 glob 输出一致。
//   - 每目录 open/Readdir/Close 失败仅 warnf + continue，部分读取按已有
//     条目继续（§4.8 少删安全侧，对齐 flat 路径）；readdir 与 fstatat 间隙
//     消失的条目被 Readdir 静默跳过（净效果与旧路径一致：不删、不计数）。
//   - 显式栈无递归深度上限——Glob 的 pathSeparatorsLimit（CVE-2022-30632
//     防护）是递归实现的防护细节而非行为契约（M5/M6 启动闸门已限 pattern
//     形态），pattern 目录层级深度即本路径的实际上限。
//
// 单快照纪律：仅消费 matchArchives 入参（now/rec 各一），不新增
// r.now()/compressing.Load() 调用（P1-2/DA-2）；clampModTime 以入口 now
// 贯通，DA-2 钳制与 tie-break 语义同 glob 路径。
func (r *Rotator) matchArchivesWalk(active, skipLink string, rec *compressRecord, now time.Time) []archiveFile {
	variants := archiveBaseGlobs(r.cfg.Pattern)
	root, comps := splitWalkShape(r.cfg.Pattern)
	// 变体/目录段模式合法性预检一次（对齐 flat 序言与 Glob ErrBadPattern
	// 的"警告+跳过"语义；M5/M6 校验下生产不可达）。
	for _, v := range variants {
		if _, err := filepath.Match(v, ""); err != nil {
			r.warnf("cleanup match %s: %v", v, err)
		}
	}
	for _, c := range comps {
		if _, err := filepath.Match(c, ""); err != nil {
			r.warnf("cleanup match %s: %v", c, err)
		}
	}
	leaf := len(comps)
	stack := []walkDir{{dir: root}}
	var files []archiveFile
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		f, err := os.Open(it.dir)
		if err != nil {
			r.warnf("cleanup readdir %s: %v", it.dir, err)
			continue
		}
		infos, rerr := f.Readdir(-1)
		if cerr := f.Close(); cerr != nil {
			r.warnf("cleanup readdir close %s: %v", it.dir, cerr)
		}
		if rerr != nil {
			r.warnf("cleanup readdir %s: %v", it.dir, rerr)
		}
		for _, fi := range infos {
			name := fi.Name()
			if it.depth == leaf {
				// 叶层：归档候选判定（目录与变体同形者排除，两判定
				// 均为纯谓词，序不影响结果集）。
				if !matchAnyBase(variants, name) || fi.IsDir() {
					continue
				}
				path := filepath.Join(it.dir, name)
				if !r.candidateAccepted(path, active, skipLink, rec) {
					continue
				}
				mod := fi.ModTime()
				if fi.Mode()&os.ModeSymlink != 0 {
					// symlink：跟随目标取 mtime（与 glob 路径 Lstat→
					// statFollow 语义一致）；悬空 → 警告并排除。
					st, ok := r.statFollow(path)
					if !ok {
						continue
					}
					mod = st.ModTime()
				}
				clampedMod, clamped := clampModTime(mod, now)
				files = append(files, archiveFile{name: path, mod: clampedMod, clamped: clamped})
			} else if matchComponent(comps[it.depth], name) {
				// 中间层：目录段候选。
				if fi.Mode()&os.ModeSymlink != 0 {
					// symlink 段：跟随 Stat 判 IsDir（对齐 glob 前置
					// os.Stat 走查）；悬空/指向文件不下降。
					child := filepath.Join(it.dir, name)
					if st, ok := r.statFollow(child); ok && st.IsDir() {
						stack = append(stack, walkDir{dir: child, depth: it.depth + 1})
					}
				} else if fi.IsDir() {
					stack = append(stack, walkDir{dir: filepath.Join(it.dir, name), depth: it.depth + 1})
				}
			}
		}
	}
	return files
}

// matchArchivesGlob 是跨目录 glob 路径的原始实现（C-V2 之前的生产路径）：
// 3×filepath.Glob（D4 三变体，每轮全树遍历 + 每目录前置 os.Stat 存在性轮）
// + seen 去重 + 逐候选 Lstat + 目录排除。双重角色（2026-11-16 审查 P1
// 方案 B）：
//   - Unix 等价性测试 oracle：C-V2 起 Unix 生产分发改走 matchArchivesWalk
//     （单遍走树）；本函数**语义冻结**保留，作为双路径等价性测试
//     （TestCleanupCronologWalkGlobEquivalence）的 oracle——勿随生产代码
//     演进，两侧任何漂移都会被等价性断言捕获。
//   - Windows cronolog 形态的生产路径：matchArchives 分发处按
//     runtime.GOOS=="windows" 回退本函数。方案 B 理由：splitWalkShape
//     硬编码 Unix "/"，Windows 的 "\" 路径零切分导致 cronolog 清理静默
//     失效；filepath.Glob 原生支持 Windows 分隔符，是变更前原生正确行为；
//     Windows 走树性能从未被 profile（开发机 darwin 无法运行 Windows 测试）。
//     方案 A（根治：splitWalkShape 平台感知分段（参考 staticDirPrefix 的
//     filepath.Separator||'/' 双分隔符模式）、walkMetaChars 按 GOOS 取
//     filepath hasMeta 的平台集合——Windows 上 Match 的 "\" 是分隔符而非
//     转义）登记为 v2 候选不实施。冻结纪律不因 Windows 生产角色改变：
//     Windows 交付的即冻结语义本身（= C-V2 之前的生产行为），任何改动
//     仍须过等价性测试。
//
// C1 增量为元数据贯通：symlink 在本阶段即走 os.Stat 获取跟随后的
// FileInfo（悬空 symlink Stat 失败 → 警告并排除）。净效果与旧实现一致——
// 旧实现中悬空 symlink 通过 Lstat 进入候选后，才在 removeExpired/
// sortByModTime 的 Stat 阶段失败排除（同样警告、同样不删、同样不计数）；
// 非 symlink 文件 Lstat≡Stat，快照值与旧实现排序/过期阶段所取一致。
func (r *Rotator) matchArchivesGlob(active, skipLink string, rec *compressRecord, now time.Time) []archiveFile {
	seen := make(map[string]struct{})
	var files []archiveFile
	for _, g := range archiveGlobs(r.cfg.Pattern) {
		matches, err := filepath.Glob(g)
		if err != nil {
			r.warnf("cleanup glob %s: %v", g, err)
			continue
		}
		for _, m := range matches {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			if !r.candidateAccepted(m, active, skipLink, rec) {
				continue
			}
			st, err := os.Lstat(m)
			if err != nil {
				r.warnf("cleanup lstat %s: %v", m, err)
				continue
			}
			if st.IsDir() {
				continue
			}
			fi := st
			if st.Mode()&os.ModeSymlink != 0 {
				var ok bool
				if fi, ok = r.statFollow(m); !ok {
					continue
				}
			}
			mod, clamped := clampModTime(fi.ModTime(), now)
			files = append(files, archiveFile{name: m, mod: mod, clamped: clamped})
		}
	}
	return files
}

// staticDirPrefix 返回 dir 的最长静态字面量前缀（N3-B 空目录回收边界）：
// 截断至首个含 '%' 的路径段之前。字面量 '%'（非占位符，如 "100%/"）同样
// 视作截断点——受保护前缀因此收缩（回收尝试范围略放大），安全性由
// os.Remove 空目录语义 + ENOTEMPTY + 切分时 MkdirAll 自愈兜底；M5 已拒绝
// 字面量含 glob 元字符，占位符段与 '%' 判定等价。dir 无占位符（平面形态）
// 时原样返回。
func staticDirPrefix(dir string) string {
	last := 0 // 最近一个路径分隔符位置（前缀截断点）
	for i := 0; i < len(dir); i++ {
		if dir[i] == filepath.Separator || dir[i] == '/' {
			last = i
		} else if dir[i] == '%' {
			return dir[:last]
		}
	}
	return dir
}

// pruneEmptyDirs 对已删除归档 f 的父目录链做 best-effort 向上回收（N3-B，
// 2026-11-15 审计：cronolog 形态空日期目录永不回收 → inode 随年龄线性累积、
// 每轮 Glob 须 readdir 全部历史空目录）。os.Remove 仅删空目录：活跃文件/
// 第三方文件/存活归档所在目录 ENOTEMPTY 即停；不越过 pattern 的静态字面量
// 目录前缀（前缀本身及其祖先绝不删除）。平面形态父目录即字面量前缀，循环
// 天然不执行。失败静默——ENOTEMPTY/ENOENT 是预期的终止语义而非异常（§4.8）。
func (r *Rotator) pruneEmptyDirs(f string) {
	boundary := staticDirPrefix(filepath.Dir(r.cfg.Pattern))
	for d := filepath.Dir(f); d != boundary; d = filepath.Dir(d) {
		if p := filepath.Dir(d); p == d {
			return // 已到根/当前目录：边界及其祖先绝不删除
		}
		if err := os.Remove(d); err != nil {
			return
		}
	}
}

// compressInFlight 报告 m 是否为在途压缩的源文件或其精确 .gz 目标产物
// （P1-2/NEW-P1-1）：排除集精确为 {rec.src, rec.gz} 两元素——gz 名由
// createGzFile 定名后随记录同步发布，半成品产物恒被覆盖。禁止按 ".N.gz"
// 名字家族猜测豁免：家族排除会连带保护已完成的历史归档（src.gz/src.N.gz），
// 固定名+压缩+MaxBackups 模式下可淘汰集永远追不上序号探测的家族增长，
// cleanup 空转、归档无界增长（§14.1-5）。历史 .gz 属正常归档，参与淘汰；
// 纯序号归档（如 src.1，非 gz）同样正常参与清理。
func compressInFlight(m string, rec *compressRecord) bool {
	return m == rec.src || m == rec.gz
}

// symlinkAbs 返回 cfg.Symlink 的绝对路径（M4 比较基准）。
// 未配置 Symlink 或解析失败时返回空串（不参与跳过判定）。
func (r *Rotator) symlinkAbs() string {
	if r.cfg.Symlink == "" {
		return ""
	}
	abs, err := filepath.Abs(r.cfg.Symlink)
	if err != nil {
		r.warnf("cleanup resolve symlink %s: %v", r.cfg.Symlink, err)
		return ""
	}
	return abs
}

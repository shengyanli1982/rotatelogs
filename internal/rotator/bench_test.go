package rotator

// bench_test.go —— 主路径基准套件（性能工程阶段 1，2026-11-13）。
//
// 测量纪律（对应 Orchestrator 注入的项目测量规则）：
//   - 硬指标优先：allocs/op 与 B/op 是确定性指标（跨进程逐位可比）；本机
//     （darwin/arm64 + APFS）文件系统密集路径 ns/op 跨进程漂移可达 ~38%，
//     ns/op 只作参考，A/B 结论须同进程紧邻交替 -count≥5 取中位数+区间。
//   - setup 后一律 b.ResetTimer()；逐轮拆卸（Close/删除/截断/重建）一律落在
//     b.StopTimer() 窗口内（规则 3/8）；增长型基准循环结束后显式 StopTimer
//     再做最终拆卸。
//   - 磁盘写满防护（规则 9）：无界增长的写入基准周期性 truncate（活跃文件
//     由 New 以 O_APPEND 打开 → 截断后从 0 密集续写，无稀疏空洞）；重建型
//     基准每轮清目录；compress 基准固定 src 体量。稳态磁盘占用 <10MB。
//   - 平台标注（规则 7）：darwin 上 open+close ≈ 30-60× stat 成本（fd 表锁
//     + vnode 锁 + 安全机制），rotate/uniqueName/compress 系列 ns/op 绝对值
//     禁止外推 Linux。
//   - 并发口径：Rotator 的 Write/Close/Flush 由 mu 保护、可并发调用（见包
//     doc，2026-11-16 心跳 idle flush 变更）；生产写入生产者仍为单一，故
//     本套件仍刻意不提供 RunParallel 基准。harness 直调 mu 保护字段
//     （buf/file/size/path 等）必持 r.mu——StopTimer 只停计时，不停 New
//     启动的心跳 flusher goroutine。
//   - 经接口（io.Writer）的写入无 DCE 风险；formatName 为纯函数，以包级
//     sink 防编译器死代码消除。
//
// 口径总览（各基准 doc 注释含细节）：
//   - BenchmarkWrite / WriteAmortizedRotate / WriteSplitBoundary：单 op 延迟
//   - BenchmarkRotate：单次 rotate 延迟（稳态，归档规模敏感度）
//   - BenchmarkCleanup：单次 cleanup 扫描延迟（零突变可重复口径）
//   - BenchmarkFormatName：纯 CPU 单次调用延迟
//   - BenchmarkUniqueName：单次探测链延迟（O(k) 探测成本量化）
//   - BenchmarkCompressStream：聚合吞吐（b.SetBytes → MB/s）

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

// benchFixedNow 是注入型固定时钟（2026-11-13T08:00:00Z）：保证 formatName /
// boundaryAfter / removeExpired 的时间输入跨进程确定，且与真实时钟同日、
// 早于基准运行时刻之后产生的任何文件 mtime。
func benchFixedNow() time.Time {
	return time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
}

// benchLogData 生成确定性伪随机真实感日志文本（~110-130B/行：时间戳+级别+
// 模块+kv 对）：供 compress 基准（真实日志可压缩但非退化数据）与写入负载
// 切片复用。固定 PCG 种子 → 跨进程逐字节一致。
func benchLogData(lines int) []byte {
	rng := rand.New(rand.NewPCG(42, 7))
	levels := [4]string{"INFO", "WARN", "ERROR", "DEBUG"}
	urls := [4]string{"/api/v1/items", "/api/v1/orders", "/healthz", "/api/v2/search"}
	var sb strings.Builder
	sb.Grow(lines * 120)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&sb, "2026-11-13T%02d:%02d:%02d.%03dZ %s [svc.handler] request completed id=%06d dur_ms=%d path=%s\n",
			rng.IntN(24), rng.IntN(60), rng.IntN(60), rng.IntN(1000),
			levels[rng.IntN(4)], i, rng.IntN(500), urls[rng.IntN(4)])
	}
	return []byte(sb.String())
}

// benchNew 创建基准专用 Rotator，失败即 Fatal。
func benchNew(tb testing.TB, cfg Config) *Rotator {
	tb.Helper()
	r, err := New(cfg)
	if err != nil {
		tb.Fatalf("benchNew: %v", err)
	}
	return r
}

// benchTouch 创建（或截断重建）空文件并钉住 mtime。
func benchTouch(tb testing.TB, path string, mtime time.Time) {
	tb.Helper()
	if err := os.WriteFile(path, nil, filePerm); err != nil {
		tb.Fatalf("benchTouch write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		tb.Fatalf("benchTouch chtimes %s: %v", path, err)
	}
}

// benchWipeDir 清空目录内全部条目（目录本身保留）。
func benchWipeDir(tb testing.TB, dir string) {
	tb.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatalf("benchWipeDir read %s: %v", dir, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			tb.Fatalf("benchWipeDir remove %s: %v", e.Name(), err)
		}
	}
}

// benchTruncateActive 将活跃文件与 size 计数归零。调用方必须处于
// b.StopTimer() 窗口内。仅对 New 打开的 O_APPEND 活跃文件安全（截断后
// 内核写入偏移即文件尾 → 密集续写）；uniqueName 创建的 O_WRONLY 句柄
// 截断会产生稀疏空洞，重建型基准应改用 Close+wipe+New。
// buf/file/size 为 mu 保护字段，直调全程持 mu（心跳 flusher 并发锁内读
// buf；本函数在 StopTimer 窗内，锁零测量代价）。
func benchTruncateActive(tb testing.TB, r *Rotator) {
	tb.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.buf.Flush(); err != nil {
		tb.Fatalf("benchTruncateActive flush: %v", err)
	}
	if err := r.file.Truncate(0); err != nil {
		tb.Fatalf("benchTruncateActive truncate: %v", err)
	}
	r.size = 0
}

// benchSeedArchives 预置 n 个匹配 D4 glob "app-*-*-*.log" 的归档文件，
// mtime 相对真实时钟递减排列且全部早于基准启动 ≥60 分钟：保证 cleanup
// 淘汰受害者（最旧者）恒为 seed[0]，而 rotate 新产生的归档（真实当下
// mtime）恒为最新——稳态确定、与真实时钟无耦合。返回 seed[0] 的路径与
// mtime（供 Full 变体拆卸时恢复受害者）。
func benchSeedArchives(tb testing.TB, dir string, n int) (victim string, victimMtime time.Time) {
	tb.Helper()
	base := time.Now()
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("app-2020-%d-%d.log", i/31+1, i%31+1))
		mt := base.Add(-time.Duration(n-i+60) * time.Minute)
		benchTouch(tb, p, mt)
		if i == 0 {
			victim, victimMtime = p, mt
		}
	}
	return victim, victimMtime
}

// BenchmarkWrite 度量 Write 热路径的单 op 延迟（无 rotate 稳态）：负载经
// bufio 拷入 64KB 缓冲，缓冲满时 flush 一次 64KB write syscall（摊销）。
// 守卫对照三角分离 needRotate 每次写入的固定开销：
//   - ProdGuards：生产默认形态（MaxSize=100MB + RotationTime=24h + 真实
//     时钟 Now=nil）→ 每次 Write 触发 r.now()=time.Now()+UTC 归一+Before；
//   - FixedClock：同守卫但注入常量时钟 → 与 ProdGuards 之差 ≈ time.Now
//     syscall/commpage 读取成本；
//   - SizeOnly：RotationTime=0 → needRotate 短路不触时钟（守卫下限参照），
//     与 FixedClock 之差 ≈ 注入闭包调用+Before 比较+分支成本。
//
// 周期 truncate（StopTimer 内）控制磁盘占用 ≤1MB（100B）/≤4MB（64KB）。
func BenchmarkWrite(b *testing.B) {
	data := benchLogData(64)
	line := make([]byte, 100)
	copy(line, data)
	large := make([]byte, WriteBufferSize)
	for off := 0; off < len(large); off += len(data) {
		copy(large[off:], data)
	}

	cases := []struct {
		name       string
		pattern    string
		payload    []byte
		truncEvery int
		maxSize    int64
		rotTime    time.Duration
		now        func() time.Time
	}{
		{"100B-ProdGuards", "app-%Y-%m-%d.log", line, 4096, 100 << 20, 24 * time.Hour, nil},
		{"100B-FixedClock", "app-%Y-%m-%d.log", line, 4096, 100 << 20, 24 * time.Hour, benchFixedNow},
		{"100B-SizeOnly", "app.log", line, 4096, int64(1) << 40, 0, nil},
		{"64KB-ProdGuards", "app-%Y-%m-%d.log", large, 64, 100 << 20, 24 * time.Hour, nil},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			r := benchNew(b, Config{
				Pattern:      filepath.Join(dir, tc.pattern),
				MaxSize:      tc.maxSize,
				RotationTime: tc.rotTime,
				Now:          tc.now,
			})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i > 0 && i%tc.truncEvery == 0 {
					b.StopTimer()
					benchTruncateActive(b, r)
					b.StartTimer()
				}
				if _, err := r.Write(tc.payload); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkWriteAmortizedRotate 度量 100B 小写 + MaxSize=64KB 的摊销延迟
// （≈每 655 次 Write 触发 1 次 rotate，含完整 rotate 机械：flush/close/
// formatName/mkdir/uniqueName 探测/open/buf.Reset 缓冲复用（C3，不再每次
// rotate 分配 64KB）；不含 cleanup——MaxBackups=0）。每 1024 op 在 StopTimer 内全量重建（Close+wipe+New），
// 抑制归档数与 uniqueName 探测深度增长（周窗口内 ≤2 次 rotate → 探测 ≤3）。
// 生产口径换算：CLI --max-size 最小 1MB（单位 MB）→ 生产下限的摊销
// rotate 成本 ≈ 本基准与 BenchmarkWrite/100B 差值 × (64KB/1MB) = ÷16。
// 与 BenchmarkWrite/100B-* 的差值即"有 rotate vs 无 rotate"对照。
func BenchmarkWriteAmortizedRotate(b *testing.B) {
	dir := b.TempDir()
	cfg := Config{Pattern: filepath.Join(dir, "app.log"), MaxSize: WriteBufferSize}
	line := make([]byte, 100)
	copy(line, benchLogData(1))
	const resetEvery = 1024
	r := benchNew(b, cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%resetEvery == 0 {
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
			benchWipeDir(b, dir)
			r = benchNew(b, cfg)
			b.StartTimer()
		}
		if _, err := r.Write(line); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := r.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkWriteSplitBoundary 度量单 op = 从 size=0 状态 Write(MaxSize+100B)
// → 恰好触发一次 D1 边界分片（§4.2）：写满 64KB → 1 次 rotate → 续写
// 100B 至新文件。逐轮在 StopTimer 内全量重建状态（Close+wipe+New），
// 保证每 op 单 rotate 确定、无 uniqueName 探测增长、无稀疏空洞。
// darwin 平台标注：单 op 含多次 open/close 系统调用，禁止外推 Linux。
func BenchmarkWriteSplitBoundary(b *testing.B) {
	dir := b.TempDir()
	cfg := Config{Pattern: filepath.Join(dir, "app.log"), MaxSize: WriteBufferSize}
	payload := make([]byte, WriteBufferSize+100)
	seg := benchLogData(8)
	for off := 0; off < len(payload); off += len(seg) {
		copy(payload[off:], seg)
	}
	var r *Rotator
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if r != nil {
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		}
		benchWipeDir(b, dir)
		r = benchNew(b, cfg)
		b.StartTimer()
		if _, err := r.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if r != nil {
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRotate 度量单次 rotate 的稳态延迟。子基准矩阵：
//   - Bare：flush+close+formatName+mkdir+uniqueName 探测+open+buf.Reset
//     缓冲复用（C3，无 64KB 分配；无 cleanup、无 symlink）——rotate 机械基线；
//   - Symlink：Bare + §4.5 原子 symlink 替换（lstat 槽位+remove tmp+
//     symlink+rename）；
//   - Full/N：Symlink + cleanup（MaxBackups=N，预置 N 个归档 → 每轮扫描
//     N+1 候选、淘汰最旧 seed[0]；拆卸恢复受害者+移除旧活跃 → 目录状态
//     恒定，uniqueName 在 base↔base.1 振荡、探测深度 ≤2）。
//
// 压缩刻意排除：compressAsync 为后台 goroutine 且 skip-if-busy 语义引入
// 非确定性、污染 CPU profile；其成本由 BenchmarkCompressStream 独立度量，
// 生产叠加时按"rotate-Full + 异步压缩吞吐"合成。
// darwin 平台标注：单 op 含多次 open/close/rename syscall，禁止外推 Linux。
func BenchmarkRotate(b *testing.B) {
	if runtime.GOOS == "windows" {
		b.Skip("rotate 基准矩阵依赖 symlink 语义（updateSymlink 在 windows 降级），跳过")
	}
	cases := []struct {
		name       string
		symlink    bool
		seeds      int
		maxBackups int // 0 = 关闭 cleanup
	}{
		{"Bare", false, 0, 0},
		{"Symlink", true, 0, 0},
		{"Full-N10", true, 10, 10},
		{"Full-N100", true, 100, 100},
		{"Full-N1000", true, 1000, 1000},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
				MaxSize:    int64(1) << 40,
				Now:        benchFixedNow,
				MaxBackups: tc.maxBackups,
			}
			if tc.symlink {
				cfg.Symlink = filepath.Join(dir, "current.log")
			}
			victim, victimMtime := benchSeedArchives(b, dir, tc.seeds)
			r := benchNew(b, cfg)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// 直调 rotate 持 mu：生产 rotate 本就在 Write 持锁下执行，且
				// flusher 并发锁内读 buf。先 Unlock 再 Fatal，防 Goexit 留锁。
				r.mu.Lock()
				old := r.path
				err := r.rotate()
				r.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				// 移除本轮新归档（旧活跃文件）使 uniqueName 探测深度归零；
				// Full 变体再恢复被 cleanup 淘汰的最旧 seed（严格 Fatal：
				// 若受害者非预期导致 old 已被删，说明稳态假设破裂）。
				if err := os.Remove(old); err != nil {
					b.Fatalf("teardown remove %s: %v", old, err)
				}
				if victim != "" {
					benchTouch(b, victim, victimMtime)
				}
				b.StartTimer()
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkCleanup 度量单次 cleanup 的扫描延迟（零突变口径）：
// MaxBackups=N+1 → evictExcess 全保留，循环内无文件删除 → 无需逐轮拆卸、
// 完全可重复。覆盖 matchArchives（3×glob readdir + N×lstat + map 去重）
// +sortByModTime（N×stat + slices.SortFunc）全链路。
// ScanAge 变体叠加 MaxAge=36500 天（cutoff≈1926 年，无文件过期）→
// removeExpired 追加一轮 N×stat；ScanAge−Scan 差值量化"同一批文件被
// 多轮 stat"的边际成本（生产同时配置 --max-age 与 --max-backups 时成立）。
// 口径：单次调用延迟；生产仅 rotate 事件后机会式触发（§4.3，低频）。
func BenchmarkCleanup(b *testing.B) {
	cases := []struct {
		name   string
		seeds  int
		maxAge int
	}{
		{"Scan-N10", 10, 0},
		{"Scan-N100", 100, 0},
		{"Scan-N1000", 1000, 0},
		{"ScanAge-N10", 10, 36500},
		{"ScanAge-N100", 100, 36500},
		{"ScanAge-N1000", 1000, 36500},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
				MaxSize:    int64(1) << 40,
				Now:        benchFixedNow,
				MaxBackups: tc.seeds + 1, // keep-all：候选恒为 N ≤ keep → 零淘汰
				MaxAge:     tc.maxAge,
			}
			benchSeedArchives(b, dir, tc.seeds)
			r := benchNew(b, cfg)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// cleanup 读 mu 保护的 r.path，直调持 mu 求统一（锁不分配，
				// uncontended 开销对扫描延迟可忽略）。
				r.mu.Lock()
				r.cleanup()
				r.mu.Unlock()
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// benchNameSink 防止编译器对 formatName 纯函数结果做死代码消除（DCE）。
var benchNameSink string

// benchSeedCronologArchives 预置 n 个 cronolog 形态（%Y/%m/%d/access.log，
// 每个日期目录一个归档）嵌套归档：i 唯一分解为 年/月/日（2020 年起按日
// 倒排），mtime 相对 benchFixedNow 递减且全部未过期——配合 keep-all 构成
// 零突变口径，无需受害者恢复拆卸。
func benchSeedCronologArchives(tb testing.TB, dir string, n int) {
	tb.Helper()
	base := benchFixedNow()
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%d", 2020-i/336),
			fmt.Sprintf("%02d", (i/28)%12+1), fmt.Sprintf("%02d", i%28+1), "access.log")
		if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
			tb.Fatalf("benchSeedCronologArchives mkdir %s: %v", p, err)
		}
		benchTouch(tb, p, base.Add(-time.Duration(n-i+60)*time.Minute))
	}
}

// BenchmarkCleanupCronolog 度量 cronolog 嵌套目录形态下单次 cleanup 的扫描
// 延迟——跨目录 glob 回退路径（matchArchivesGlob）此前无任何基准直接驱动
// （INFO-C 测量基建，2026-11-15 性能报告 §2.3 缺口 1）。覆盖 3×filepath.Glob
// 跨全部日期目录遍历 + seen 去重 + 逐候选 Lstat + sortByModTime 全链路。
// 口径与 BenchmarkCleanup 一致：MaxBackups=N+1 keep-all 零突变、Config.Now
// 注入时钟零 sleep、无拆卸完全可重复；活跃文件位于当前日期目录（名字排除，
// 不计入候选）。价值：为"是否值得把平面快路径推广为多目录单遍扫描"提供
// 决策数据。darwin 平台标注（规则 7）：Glob 为目录 open/readdir 密集路径，
// ns/op 禁止外推 Linux；allocs/B 为确定性指标。
func BenchmarkCleanupCronolog(b *testing.B) {
	cases := []struct {
		name  string
		seeds int
	}{
		{"Scan-N100", 100},
		{"Scan-N1000", 1000},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				Pattern:    filepath.Join(dir, "%Y", "%m", "%d", "access.log"),
				MaxSize:    int64(1) << 40,
				Now:        benchFixedNow,
				MaxBackups: tc.seeds + 1, // keep-all：候选恒为 N ≤ keep → 零淘汰
			}
			benchSeedCronologArchives(b, dir, tc.seeds)
			r := benchNew(b, cfg)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// 直调 cleanup 持 mu（读 r.path，与 BenchmarkCleanup 同纪律）。
				r.mu.Lock()
				r.cleanup()
				r.mu.Unlock()
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkFormatName 度量 formatName 的纯 CPU 单次延迟（无 I/O）：
// Literal 走无占位符短路（ContainsRune 扫描后原样返回）；Daily 为生产
// 常见形态（3 个时间 token → 3 次 t.Format）；Cronolog 为目录归档形态
// （4 token + 路径分层）。生产每次 rotate 仅调用一次——低频路径，
// 绝对值供归因参照，不构成独立优化立项依据（反模式守则：不优化低频操作）。
func BenchmarkFormatName(b *testing.B) {
	cases := []struct{ name, pattern string }{
		{"Literal", "app.log"},
		{"Daily", "app-%Y-%m-%d.log"},
		{"Cronolog", "%Y/%m/%d/access-%H.log"},
	}
	t := benchFixedNow()
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			r := &Rotator{cfg: Config{Pattern: tc.pattern}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchNameSink = r.formatName(t)
			}
		})
	}
}

// BenchmarkUniqueName 度量单次探测链延迟：预置 n 个已存在文件
// （base, base.1..base.n-1）→ uniqueName(base) 需 n 次 EEXIST 探测 + 1 次
// O_EXCL 成功创建（合计 n+1 次 open syscall）；拆卸（StopTimer 内
// Close+Remove 新产物）恢复 n 个 → 每 op 恒定 O(n)。
// 量化已知 INFO 观察：固定文件名+归档累积场景下探测深度随归档数线性增长，
// allocs/op ≈ 2n（每级探测 fmt.Sprintf 生成候选名）。
// darwin 平台标注（规则 7）：EEXIST 快速失败 open ≈ 目录查找成本，成功
// open+close ≈ 30-60× stat——绝对值禁止外推 Linux。
func BenchmarkUniqueName(b *testing.B) {
	for _, n := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("Existing%d", n), func(b *testing.B) {
			dir := b.TempDir()
			base := filepath.Join(dir, "app.log")
			for i := 0; i < n; i++ {
				p := base
				if i > 0 {
					p = fmt.Sprintf("%s.%d", base, i)
				}
				benchTouch(b, p, time.Now())
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f, err := uniqueName(base)
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if cerr := f.Close(); cerr != nil {
					b.Fatal(cerr)
				}
				if rerr := os.Remove(f.Name()); rerr != nil {
					b.Fatal(rerr)
				}
				b.StartTimer()
			}
			b.StopTimer()
		})
	}
}

// BenchmarkCompressStream 度量单位数据压缩聚合吞吐（b.SetBytes=src 字节数
// → 报告 MB/s）。计时窗含 gzip deflate CPU + 读 src + 写 dst + close dst
// + unlink src；open/定名阶段（openCompressPair + createGzFile 序号探测，
// 生产中每 rotate 一次、~3 个元数据 syscall）落在 StopTimer 内不计。
// 数据为真实感日志文本（~120B/行，可压缩非退化）；实际压缩比采集时记录。
func BenchmarkCompressStream(b *testing.B) {
	cases := []struct {
		name string
		size int
	}{
		{"64KB", 64 << 10},
		{"1MB", 1 << 20},
	}
	full := benchLogData(12000) // ≈1.2MB（>1MB，保证 1MB 变体可切片）
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			if len(full) < tc.size {
				b.Fatalf("benchLogData 产出 %d < %d", len(full), tc.size)
			}
			data := full[:tc.size]
			dir := b.TempDir()
			src := filepath.Join(dir, "app.log")
			dst := src + ".gz"
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if err := os.WriteFile(src, data, filePerm); err != nil {
					b.Fatal(err)
				}
				if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
					b.Fatal(err)
				}
				sf, err := os.Open(src)
				if err != nil {
					b.Fatal(err)
				}
				df, err := os.Create(dst)
				if err != nil {
					sf.Close()
					b.Fatal(err)
				}
				b.StartTimer()
				if err := compressStream(src, sf, df); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
		})
	}
}

// ============================================================================
// 同进程 A/B 基建（阶段 C 性能优化，2026-11-14）
//
// 测量纪律：本机（darwin/arm64 + APFS）文件密集路径 ns/op 跨进程漂移可达
// 37.6%，A/B 结论必须同进程紧邻交替采集。以下 legacy* 函数是 c3b32ba 生产
// 管道的**冻结拷贝（test-only，永不随生产代码演进）**：与生产代码共享不可变
// 助手（clampModTime/compressInFlight/symlinkAbs/archiveGlobs/warnf），冻结
// 语义以 c3b32ba 的 cleanup.go/rotator.go 为准。BenchmarkXxxABOld 跑冻结
// 旧实现、BenchmarkXxxABNew 跑当前实现，-test.count=N 时框架按声明序对每个
// 基准连跑 N 次（实测输出为 Old×N 连续段 → New×N 连续段，**非** Old,New
// 交错——INFO-F，2026-11-16 实测修正原注释的交错声明）；框架 count 序下
// 的 A/B 结论只能跨进程对比，同进程紧邻交替配对由下方手动 harness
// （BenchmarkCleanupAB/RotateAB）提供。
//
// A/A 校验：在 C1/C3 落地前，Old 与 New 两侧行为逐位一致，运行 AB 基准
// 应得 ns 差 <5%（噪声）、allocs/B 完全一致——校验 harness 无顺序偏置。
// ============================================================================

// legacyCleanup 是 c3b32ba cleanup() 的冻结拷贝：在途快照 + 入口单 now
// 快照后走 3×filepath.Glob 匹配、逐轮 os.Stat（removeExpired/sortByModTime
// 各自独立 stat，共 2-3 轮元数据 syscall/文件）。
func legacyCleanup(r *Rotator) {
	rec := r.compressing.Load()
	now := r.now()
	files := legacyMatchArchives(r, rec)
	if r.cfg.MaxAge > 0 {
		files = legacyRemoveExpired(r, files, now)
	}
	if r.cfg.MaxBackups > 0 {
		legacyEvictExcess(r, files, rec != nil, now)
	}
}

// legacyMatchArchives 是 c3b32ba matchArchives 的冻结拷贝（3×Glob + Lstat）。
func legacyMatchArchives(r *Rotator, rec *compressRecord) []string {
	active := filepath.Clean(r.path)
	skipLink := r.symlinkAbs()
	seen := make(map[string]struct{})
	var files []string
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
			if filepath.Clean(m) == active {
				continue
			}
			if skipLink != "" {
				if abs, err := filepath.Abs(m); err == nil && abs == skipLink {
					continue
				}
			}
			if rec != nil && compressInFlight(filepath.Clean(m), rec) {
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
			files = append(files, m)
		}
	}
	return files
}

// legacyEvictExcess 是 c3b32ba evictExcess 的冻结拷贝（keep-- 名额预留）。
func legacyEvictExcess(r *Rotator, files []string, inFlight bool, now time.Time) {
	keep := r.cfg.MaxBackups
	if inFlight {
		keep--
	}
	candidates := legacySortByModTime(r, files, now)
	if n := len(candidates) - keep; n > 0 {
		for _, f := range candidates[:n] {
			if err := os.Remove(f); err != nil {
				r.warnf("cleanup remove %s: %v", f, err)
			}
		}
	}
}

// legacySortByModTime 是 c3b32ba sortByModTime 的冻结拷贝（N×os.Stat +
// DA-2 钳制 + clamped 优先 tie-break + 文件名回退）。
func legacySortByModTime(r *Rotator, files []string, now time.Time) []string {
	type archived struct {
		path    string
		mod     time.Time
		clamped bool
	}
	items := make([]archived, 0, len(files))
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			r.warnf("cleanup stat %s: %v", f, err)
			continue
		}
		mod, clamped := clampModTime(st.ModTime(), now)
		items = append(items, archived{path: f, mod: mod, clamped: clamped})
	}
	slices.SortFunc(items, func(a, b archived) int {
		if c := a.mod.Compare(b.mod); c != 0 {
			return c
		}
		if a.clamped != b.clamped {
			if a.clamped {
				return -1
			}
			return 1
		}
		return strings.Compare(a.path, b.path)
	})
	sorted := make([]string, len(items))
	for i, it := range items {
		sorted[i] = it.path
	}
	return sorted
}

// legacyRemoveExpired 是 c3b32ba removeExpired 的冻结拷贝（N×os.Stat）。
func legacyRemoveExpired(r *Rotator, files []string, now time.Time) []string {
	cutoff := now.AddDate(0, 0, -r.cfg.MaxAge)
	kept := make([]string, 0, len(files))
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			r.warnf("cleanup stat %s: %v", f, err)
			continue
		}
		mod, _ := clampModTime(st.ModTime(), now)
		if mod.Before(cutoff) {
			if err := os.Remove(f); err != nil {
				r.warnf("cleanup remove %s: %v", f, err)
			}
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// rotateLegacy 是 c3b32ba rotate() 的冻结拷贝：步骤 6 保持
// bufio.NewWriterSize（C3 对照面），步骤 9 调 legacyCleanup（C1 对照面）。
func rotateLegacy(r *Rotator) error {
	r.RotateFlag.CompareAndSwap(true, false)
	if err := r.buf.Flush(); err != nil {
		return fmt.Errorf("rotatelogs: flush %s: %w", r.path, err)
	}
	oldPath := r.path
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("rotatelogs: close %s: %w", oldPath, err)
	}
	name := r.formatName(r.now())
	if err := os.MkdirAll(filepath.Dir(name), dirPerm); err != nil {
		return fmt.Errorf("rotatelogs: mkdir %s: %w", filepath.Dir(name), err)
	}
	f, err := uniqueName(name)
	if err != nil {
		return err
	}
	r.file = f
	r.buf = bufio.NewWriterSize(f, WriteBufferSize)
	r.path = f.Name()
	r.size = 0
	r.nextRotate = r.boundaryAfter(r.now())
	if r.cfg.Symlink != "" {
		if err := updateSymlink(r.path, r.cfg.Symlink); err != nil {
			r.warnf("symlink %s -> %s: %v", r.cfg.Symlink, r.path, err)
		}
	}
	if r.cfg.Compress {
		r.compressAsync(oldPath)
	}
	if r.cfg.MaxBackups > 0 || r.cfg.MaxAge > 0 {
		legacyCleanup(r)
	}
	if r.cfg.Verbose {
		fmt.Fprintf(os.Stderr, "rotatelogs: rotated %s -> %s\n", oldPath, r.path)
	}
	return nil
}

// benchmarkCleanupAB 以与 BenchmarkCleanup 完全一致的 setup 矩阵（keep-all
// 零突变口径）驱动注入的 cleanup 实现，供 Old/New 两侧同进程紧邻交替对比。
func benchmarkCleanupAB(b *testing.B, fn func(*Rotator)) {
	cases := []struct {
		name   string
		seeds  int
		maxAge int
	}{
		{"Scan-N10", 10, 0},
		{"Scan-N100", 100, 0},
		{"Scan-N1000", 1000, 0},
		{"ScanAge-N10", 10, 36500},
		{"ScanAge-N100", 100, 36500},
		{"ScanAge-N1000", 1000, 36500},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
				MaxSize:    int64(1) << 40,
				Now:        benchFixedNow,
				MaxBackups: tc.seeds + 1,
				MaxAge:     tc.maxAge,
			}
			benchSeedArchives(b, dir, tc.seeds)
			r := benchNew(b, cfg)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// 两侧同等持 mu（cleanup/legacyCleanup 读 r.path；A/B 公平）。
				r.mu.Lock()
				fn(r)
				r.mu.Unlock()
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkCleanupABOld 以 c3b32ba 冻结管道跑 cleanup 扫描（A/B 旧侧）。
// 声明于 ABNew 之前以保持输出分组：-count=N 时框架先连跑 Old×N 再跑
// New×N（非交错，INFO-F 实测修正）；紧邻交替对照见 BenchmarkCleanupAB。
func BenchmarkCleanupABOld(b *testing.B) { benchmarkCleanupAB(b, legacyCleanup) }

// BenchmarkCleanupABNew 以当前实现跑 cleanup 扫描（A/B 新侧）。
func BenchmarkCleanupABNew(b *testing.B) { benchmarkCleanupAB(b, (*Rotator).cleanup) }

// benchmarkRotateAB 以与 BenchmarkRotate 对应子项完全一致的 setup/拆卸
// （振荡稳态：每轮删旧活跃、Full 变体恢复受害 seed）驱动注入的 rotate
// 实现。矩阵仅含判据涉及的两个形态：Bare（C3 对照）与 Full-N10（C1 尖峰对照）。
func benchmarkRotateAB(b *testing.B, fn func(*Rotator) error) {
	if runtime.GOOS == "windows" {
		b.Skip("rotate 基准矩阵依赖 symlink 语义，跳过")
	}
	cases := []struct {
		name       string
		symlink    bool
		seeds      int
		maxBackups int
	}{
		{"Bare", false, 0, 0},
		{"Full-N10", true, 10, 10},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
				MaxSize:    int64(1) << 40,
				Now:        benchFixedNow,
				MaxBackups: tc.maxBackups,
			}
			if tc.symlink {
				cfg.Symlink = filepath.Join(dir, "current.log")
			}
			victim, victimMtime := benchSeedArchives(b, dir, tc.seeds)
			r := benchNew(b, cfg)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// 两侧同等持 mu（与 BenchmarkRotate 同纪律；A/B 公平）：
				// rotate/rotateLegacy 假定调用方持锁，flusher 并发锁内读 buf。
				r.mu.Lock()
				old := r.path
				err := fn(r)
				r.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := os.Remove(old); err != nil {
					b.Fatalf("teardown remove %s: %v", old, err)
				}
				if victim != "" {
					benchTouch(b, victim, victimMtime)
				}
				b.StartTimer()
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkRotateABOld 以 c3b32ba 冻结 rotate 跑单次 rotate（A/B 旧侧）。
func BenchmarkRotateABOld(b *testing.B) { benchmarkRotateAB(b, rotateLegacy) }

// BenchmarkRotateABNew 以当前实现跑单次 rotate（A/B 新侧）。
func BenchmarkRotateABNew(b *testing.B) { benchmarkRotateAB(b, (*Rotator).rotate) }

// ============================================================================
// 手动紧邻交替 A/B harness（权威 ns A/B 口径）
//
// Go 框架的 -count=N 是"每基准连跑 N 次"（Old×N → New×N），不构成紧邻交替；
// 本 harness 在单个基准函数内做 per-op 配对交替：old→new→old→new…，
// 每 op 手动计时（time.Now，对 ≥100µs 的 op 时钟开销 <0.1%），拆卸落在
// 计时窗外。allocs/B 以 abBlockOps 为批量经 runtime.ReadMemStats 差值
// （Mallocs/TotalAlloc，STW 读取在计时窗外），与框架 B/op 口径一致。
//
// GC 在测量期关闭（debug.SetGCPercent(-1)，逐用例恢复）：消除旧侧大额
// 分配触发的 GC assist/sweep 落入新侧计时窗的交叉污染；关闭 GC 使
// 分配密集侧（旧）更快 → Δ% 是保守估计。
//
// 运行方式：-test.bench 'CleanupAB$|RotateAB$' -test.benchtime=1x
// （框架 run1 校准会使函数体执行两次，ReportMetric 以最后一次为准，
// 零突变/带拆卸复原的口径下重复执行无副作用）。
// ============================================================================

const (
	abSamples  = 101 // 每侧配对样本数（奇数保证中位数取真实样本）
	abBlockOps = 25  // alloc 统计的 ReadMemStats 批量粒度
)

// abSide 汇总单侧测量结果：ns 样本五数概括 + allocs/B（批量中位）。
type abSide struct {
	ns     []float64 // 每 op ns 样本
	allocs []float64 // 每块 allocs/op
	bytes  []float64 // 每块 B/op
}

func (s abSide) medNs() float64 { return abMedian(s.ns) }
func (s abSide) minNs() float64 { return s.ns[0] } // 调用前已排序
func (s abSide) maxNs() float64 { return s.ns[len(s.ns)-1] }

// abMedian 返回样本中位数（先排序，副作用由调用方接受）。
func abMedian(xs []float64) float64 {
	slices.Sort(xs)
	return xs[len(xs)/2]
}

// runABAlternating 执行 abSamples 轮 old/new 紧邻配对交替测量。
// oldOp/newOp 为单 op 函数；teardown 在每次 op 的计时窗外调用（可为 nil）。
// 返回 (old, new) 两侧汇总；ns 样本已排序。
func runABAlternating(b *testing.B, oldOp, newOp, teardown func()) (abSide, abSide) {
	b.Helper()
	newSide := func() abSide {
		return abSide{
			ns:     make([]float64, 0, abSamples),
			allocs: make([]float64, 0, abSamples/abBlockOps+1),
			bytes:  make([]float64, 0, abSamples/abBlockOps+1),
		}
	}
	oldS, newS := newSide(), newSide()

	runBlock := func(side *abSide, op func()) {
		var m0, m1 runtime.MemStats
		blockStart := len(side.ns)
		runtime.ReadMemStats(&m0)
		for i := blockStart; i < blockStart+abBlockOps; i++ {
			t0 := time.Now()
			op()
			side.ns = append(side.ns, float64(time.Since(t0).Nanoseconds()))
			if teardown != nil {
				teardown()
			}
		}
		runtime.ReadMemStats(&m1)
		side.allocs = append(side.allocs, float64(m1.Mallocs-m0.Mallocs)/abBlockOps)
		side.bytes = append(side.bytes, float64(m1.TotalAlloc-m0.TotalAlloc)/abBlockOps)
	}

	// 尾部不足一块的样本只计 ns（allocs 批量差值需要整块）。
	fullBlocks := abSamples / abBlockOps
	tail := abSamples - fullBlocks*abBlockOps
	gcRestore := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcRestore)
	for blk := 0; blk < fullBlocks; blk++ {
		runBlock(&oldS, oldOp) // old 先行：紧邻交替 old→new→old→new…
		runBlock(&newS, newOp)
	}
	if tail > 0 {
		runTail := func(side *abSide, op func()) {
			for i := 0; i < tail; i++ {
				t0 := time.Now()
				op()
				side.ns = append(side.ns, float64(time.Since(t0).Nanoseconds()))
				if teardown != nil {
					teardown()
				}
			}
		}
		runTail(&oldS, oldOp)
		runTail(&newS, newOp)
	}
	abMedian(oldS.ns) // 排序（返回值即中位）
	abMedian(newS.ns)
	return oldS, newS
}

// reportAB 以自定义指标输出 A/B 汇总：old/new ns 中位、Δ%（负=新侧更快）、
// allocs 与 B/op（批量中位）。逐轮明细经 b.Logf 输出（-test.v 可见）。
func reportAB(b *testing.B, label string, oldS, newS abSide) {
	b.Helper()
	oldMed, newMed := oldS.medNs(), newS.medNs()
	delta := (newMed - oldMed) / oldMed * 100
	b.ReportMetric(oldMed, "old-ns/op")
	b.ReportMetric(newMed, "new-ns/op")
	b.ReportMetric(delta, "Δ%")
	if len(oldS.allocs) > 0 {
		b.ReportMetric(abMedian(slices.Clone(oldS.allocs)), "old-allocs")
		b.ReportMetric(abMedian(slices.Clone(newS.allocs)), "new-allocs")
		b.ReportMetric(abMedian(slices.Clone(oldS.bytes)), "old-B")
		b.ReportMetric(abMedian(slices.Clone(newS.bytes)), "new-B")
	}
	b.Logf("%s A/B: old med=%.0f ns [%.0f, %.0f] | new med=%.0f ns [%.0f, %.0f] | Δ=%+.1f%%",
		label, oldMed, oldS.minNs(), oldS.maxNs(), newMed, newS.minNs(), newS.maxNs(), delta)
}

// BenchmarkCleanupAB 紧邻交替 A/B：c3b32ba 冻结 cleanup 管道 vs 当前实现。
// 与 BenchmarkCleanup 同 setup 矩阵（keep-all 零突变，无拆卸）。
func BenchmarkCleanupAB(b *testing.B) {
	cases := []struct {
		name   string
		seeds  int
		maxAge int
	}{
		{"Scan-N10", 10, 0},
		{"Scan-N100", 100, 0},
		{"Scan-N1000", 1000, 0},
		{"ScanAge-N10", 10, 36500},
		{"ScanAge-N100", 100, 36500},
		{"ScanAge-N1000", 1000, 36500},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
				MaxSize:    int64(1) << 40,
				Now:        benchFixedNow,
				MaxBackups: tc.seeds + 1,
				MaxAge:     tc.maxAge,
			}
			benchSeedArchives(b, dir, tc.seeds)
			r := benchNew(b, cfg)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				oldS, newS := runABAlternating(b,
					func() {
						r.mu.Lock()
						legacyCleanup(r)
						r.mu.Unlock()
					},
					func() {
						r.mu.Lock()
						r.cleanup()
						r.mu.Unlock()
					},
					nil)
				reportAB(b, "Cleanup/"+tc.name, oldS, newS)
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkRotateAB 紧邻交替 A/B：c3b32ba 冻结 rotate vs 当前实现。
// 与 BenchmarkRotate 对应子项同 setup/拆卸（振荡稳态）。矩阵：Bare（C3
// 对照）与 Full-N10（C1 尖峰对照）。
func BenchmarkRotateAB(b *testing.B) {
	if runtime.GOOS == "windows" {
		b.Skip("rotate 基准矩阵依赖 symlink 语义，跳过")
	}
	cases := []struct {
		name       string
		symlink    bool
		seeds      int
		maxBackups int
	}{
		{"Bare", false, 0, 0},
		{"Full-N10", true, 10, 10},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				Pattern:    filepath.Join(dir, "app-%Y-%m-%d.log"),
				MaxSize:    int64(1) << 40,
				Now:        benchFixedNow,
				MaxBackups: tc.maxBackups,
			}
			if tc.symlink {
				cfg.Symlink = filepath.Join(dir, "current.log")
			}
			victim, victimMtime := benchSeedArchives(b, dir, tc.seeds)
			r := benchNew(b, cfg)
			// 拆卸（计时窗外）：移除本轮旧活跃文件使 uniqueName 探测归零，
			// Full 变体恢复被 cleanup 淘汰的最旧 seed——与 BenchmarkRotate 一致。
			var oldPath string
			teardown := func() {
				if err := os.Remove(oldPath); err != nil {
					b.Fatalf("teardown remove %s: %v", oldPath, err)
				}
				if victim != "" {
					benchTouch(b, victim, victimMtime)
				}
			}
			// 两侧同等持 mu（与 benchmarkRotateAB 同纪律；A/B 公平）：
			// rotate/rotateLegacy 假定调用方持锁，flusher 并发锁内读 buf。
			oldOp := func() {
				r.mu.Lock()
				oldPath = r.path
				err := rotateLegacy(r)
				r.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
			}
			newOp := func() {
				r.mu.Lock()
				oldPath = r.path
				err := r.rotate()
				r.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				oldS, newS := runABAlternating(b, oldOp, newOp, teardown)
				reportAB(b, "Rotate/"+tc.name, oldS, newS)
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// ============================================================================
// 心跳 flush 子系统基准（r4 性能报告 §3 缺口 G1+G2 / #C-1，2026-11-16）
//
// e1aaafc 引入的心跳 idle flush 子系统此前零基准覆盖（仅注入时钟功能测试）。
// 本段补齐两条路径的 per-call 成本：导出 Flush()（G1）与心跳 idle 判定
// maybeFlushIdleLocked（G2）。口径纪律：
//   - 单 Rotator 复用 + 循环内重置（M2：避免 New-per-op 的异步 ticker
//     3 allocs/248B 落窗非确定性）；New 的 ticker 分配落在 ResetTimer 前。
//   - 注入时钟（benchFixedNow / testClock）使 flusher 在测量窗外双时钟恒
//     新鲜 → 恒 NotIdle/no-op，零异步落窗干扰；不做真实定时器 sleep 基准
//     （可见性延迟分布已由功能测试钉住，基准只测成本不测延迟）。
//   - harness 直调 mu 保护字段（buf/双时钟/注入时钟变量）全程持 r.mu；
//     重置拆卸落 StopTimer 窗；Close 不持 mu 调用。
//   - 多写者 RunParallel 基准不做（#C-2 裁定：CLI 单生产者形态）。
// ============================================================================

// BenchmarkFlush 度量导出 Flush() 的 per-call 成本（G1）：mu + flushLocked
// （buf.Flush + lastFlushAt 刷新）+ 成功路径错误包装分支（不分配）。
//   - Empty：缓冲恒空 → no-op flush 路径下限（mu + 空刷 + 时钟刷新）；
//     零突变、无拆卸、磁盘零增长。
//   - Partial：每 op = Write(100B) + Flush() → 真实 100B 部分写 syscall；
//     与 Empty 差值 ≈ Write 守卫 + 缓冲拷贝 + 部分写 syscall 的边际成本。
//     周期 truncate（StopTimer 内、持 mu）控制磁盘 ≤~410KB。
//
// 注入固定时钟使 flusher 零干扰（见段首纪律）；生产 Now=nil 形态每次
// Flush 多一次 time.Now 读（~1-3ns，r4 §4.1 profile，低于 ns 噪声地板）。
// darwin 平台标注（规则 7）：Partial ns/op 由 APFS write syscall 主导，
// 禁止外推 Linux；allocs/B 为确定性硬指标（预期两变体均 0/0）。
func BenchmarkFlush(b *testing.B) {
	line := make([]byte, 100)
	copy(line, benchLogData(1))
	cases := []struct {
		name       string
		partial    bool
		truncEvery int
	}{
		{"Empty", false, 0},
		{"Partial", true, 4096},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			dir := b.TempDir()
			r := benchNew(b, Config{
				Pattern: filepath.Join(dir, "app.log"),
				MaxSize: int64(1) << 40,
				Now:     benchFixedNow,
			})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if tc.partial {
					if i > 0 && i%tc.truncEvery == 0 {
						b.StopTimer()
						benchTruncateActive(b, r)
						b.StartTimer()
					}
					if _, err := r.Write(line); err != nil {
						b.Fatal(err)
					}
				}
				if err := r.Flush(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := r.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkIdleFlush 白盒直调 maybeFlushIdleLocked（持 r.mu）度量心跳 idle
// 判定路径成本（G2），注入可推进时钟（testClock）控制三态、零 sleep：
//   - NoOp：缓冲空 + 双时钟陈旧 → Buffered()==0 守卫短路（不触时钟比较），
//     生产心跳稳态 no-op 形态；零突变、无拆卸。
//   - NotIdle：缓冲 100B + 时钟冻结（双时钟恒新鲜）→ 纯判定成本
//     （Buffered + 两次 Sub + 比较）；零突变、无拆卸。
//   - IdleFlush：每 op = 老化 + 判定 + 部分 flush + 双时钟重置。老化动作
//     （一次指针写 + Add，~ns 级）刻意落在测量窗内同一持锁段——换取测量
//     窗外双时钟恒新鲜、flusher 恒 NotIdle，消除"重置与测量 op 之间
//     flusher 抢先 flush"的异步落窗污染；相对 µs 级部分写 syscall 可忽略。
//     重置（Write 回填 + 周期 truncate）落 StopTimer 窗（持 mu），磁盘
//     ≤~410KB。executeAt 老化恰达 FlushIdleTimeout 边界（≥ 语义），判定
//     在第一个 Sub 比较即短路进入 flush（静默分支）。
//
// 三态均单 Rotator 复用（M2）；硬指标预期全 0 allocs/0 B（判定/flush/时钟
// 操作不分配，setup 与重置均在计时窗外）。ns/op 只作参考：NoOp/NotIdle 为
// 纯 CPU 锁内判定，IdleFlush 由 APFS 部分写 syscall 主导（禁止外推 Linux）。
func BenchmarkIdleFlush(b *testing.B) {
	line := make([]byte, 100)
	copy(line, benchLogData(1))
	// newClockRotator 构造注入可推进时钟的单 Rotator（MaxSize 足够大 →
	// 永不触发大小切分；字面量 pattern 满足 M1）。
	newClockRotator := func(tb testing.TB) (*Rotator, *time.Time) {
		dir := tb.TempDir()
		clockPtr, clock := testClock(benchFixedNow())
		r := benchNew(tb, Config{
			Pattern: filepath.Join(dir, "app.log"),
			MaxSize: int64(1) << 40,
			Now:     clock,
		})
		return r, clockPtr
	}
	b.Run("NoOp", func(b *testing.B) {
		r, clockPtr := newClockRotator(b)
		// 空缓冲 + 陈旧双时钟：守卫先于时钟比较短路（no-op 语义与
		// TestIdleFlushSkipsEmptyBuffer 同款状态）。时钟变量经 r.now()
		// 被 flusher 锁内读，推进必持 mu。
		r.mu.Lock()
		*clockPtr = clockPtr.Add(10 * FlushIdleTimeout)
		r.mu.Unlock()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			r.mu.Lock()
			err := r.maybeFlushIdleLocked(r.now())
			r.mu.Unlock()
			if err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	})
	b.Run("NotIdle", func(b *testing.B) {
		r, _ := newClockRotator(b)
		if _, err := r.Write(line); err != nil {
			b.Fatal(err)
		}
		// 时钟冻结 → now-executeAt == now-lastFlushAt == 0 < 阈值，判定
		// 恒 NotIdle：缓冲滞留 100B、文件零增长，无需逐轮重置。
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			r.mu.Lock()
			err := r.maybeFlushIdleLocked(r.now())
			r.mu.Unlock()
			if err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	})
	b.Run("IdleFlush", func(b *testing.B) {
		r, clockPtr := newClockRotator(b)
		if _, err := r.Write(line); err != nil {
			b.Fatal(err)
		}
		const truncEvery = 4096
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// 老化在测量窗内持锁执行（见 doc 注释）：上一轮重置的 Write
			// 使 executeAt == 当前时钟，推进一个阈值后恰好 ≥ 边界触发
			// 静默分支 flush；成功后双时钟重置为 now（窗外恒新鲜）。
			r.mu.Lock()
			*clockPtr = clockPtr.Add(FlushIdleTimeout)
			err := r.maybeFlushIdleLocked(r.now())
			r.mu.Unlock()
			if err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			if (i+1)%truncEvery == 0 {
				benchTruncateActive(b, r)
			}
			if _, err := r.Write(line); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		b.StopTimer()
		if err := r.Close(); err != nil {
			b.Fatal(err)
		}
	})
}

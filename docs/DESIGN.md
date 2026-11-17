# rotatelogs 设计规格文档

> 版本: v1.0 | 日期: 2026-11-13 | 状态: 待评审

## 1. 概述

### 1.1 产品定位

rotatelogs 是一个 Go CLI 工具，从 stdin 管道接收进程 console 输出，原封不动写入带轮转的日志文件。适用于 7×24 小时长期运行场景。

### 1.2 设计立场

rotatelogs 优化目标是 **shell 管道中的静默可靠性**。它是透明的数据导管——禁止修改、过度缓冲或丢失数据。它不是日志管理平台、不是日志解析器、不是日志投递器。

### 1.3 使用方式

```bash
myapp 2>&1 | rotatelogs [flags] <filename-pattern>
```

### 1.4 设计原则

| 原则   | 含义                              |
| ------ | --------------------------------- |
| 极简   | 单 goroutine 写入，无复杂并发模型 |
| 可靠   | 固定内存使用，无泄漏风险          |
| 透明   | 不修改任何输入字节                |
| 跨平台 | Linux / macOS / Windows           |

### 1.5 CLI 治理分类

| 维度               | 分类                                |
| ------------------ | ----------------------------------- |
| Primary Role       | Runtime（长运行进程管理连续数据流） |
| Primary User       | Developer/Ops (human)               |
| Interaction Form   | Pipe-through (fire-and-forget)      |
| Statefulness       | Stateless per-invocation            |
| Risk Profile       | Low-Medium                          |
| Secondary Surfaces | stderr verbose, exit codes          |

## 2. 架构设计

### 2.1 数据流

```
stdin (os.Stdin)
  │
  ▼
bufio.Reader (4KB buffer) ── 从 stdin 读取数据块
  │
  ▼
Rotator.Write() ── 核心切分逻辑（检查时间/大小/信号 flag）
  │
  ▼
bufio.Writer (64KB buffer) ── 批量写入减少 syscall
  │
  ▼
os.File ── 当前活跃日志文件
```

### 2.2 为什么不用异步写入（law 库）

| 考量       | 结论                                            |
| ---------- | ----------------------------------------------- |
| 写入目标   | 本地磁盘，延迟通常 <1ms                         |
| 背压机制   | stdin pipe 自带（pipe buffer 满时上游进程阻塞） |
| 缓冲优化   | bufio.Writer 64KB 已提供足够批量写入            |
| 复杂度代价 | 引入 law 需协调多层 flush，增加关停复杂度       |
| 数据安全   | 异步模型下进程崩溃可能丢失队列中数据            |
| 结论       | bufio 足够，不引入 law                          |

### 2.3 为什么不引入 shengyanli1982 其他工具库

| 库        | 结论   | 理由                                                      |
| --------- | ------ | --------------------------------------------------------- |
| gs        | 不引入 | 不支持 SIGHUP；stdlib `signal.NotifyContext` 一行解决     |
| retry     | 不引入 | 设计为 fail-skip，文件错误多为永久性，重试无意义          |
| workqueue | 不引入 | 压缩模型是"if busy then skip"（~10行），9种队列是过度设计 |
| toolkit   | 不引入 | strconv 包装零增值，config 包会引入 viper 全家桶          |

### 2.4 模块结构

```
rotatelogs/
├── main.go                      # func main() { cmd.Execute() }
├── go.mod
├── go.sum
├── cmd/
│   ├── root.go                  # cobra Command + flags + 主循环
│   ├── signal_unix.go           # //go:build !windows — SIGHUP/SIGTERM/SIGINT
│   └── signal_windows.go        # //go:build windows — 仅 os.Interrupt
├── internal/
│   └── rotator/
│       ├── config.go            # Config struct 定义
│       ├── rotator.go           # Rotator struct + New() + Write() + Close()
│       ├── strftime.go          # strftime → Go time layout 转换
│       ├── cleanup.go           # (r *Rotator) cleanup() 方法
│       ├── compress.go          # (r *Rotator) compressAsync() 方法
│       ├── symlink_unix.go      # //go:build !windows
│       ├── symlink_windows.go   # //go:build windows
│       ├── rotator_test.go
│       ├── strftime_test.go
│       ├── cleanup_test.go
│       └── compress_test.go
├── DESIGN.md
└── README.md
```

### 2.5 模块职责

| 模块                   | 职责                                           | 关键接口                                                                      |
| ---------------------- | ---------------------------------------------- | ----------------------------------------------------------------------------- |
| cmd/root.go            | CLI 入口、flags 解析、组装 Rotator、启动主循环 | `Execute()`                                                                   |
| cmd/signal\_\*.go      | 跨平台信号监听                                 | `setupSignals(rotateFlag *atomic.Bool) (context.Context, context.CancelFunc)` |
| rotator/config.go      | 配置定义                                       | `Config` struct                                                               |
| rotator/rotator.go     | 核心 io.Writer，管理文件、执行切分             | `New(Config)`, `Write([]byte)`, `Close()`                                     |
| rotator/strftime.go    | strftime 格式转 Go time layout                 | `parsePattern(strftime string) string`                                        |
| rotator/cleanup.go     | 按 MaxAge/MaxBackups 删除旧文件                | `cleanup()` 方法                                                              |
| rotator/compress.go    | 异步 gzip 压缩已切分文件                       | `compressAsync(path)` 方法                                                    |
| rotator/symlink\_\*.go | 跨平台 symlink 操作                            | `updateSymlink(target, link)`                                                 |

## 3. CLI 接口规格

### 3.1 命令格式

```
rotatelogs [flags] <filename-pattern>
```

单一命令，无子命令树。filename-pattern 为必选位置参数。

### 3.2 Flags

| Flag              | 短写 | 类型     | 默认值 | 说明                               |
| ----------------- | ---- | -------- | ------ | ---------------------------------- |
| `--max-size`      | `-s` | uint     | 100    | 单个日志文件最大大小(MB)。0=不限制 |
| `--rotation-time` | `-t` | duration | 24h    | 时间切分周期。0=禁用时间切分；未显式指定且 pattern 无时间占位符且 `--max-size` > 0 时降级为仅按大小切分（输出一行警告，见 §4.8） |
| `--max-backups`   | `-b` | int      | 0      | 保留最大备份文件数。0=不删除       |
| `--max-age`       | `-a` | int      | 0      | 保留最大天数。0=不删除             |
| `--compress`      | `-z` | bool     | false  | 切分后异步 gzip 压缩旧文件         |
| `--symlink`       | `-l` | string   | ""     | 维护指向当前文件的符号链接路径     |
| `--verbose`       | `-v` | bool     | false  | 输出切分事件到 stderr              |
| `--local-time`    |      | bool     | false  | 使用本地时间（默认 UTC）           |

### 3.3 filename-pattern 格式

使用 **strftime 格式**（业界标准，Apache rotatelogs / cronolog / logrotate 通用）：

| 占位符 | 含义        | 示例输出 |
| ------ | ----------- | -------- |
| `%Y`   | 四位年      | 2026     |
| `%m`   | 两位月      | 11       |
| `%d`   | 两位日      | 13       |
| `%H`   | 两位时(24h) | 14       |
| `%M`   | 两位分      | 30       |
| `%S`   | 两位秒      | 00       |

内部实现：解析 strftime 占位符，转换为 Go `time.Format` 布局字符串（简单映射表，约 20 行代码）。

映射关系：

| strftime | Go time.Format |
| -------- | -------------- |
| `%Y`     | `2006`         |
| `%m`     | `01`           |
| `%d`     | `02`           |
| `%H`     | `15`           |
| `%M`     | `04`           |
| `%S`     | `05`           |

示例：

- `/var/log/app/app-%Y-%m-%d.log` → 按天：`app-2026-11-13.log`
- `/var/log/app/app-%Y-%m-%dT%H.log` → 按小时：`app-2026-11-13T14.log`
- `/var/log/app/%Y/%m/app-%d.log` → 按目录归档：`2026/11/app-13.log`
- `logs/app-%Y%m%d-%H%M%S.log` → 精确到秒：`app-20261113-143000.log`

### 3.4 使用示例

```bash
# 基本用法：按天切分，100MB 大小限制
myapp 2>&1 | rotatelogs '/var/log/myapp/app-%Y-%m-%d.log'

# 完整配置
myapp 2>&1 | rotatelogs \
  --max-size 200 \
  --rotation-time 12h \
  --max-backups 30 \
  --max-age 7 \
  --compress \
  --symlink /var/log/myapp/current.log \
  --verbose \
  --local-time \
  '/var/log/myapp/app-%Y-%m-%d.log'

# 仅按大小切分（禁用时间切分）
myapp 2>&1 | rotatelogs -t 0 -s 50 '/var/log/myapp/app.log'

# 仅按时间切分（禁用大小切分）
myapp 2>&1 | rotatelogs -s 0 -t 1h '/var/log/myapp/app-%Y-%m-%dT%H.log'

# 按目录归档（类似 cronolog 风格）
myapp 2>&1 | rotatelogs '/var/log/myapp/%Y/%m/%d/access.log'
```

### 3.5 退出码

| 退出码 | 含义                                 |
| ------ | ------------------------------------ |
| 0      | 正常退出（stdin EOF 或收到终止信号） |
| 1      | 启动失败（参数错误、无法创建文件等） |
| 2      | 运行时 IO 错误（磁盘满、权限问题等） |

## 4. 核心行为规格

### 4.1 写入行为

- 必须原封不动写入，禁止修改任何字节
- 必须保证写入顺序与接收顺序一致
- bufio.Writer 缓冲区大小固定 64KB
- 每次 Write 调用前必须检查切分条件

> Addendum（2026-11-16 心跳 idle flush，批准方案 A′）：64KB 写缓冲在原"容量驱动（bufio 自满自动 flush）+ 切分/Close flush"之外新增**时间驱动 flush**，修复"日志写入后 tail -f 看不到、直到 Close 才落盘"痛点（默认 24h 切分 + 小日志量下数据滞留用户态缓冲数小时，SIGKILL 丢整个缓冲）：
>
> - **心跳策略（借自 law，github.com/shengyanli1982/law）**：内部 flusher goroutine 每 `FlushHeartbeatInterval = 300ms` 醒来一次，在锁下做双时钟 idle 判定——`Buffered() > 0 && (now - executeAt >= FlushIdleTimeout || now - lastFlushAt >= FlushIdleTimeout)` → flush。`executeAt` = 最后写入活动（管静默期），`lastFlushAt` = 最后成功 flush（管饥饿上界：连续写入不得无限推迟 flush）；flush 成功后双时钟重置为 now，失败不更新时钟（下次心跳原时钟重试）。`FlushIdleTimeout = 2s`，即**可见性延迟上界 ≤ 2s**（名义阈值；严格上界 = FlushIdleTimeout + 一个心跳周期 = 2s + 300ms，idle 期满恰好错过一次 tick 时；A′ 为纯心跳、非行级即时 flush，用户选定语义）。
> - **单生产者适配判断（借策略不借 MPSC）**：law 用异步 MPSC + BufferPool 是因为它有海量并发生产者 goroutine；rotatelogs 只有一个生产者（cmd 层 pump 读 stdin），内核管道缓冲（Linux 64KB）已是阶段 1 解耦层，再套 MPSC 冗余且异步队列在 stdin 洪峰下无界增长 → OOM。故只借 law 的心跳 idle flush 策略，用 `sync.Mutex` 保护 bufio（单生产者 + 低频 flusher，锁竞争可忽略），不引入 MPSC/池/事件循环。double cache 天然存在：内核管道（阶段 1）+ 64KB bufio（阶段 2）。
> - **flush 驱动分工**：容量驱动由 bufio 自满承担（64KB，不单独实现容量预刷——law 的 2KB 小缓冲才需显式预刷）；时间驱动由心跳承担。洪峰下 executeAt 恒新，批处理由 bufio 自满承担，吞吐不退化（饥饿上界分支至多每 2s 额外 flush 一次，成本可忽略）。
> - **并发契约变更（取代原"单 goroutine 写入模型，Write/Close 禁止并发"）**：Write/Close/Flush 由 mu 保护，可并发调用。并发参与者：心跳 flusher goroutine（mu 下触碰 buf）、异步压缩 goroutine（不碰写入路径数据结构）、SIGHUP 信号 goroutine（atomic RotateFlag）。Write 全程持锁（rotate 在持锁下调用，内部用 flushLocked 不重复加锁），保持 0-alloc；C3 rotate 的 bufio Reset 缓冲复用、D1 边界分片、WriteBufferSize 64KB 契约值均保留。
> - **Close 生命周期（防死锁 + 幂等，严格顺序）**：a. 锁内置 closed（并发 Write 立即 errClosed）→ b. stopOnce 关闭 stopCh + flusherWg.Wait（**不持 mu**——flusher 可能正在等 mu，持 mu 等待会死锁）→ c. 锁内终刷 + 关文件 → d. symlink best-effort + compressWg.Wait + 单行错误合并（不变）。flusher 仅在 New 成功路径末尾启动（错误路径不启动，防泄漏）；心跳 flush 失败仅 verbose 警告并继续循环，bufio sticky error 由下一次 Write surfaced 传播为 exit(2)。
> - **与 §12.3 的关系**：本变更不引入 law 或异步写入层——写入仍是同步单生产者（pump → Write），flusher 仅在锁下执行 flush、不排队写入请求；"禁止多 goroutine 并发写入同一文件"指禁止无同步的并发写入，mu 保护下的 flush 不违反。原"64KB 缓冲无时间维 flush"已知限制由本 addendum 取代（硬杀丢失窗口从整个缓冲缩小到 ≤ 2s，名义阈值，严格上界见上）。

### 4.2 切分触发条件

切分在以下任一条件满足时触发（OR 逻辑）：

1. **时间到期**：当前时间 >= nextRotate
2. **大小超限**：当前文件累计写入字节 >= Config.MaxSize（单位：bytes，由 cmd 层从 MB 转换）
3. **信号触发**：SIGHUP 设置的 atomic flag 为 true

切分流程（严格顺序）：

```
1. Flush bufio.Writer 到文件
2. 关闭当前文件句柄
3. 基于当前时间 + pattern 生成新文件名
4. 如果文件名冲突 → 追加序号(.1, .2, ...)
5. 自动创建父目录（MkdirAll）
6. 创建新文件，初始化新 bufio.Writer
7. 更新 symlink（如果配置了）
8. 触发异步压缩（如果配置了）
9. 触发清理（如果配置了 MaxAge 或 MaxBackups）
10. verbose 模式输出切分事件到 stderr
```

> 注（2026-11-13 L5 校准回写）：步骤 4/5 的实现序为 5→4——步骤 4 的 O_EXCL 冲突探测要求父目录已存在，否则跨期新目录（如 `/%Y/%m/`）场景切分直接失败；此为 D3 决策的技术必然，步骤逻辑语义不变。

> 注（2026-11-13 审计 DA-1 修订）：`RotationTime > 24h` 时时间边界改为**周期递推锚定**——首边界仍按自然对齐公式计算（启动日午夜 + 周期），此后每一边界自上一计划边界严格 + 周期递推（size/SIGHUP 提前切分不顺延计划边界），不再每日重新锚定当日午夜；旧实现在周期 > 24h 且非 24h 整数倍时稳态周期静默退化为 24h。`RotationTime ≤ 24h` 的自然边界对齐语义保持不变。

### 4.3 清理策略

- 时机：仅在切分发生后执行（机会式，即"在切分事件发生时顺带执行"，无独立定时器），零常驻 goroutine
- MaxBackups：按文件修改时间（ModTime）排序，保留最新 N 个，删除更旧文件（2026-11-13 L5 校准修订：原"按文件名排序"在固定名+序号模式下字典序与时间序不一致——`.10` 排在 `.2` 前——会误删较新归档，属数据丢失缺陷）
- MaxAge：删除修改时间超过 N 天的文件
- 两者可同时生效，取更严格的条件
- 时钟 step 回拨钳制（2026-11-13 审计 DA-2 修订）：mtime 晚于清理入口当前时间快照的归档（NTP step/VM 挂起恢复等时钟快进期产物）钳制为 now 参与 ModTime 排序与 MaxAge cutoff 判定，钳制后 mtime 相同的文件间被钳制者优先删除（真实当期新文件幸存）、再同按文件名升序——否则未来 mtime 归档恒冒充"最新"且恒不过期，回拨后清理将删新留旧（保留语义反转）；被钳制者不因 now > cutoff 提前过期，待 MaxBackups 淘汰或真实时间追上后处理
- 清理失败禁止影响主写入流程（仅 verbose 时输出警告到 stderr）
- 匹配文件集：将 pattern 中的 strftime 占位符替换为 `*` 后匹配（如 `app-%Y-%m-%d.log` → `app-*.log`）。机制双路径：pattern 目录部分不含占位符（平面目录，生产主流）→ 单次目录扫描 + 内存 `filepath.Match` + `fstatat` 一次取 FileInfo 贯通排序/过期判定（2026-11-13 性能优化 C1）；目录部分含占位符（cronolog 形态，归档跨日期目录分布）→ 多目录单遍走树（2026-11-16 性能优化 C-V2；仅 Unix，Windows 例外见本条末句）：自 pattern 目录首个占位符段之前的字面量前缀起做显式栈 DFS（无递归深度上限——`filepath.Glob` 的 10000 层上限是递归实现的栈防护细节而非行为契约，pattern 目录层级深度即实际上界），每目录恰好一次 open+`Readdir(-1)`+close，目录段按 token 替换后的段模式逐层 `filepath.Match`（字面量段与 `\` 转义语义与 `filepath.Glob` 逐层拆分一致），叶层对三变体（基础/`.gz`/`.[0-9]*`）做基名匹配一次收割全部变体。两条生产路径与冻结的 glob 参照实现（`matchArchivesGlob`）在 Unix 上语义逐项等价（等价性测试以全形态日期树钉住：变体、symlink 三态、悬空、活跃、空目录、跨目录同名、字面量与 token 目录段混合；另含 root=`"."`（相对路径 pattern、无字面量前缀）与 root=`"/"`（绝对根、首个目录段即含 token）两个退化形态子测试）。Windows 上 cronolog 形态不走树而回退该 glob 参照实现本身（2026-11-16 审查 P1 方案 B：走树形状解析硬编码 Unix `/` 分隔符，Windows 的 `\` 路径会零切分导致清理静默失效；`filepath.Glob` 原生支持 Windows 分隔符，为 C-V2 变更前的原生正确行为，且 Windows 走树性能从未被 profile）——`matchArchivesGlob` 因此兼任 Windows 生产路径与 Unix 等价性测试 oracle 双重角色（函数体保持冻结），平台感知走树（双分隔符分段 + 按 GOOS 取段元字符集）登记为 v2 候选不实施
- cronolog 形态的已知限制（P2-A，形状匹配固有残余面）：目录段为通配（`%Y` → `*`）时，同层非日期目录内的同名归档形状文件（如 `not-a-date/access.log` 命中 `*/*/*/access.log`）同样进入清理候选集——这是 D4 形状匹配的固有语义（glob 参照实现同样命中，非单遍走树引入），依赖用户不将非日期目录混入 pattern 覆盖范围；字面量目录段（如 `static/`）可收窄该面
- cronolog 形态的空日期目录在清理删除其中归档文件后 best-effort 逐级向上回收（2026-11-15 审计 N3-B）：仅删空目录（活跃文件/第三方文件/存活归档所在目录非空即停），不越过 pattern 的静态字面量目录前缀（前缀本身及其祖先绝不删除），失败静默不传播（§4.8）
- 压缩在途时，清理仅跳过在途压缩的源文件及其**精确** `.gz` 目标名（压缩启动时同步定名后发布的 `{src, gz}` 两元素对），不豁免整个 `.N.gz` 序号家族——否则家族豁免会连带保护已完成的历史归档，固定名+压缩+MaxBackups 模式下归档数无界增长（2026-11-13 L5 复审 NEW-P1-1 修订）
- 在途压缩对 MaxBackups 占用一个名额（keep 预留 1）：源文件与其 `.gz` 目标是同一份归档的两种形态，等量交换不改变归档总数；无此预留则在途文件逃过计数，稳态突破"最多 N 个"上界（2026-11-13 L5 复审 NEW-P2-1R 补记）

### 4.4 压缩策略

- 时机：切分完成后异步执行
- 并发控制：同一时刻最多 1 个压缩 goroutine（使用 `sync.WaitGroup.Go`）
- 如果上一次压缩未完成，跳过本次（不排队）
- 压缩完成后删除原文件，生成 `.gz` 后缀文件
- 压缩失败禁止影响主写入流程（仅 verbose 时输出警告到 stderr）
- 禁止压缩当前正在写入的文件
- 产物名冲突时按序号探测（`app.log.gz` 被占用 → `app.log.1.gz`、`app.log.2.gz`…，`O_EXCL` 绝不覆盖既有归档）；序号探测产生的历史 `.gz` 属正常归档，参与 MaxBackups 计数与淘汰，不因序号家族获得豁免

### 4.5 Symlink 行为

- 启动时必须创建/更新 symlink 指向当前文件
- 每次切分后必须更新 symlink 指向新文件
- 同目录使用相对路径，跨目录使用绝对路径
- Windows 平台：静默跳过（verbose 时输出提示）
- symlink 更新失败禁止影响主写入流程

### 4.6 信号处理

| 信号    | 行为                                    | 平台      |
| ------- | --------------------------------------- | --------- |
| SIGHUP  | 设置 atomic flag，下次 Write 时触发切分 | Unix only |
| SIGTERM | 优雅关停：flush → close → exit(0)       | Unix only |
| SIGINT  | 优雅关停：flush → close → exit(0)       | 全平台    |

实现方式：

- 信号监听在独立 goroutine（`signal.Notify`）
- SIGHUP 通过 `atomic.Bool` 传递到主写入循环
- SIGTERM/SIGINT 通过 `context.Cancel` 通知主循环退出

### 4.7 stdin EOF 行为

- stdin 读取返回 EOF 时，必须立即执行优雅关停
- 关停流程：flush bufio.Writer → 关闭文件 → 更新 symlink → exit(0)
- 禁止等待、禁止重试

### 4.8 错误处理

| 错误场景                                                                                                                       | 行为                       |
| ------------------------------------------------------------------------------------------------------------------------------ | -------------------------- |
| 无法创建日志文件                                                                                                               | 输出错误到 stderr，exit(1) |
| 写入时磁盘满                                                                                                                   | 输出错误到 stderr，exit(2) |
| 清理/压缩/symlink 失败                                                                                                         | verbose 输出警告，继续运行 |
| pattern 无效（不含时间占位符且**显式**传入 `--rotation-time` > 0；或保持隐式默认但 `--max-size` = 0 不降级，M2） | 输出错误到 stderr，exit(1) |
| pattern 无时间占位符且 `--rotation-time` 保持隐式默认（未显式传入）且 `--max-size` > 0（2026-11-16 行为变更，方案 B） | cmd 层意图预处理：RotationTime 归零，降级为仅按大小切分，stderr 输出恰一行降级警告（不受 `--verbose` 门控），正常运行退出 exit(0)；M2 规则本身不变 |
| pattern 字面量含 glob 元字符（`*` `?` `[`，Unix 另禁 `\`，Windows 禁集仍 `*` `?` `[`；2026-11-13 审计 DA-3 修订：清理按 glob 匹配，元字符致清理静默失效或误删第三方文件；2026-11-17 审计 R5-P1 修订：`\` 在 Unix filepath.Match 中是转义元字符而生成端字面量保留，生成↔清理分叉致自家归档永不命中清理、脱转义形状误删第三方文件（如 `app\.log`→`app.log`）；收紧副作用：反斜杠仅在目录段且目录无 token 的形态在 Unix 同样启动拒绝，与 M5 无条件哲学一致） | 输出错误到 stderr，exit(1) |

## 5. 配置模型

### 5.1 Config Struct

```go
package rotator

import "time"

// Config 定义 Rotator 的全部配置项。
// 由 cmd 层从 cobra flags 解析后填充。
type Config struct {
    // Pattern 是文件名模板，使用 strftime 格式。
    // 必填。示例: "/var/log/app/app-%Y-%m-%d.log"
    Pattern string

    // MaxSize 是单个文件最大字节数。0 表示不限制。
    // 由 --max-size (MB) × 1024 × 1024 计算得出。
    MaxSize int64

    // RotationTime 是时间切分周期。0 表示禁用时间切分。
    RotationTime time.Duration

    // MaxBackups 是保留的最大备份文件数。0 表示不删除。
    MaxBackups int

    // MaxAge 是保留的最大天数。0 表示不删除。
    MaxAge int

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
```

### 5.2 配置校验（2026-11-13 追加）

Config 由 cmd 层从 cobra flags 解析后填充，`rotator.New(cfg)` 第一步执行 `cfg.Validate()`（validator/v10）：

- 字段级：Pattern required；MaxSize/RotationTime/MaxBackups/MaxAge 非负
- 跨字段（struct-level）：M1 MaxSize 与 RotationTime 至少一个 > 0；M2 RotationTime > 0 时 Pattern 必须含时间占位符；M5 Pattern 字面量（剔除时间占位符后）不得含 glob 元字符 `*` `?` `[`，Unix 另禁 `\`（Windows 禁集仍 `*` `?` `[`；2026-11-13 审计 DA-3：清理按 glob 匹配，元字符致清理静默失效或误删第三方文件；2026-11-17 审计 R5-P1：`\` 在 Unix filepath.Match 中是转义元字符而生成端 formatName 字面量保留，生成↔清理分叉致自家归档永不命中清理、脱转义形状误删第三方文件（如 `app\.log`→`app.log`）；收紧副作用：反斜杠仅在目录段且目录无 token 的形态在 Unix 同样启动拒绝，与 M5 无条件拒绝哲学一致）；M6 MaxBackups/MaxAge 任一 > 0 时 Pattern 基名首字符须为字面量前缀锚（首个时间占位符前存在非空字面量；2026-11-15 审计 N1 引入，2026-11-16 审计 P1-A 收紧为前缀判定：基名以占位符开头时清理 glob 以 `*` 开头、左侧零收窄，仅后缀锚（%Y.log、%Y%m%d.log）形同 `*.log` 仍误删同目录第三方文件；cronolog 形态 %Y/%m/%d/access.log 的基名全字面量不受影响；cleanup 关闭时不限制）；M7 MaxAge ≤ 365000 天（2026-11-15 审计 N2：AddDate 天→秒换算 int64 回绕致 cutoff 落入未来、全部归档静默删除）
- New() 内：M3a Symlink 路径不得与初始日志文件路径相同，且 Symlink+".tmp" 临时槽位不得与初始日志文件路径相同（2026-11-16 审计 P1-B 扩展：updateSymlink 的 os.Remove(tmp) 会解链命中该槽位的活跃文件，写入进入孤儿 inode；依赖 formatName(now) 时间计算，无法在 Validate() 静态判定）；M3b 对 link 正式槽位与 tmp 临时槽位同施非 symlink 占位拒绝（运行时兜底，覆盖日期推进后才可观察的碰撞形态，如含日期字面量的 Symlink 名）
- 校验失败统一返回 `ErrInvalidConfig`（含字段明细），cmd 层映射 exit(1)
- 详细规则见 `.agent-work-b652f803/plans/rotatelogs-v1/addendum-config-validation-errors.md`

### 5.3 配置传递方式

选择 Config struct 直接传入（非 Functional Options）：

- CLI 工具的参数由 cobra flags 解析，已是 named params
- 再包一层 Option 模式是多余抽象
- Config struct 简单直接，零认知负担

## 6. 核心类型设计

### 6.1 Rotator

```go
package rotator

// Rotator 实现 io.WriteCloser，管理日志文件的写入和轮转。
// 并发安全性：Write 和 Close 禁止并发调用（单 goroutine 模型）。
// Rotate 可以从信号处理 goroutine 通过 atomic flag 间接触发。
type Rotator struct {
    cfg        Config
    layout     string         // Pattern 转换后的 Go time layout
    file       *os.File       // 当前活跃文件
    buf        *bufio.Writer  // 写缓冲
    size       int64          // 当前文件已写入字节数
    nextRotate time.Time      // 下一个时间切分点
    RotateFlag atomic.Bool    // SIGHUP 设置的强制切分标志（导出，供 cmd 层信号处理写入）
    compressWg sync.WaitGroup // 压缩 goroutine 跟踪
    closed     bool
}

// New 创建 Rotator 并打开初始日志文件。
// 如果初始文件无法创建，返回 error。
func New(cfg Config) (*Rotator, error)

// Write 实现 io.Writer。写入前检查切分条件（时间/大小/RotateFlag）。
func (r *Rotator) Write(p []byte) (int, error)

// Close 实现 io.Closer。flush 缓冲区、关闭文件、等待压缩完成。
func (r *Rotator) Close() error

// rotate 执行切分：关闭旧文件、打开新文件、触发清理和压缩。
// 私有方法，由 Write 内部在条件满足时调用。
func (r *Rotator) rotate() error
```

> 注（2026-11-16）：并发契约已变更为 mu 保护（Write/Close/Flush 可并发调用），结构体新增 mu、双时钟（executeAt/lastFlushAt）与 flusher 生命周期（stopCh/flusherWg/stopOnce）字段及导出 Flush() 方法；上方代码块为 v1 初始设计快照，现行契约与 flush 策略见 §4.1 addendum。

### 6.2 strftime 转换

```go
package rotator

// strftimeReplacements 定义 strftime → Go time.Format 的映射。
var strftimeReplacements = map[string]string{
    "%Y": "2006",
    "%m": "01",
    "%d": "02",
    "%H": "15",
    "%M": "04",
    "%S": "05",
}

// parsePattern 将 strftime 格式的 pattern 转换为 Go time.Format 布局字符串。
func parsePattern(pattern string) string
```

### 6.3 主循环（cmd/root.go）

```go
// 伪代码描述主循环逻辑
func runRotator(cfg rotator.Config) error {
    // 1. 创建 Rotator
    r, err := rotator.New(cfg)
    if err != nil {
        return err
    }
    defer r.Close()

    // 2. 设置信号监听（cmd 包内实现，非 rotator 包）
    //    SIGHUP → r.RotateFlag.Store(true)
    //    SIGTERM/SIGINT → cancel context
    ctx, stop := setupSignals(&r.RotateFlag)
    defer stop()

    // 3. 主读取循环
    reader := bufio.NewReader(os.Stdin)
    buf := make([]byte, 4096)
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
        }

        n, err := reader.Read(buf)
        if n > 0 {
            if _, werr := r.Write(buf[:n]); werr != nil {
                return werr
            }
        }
        if err == io.EOF {
            return nil
        }
        if err != nil {
            return err
        }
    }
}
```

## 7. 跨平台策略

### 7.1 平台差异矩阵

| 功能             | Linux | macOS | Windows                     |
| ---------------- | ----- | ----- | --------------------------- |
| stdin pipe       | ✅    | ✅    | ✅                          |
| SIGHUP 强制切分  | ✅    | ✅    | ❌ (静默忽略)               |
| SIGTERM 优雅退出 | ✅    | ✅    | ❌ (仅 SIGINT)              |
| symlink          | ✅    | ✅    | ⚠️ (需权限，失败时静默跳过) |
| gzip 压缩        | ✅    | ✅    | ✅                          |
| 文件清理         | ✅    | ✅    | ✅                          |

### 7.2 Build Tags 策略

信号处理归属 cmd 包（进程生命周期管理），symlink 归属 rotator 包（文件操作）：

```go
// cmd/signal_unix.go
//go:build !windows

package cmd

// setupSignals 监听 SIGHUP(设置 rotateFlag)、SIGTERM/SIGINT(cancel context)。
func setupSignals(rotateFlag *atomic.Bool) (context.Context, context.CancelFunc)
```

```go
// cmd/signal_windows.go
//go:build windows

package cmd

// setupSignals 仅监听 os.Interrupt (Ctrl+C)，返回 cancel context。
func setupSignals(rotateFlag *atomic.Bool) (context.Context, context.CancelFunc)
```

```go
// internal/rotator/symlink_unix.go
//go:build !windows

package rotator

// updateSymlink 创建/更新符号链接（同目录用相对路径，跨目录用绝对路径）。
func updateSymlink(target, link string) error
```

```go
// internal/rotator/symlink_windows.go
//go:build windows

package rotator

// updateSymlink Windows 降级：尝试创建，失败时静默返回 nil。
func updateSymlink(target, link string) error
```

## 8. Go 1.25 特性采用

| 特性                | 采用 | 用途                                     |
| ------------------- | ---- | ---------------------------------------- |
| `sync.WaitGroup.Go` | ✅   | 压缩 goroutine 管理（替代手动 Add/Done） |
| Slice 栈分配优化    | 自动 | 减少 GC 压力                             |
| 容器感知 GOMAXPROCS | 自动 | K8s/Docker 部署时自动适配                |
| `os.Root`           | ❌   | 用户自控路径，无不可信输入，属防御性编程 |
| `testing/synctest`  | ❌   | Config.Now 注入更简单直接                |
| `io/fs.ReadLinkFS`  | ❌   | 直接 os.Symlink/os.Readlink 足够         |

## 9. 技术选型

### 9.1 外部依赖

| 库                                     | 版本     | 用途                                                                                                                         |
| -------------------------------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------- |
| github.com/spf13/cobra                 | v1.10.2  | CLI 命令定义、flags 解析                                                                                                     |
| github.com/stretchr/testify            | v1.11.1  | 测试断言 (assert/require)                                                                                                    |
| github.com/go-playground/validator/v10 | v10.30.4 | Config 字段与跨字段校验（2026-11-13 用户追加）                                                                               |
| github.com/spf13/pflag                 | v1.0.9   | cobra 附带的 flag 库；registerFlags 直接引用 `*pflag.FlagSet` 类型（cobra 依赖图内，零新增供应链面，2026-11-13 L5 校准补记） |

### 9.2 标准库依赖

| 包            | 用途                      |
| ------------- | ------------------------- |
| bufio         | 读写缓冲                  |
| compress/gzip | gzip 压缩                 |
| context       | 信号取消传播              |
| fmt           | 格式化输出(verbose/error) |
| io            | io.Writer 接口、io.EOF    |
| os            | 文件操作、信号            |
| os/signal     | 信号监听                  |
| path/filepath | 路径处理、Glob 匹配       |
| strings       | strftime 解析             |
| sync          | WaitGroup                 |
| sync/atomic   | atomic.Bool               |
| time          | 时间计算、格式化          |

## 10. 测试策略

### 10.1 测试原则

- 使用 `t.TempDir()` 创建真实临时文件，禁止 mock 文件系统
- 使用 testify `assert`/`require` 做断言
- 时间相关测试通过 `Config.Now` 注入控制
- 每个测试必须独立，禁止依赖执行顺序

### 10.2 测试覆盖范围

| 测试文件         | 覆盖场景                                                                             |
| ---------------- | ------------------------------------------------------------------------------------ |
| rotator_test.go  | 基本写入、时间切分、大小切分、SIGHUP 切分、文件名冲突追加序号、Close flush、EOF 处理 |
| strftime_test.go | strftime 转换正确性、无占位符 pattern、混合路径                                      |
| cleanup_test.go  | MaxBackups 清理、MaxAge 清理、两者同时生效、清理失败不影响写入                       |
| compress_test.go | 正常压缩、单并发控制、压缩失败不影响写入、.gz 文件正确性                             |

### 10.3 测试示例

```go
func TestRotateBySize(t *testing.T) {
    dir := t.TempDir()
    now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

    cfg := Config{
        Pattern:      filepath.Join(dir, "app-%Y%m%d.log"),
        MaxSize:      10, // 10 bytes
        RotationTime: 0,  // 禁用时间切分
        Now:          func() time.Time { return now },
    }

    r, err := New(cfg)
    require.NoError(t, err)
    defer r.Close()

    // 写入 15 字节，应触发切分
    _, err = r.Write([]byte("123456789012345"))
    require.NoError(t, err)

    // 验证产生了 2 个文件
    files, _ := filepath.Glob(filepath.Join(dir, "app-*"))
    assert.Len(t, files, 2)
}
```

## 11. 风险与缓解

| 风险                               | 影响                                                                                | 缓解措施                                                                                                      |
| ---------------------------------- | ----------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| 磁盘满导致写入失败                 | 数据丢失，进程退出(2)                                                               | 明确退出码，stderr 报错                                                                                       |
| 高频切分导致 inode 耗尽            | 无法创建新文件                                                                      | MaxBackups 限制文件数                                                                                         |
| Windows symlink 权限不足           | symlink 功能不可用                                                                  | 静默降级，verbose 提示                                                                                        |
| 压缩 goroutine panic               | 主流程不受影响                                                                      | recover + verbose 报告                                                                                        |
| 大文件压缩耗时长                   | 跳过本次压缩                                                                        | 单并发策略，下次切分再尝试                                                                                    |
| 压缩中途进程被硬杀（kill -9/掉电） | 损坏的 `.gz` 半成品以正常归档名残留（源数据无损，后续切分会重新压缩为序号名产物）   | 配置 MaxAge/MaxBackups 可随时间自然淘汰；v2 计划 tmp+rename 原子发布（2026-11-13 审计 DA-4）                  |
| 多实例共用同一 pattern 目录        | 各自的切分/清理/压缩可删除对方仍持有的归档，写入落入孤儿 inode 丢数据（行为未定义） | v1 契约单 pattern 目录单实例（README 已知限制披露）；v2 可评估 lock file（2026-11-13 审计 OBS-4）             |
| 全链路无 fsync                     | 掉电可丢失 OS page cache 内数据（可远超 64KB 缓冲区）                               | 与业界同类一致（lumberjack/Apache rotatelogs）；优雅关停仅承诺已 flush 数据（§14.2-2，2026-11-13 审计 OBS-6） |

## 12. v1 边界

### 12.1 v1 包含

- stdin 管道读取 + 原封不动写入
- 时间+大小双切分
- MaxAge/MaxBackups 清理
- gzip 压缩（异步单并发）
- Symlink
- SIGHUP 强制切分
- Verbose 模式
- 时区支持
- 跨平台
- strftime 格式 pattern

### 12.2 v1 不包含（延迟）

- 配置文件支持（YAML/TOML）
- 子命令（status/clean）
- 多输出目标
- 网络日志投递
- 日志解析/过滤
- Web UI / API

### 12.3 明确禁止

- 禁止修改输入数据
- 禁止多 goroutine 并发写入同一文件
- 禁止引入 law 或其他异步写入层
- 禁止使用 os.Root（属防御性编程）
- 禁止 mock 文件系统测试
- 禁止引入 shengyanli1982 下的 gs/retry/workqueue/toolkit（场景不匹配）

## 13. 竞品对标

| 特性         | rotatelogs(本项目) | Apache rotatelogs | cronolog  | lumberjack        | file-rotatelogs |
| ------------ | ------------------ | ----------------- | --------- | ----------------- | --------------- |
| 形态         | CLI                | CLI               | CLI       | Go 库             | Go 库(已归档)   |
| 时间切分     | ✅                 | ✅                | ✅        | ❌                | ✅              |
| 大小切分     | ✅                 | ✅                | ❌        | ✅                | ❌              |
| 压缩         | ✅                 | ❌                | ❌        | ✅                | ❌              |
| Symlink      | ✅                 | ✅(hard link)     | ❌        | ❌                | ✅              |
| 清理策略     | MaxAge+MaxBackups  | 循环覆盖(-n)      | ❌        | MaxAge+MaxBackups | MaxAge/Count    |
| 跨平台       | ✅                 | Unix only         | Unix only | ✅                | ✅              |
| Pattern 格式 | strftime           | strftime          | strftime  | 固定格式          | strftime        |
| 活跃维护     | ✅                 | ✅(Apache)        | ❌        | ✅                | ❌(已归档)      |

**差异化优势**：同时支持时间+大小双切分 + 压缩 + symlink + 全平台 + strftime 格式，填补 file-rotatelogs 归档后的空白。

## 14. 验收标准

### 14.1 功能验收

| #   | 验收项          | 验收方法                                        | 通过标准                                                   |
| --- | --------------- | ----------------------------------------------- | ---------------------------------------------------------- |
| 1   | 数据完整性      | `echo "hello" \| rotatelogs pattern` 后读取文件 | 文件内容与输入字节完全一致                                 |
| 2   | 时间切分        | 注入 Config.Now，模拟跨越 rotation-time 边界    | 产生 2 个独立文件，文件名含正确时间戳                      |
| 3   | 大小切分        | 写入超过 MaxSize 的数据                         | 产生 2+ 个文件，每个不超过 MaxSize                         |
| 4   | SIGHUP 切分     | 运行中发送 SIGHUP，继续写入                     | 信号后首次写入触发切分，产生新文件                         |
| 5   | 清理 MaxBackups | 切分 N+1 次后检查文件数与保留集合               | 目录中最多保留 N 个归档文件，且保留的是修改时间最新的 N 个 |
| 6   | 清理 MaxAge     | 创建修改时间为 N+1 天前的文件，触发切分         | 过期文件被删除                                             |
| 7   | 压缩            | 启用 --compress，触发切分后等待                 | 旧文件变为 .gz，内容可 gunzip 还原                         |
| 8   | Symlink         | 启用 --symlink，触发切分                        | symlink 始终指向当前活跃文件                               |
| 9   | 优雅关停        | 发送 SIGTERM                                    | 进程退出码 0，文件内容完整（无截断）                       |
| 10  | EOF 退出        | 关闭 stdin pipe                                 | 进程立即退出码 0                                           |
| 11  | 文件名冲突      | 同一时间周期内触发 2 次切分                     | 第二个文件追加 .1 后缀                                     |
| 12  | strftime 解析   | 传入 `%Y-%m-%d` 格式 pattern                    | 正确生成对应日期文件名                                     |

### 14.2 非功能验收

| #   | 验收项     | 验收方法                             | 通过标准                                                                                                                           |
| --- | ---------- | ------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| 1   | 内存稳定   | 持续写入 1GB 数据，监控 RSS          | 内存占用相对基线恒定、无增长趋势（无泄漏）；绝对值 ≤16MB（Go runtime+依赖基线 ~8MB，2026-11-13 用户裁定修订，原 <10MB 物理不可达） |
| 2   | 零数据丢失 | 写入已知内容后 kill -TERM，对比文件  | 已 flush 数据完整，无截断                                                                                                          |
| 3   | 跨平台编译 | `GOOS=linux/darwin/windows go build` | 三平台均编译通过                                                                                                                   |
| 4   | 测试通过   | `go test ./...`                      | 全部 PASS，覆盖率 >80%                                                                                                             |

### 14.3 待确认问题

| #   | 问题                                                     | 影响范围   | 建议                                                                       |
| --- | -------------------------------------------------------- | ---------- | -------------------------------------------------------------------------- |
| 1   | `--rotation-time` 是否支持 `7d` 这种非标准 Go duration？ | CLI 解析   | 建议仅支持标准 Go duration（h/m/s），文档中给出常见示例                    |
| 2   | 压缩跳过时是否需要记录未压缩文件列表，以便下次补压？     | 压缩完整性 | v1 不补压（极简原则），v2 可考虑                                           |
| 3   | pattern 不含任何 strftime 占位符时行为？                 | 错误处理   | 已决议（2026-11-16 方案 B）：**显式**传入 `--rotation-time` > 0 时报错退出（M2）；保持隐式默认（24h）且 `--max-size` > 0 时自动降级为仅按大小切分并输出一行警告（见 §4.8）；显式 `-t 0` 仅按大小切分允许（固定文件名 + 序号） |

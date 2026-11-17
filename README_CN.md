# rotatelogs

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/shengyanli1982/rotatelogs)

cli 工具：从 stdin 管道接收输出，逐字节透传到带轮转的日志文件。不解析、不过滤、不修改输入；固定内存，心跳实时 flush（≤2s 可见）；支持 Linux / macOS / Windows。

```bash
myapp 2>&1 | rotatelogs [flags] <filename-pattern>
```

## 快速开始

### 安装

```bash
# 需 Go 1.25+
go install github.com/shengyanli1982/rotatelogs@latest
```

或从源码构建：

```bash
git clone https://github.com/shengyanli1982/rotatelogs.git
cd rotatelogs
go build -o rotatelogs .
```

或用 Makefile 预编译发布（依赖 `zip` 命令）：

```bash
make release
```

`make build` 产出 4 平台二进制至 `dist/`；`make release` 额外打包 zip。版本号默认取 `git describe`，可用 `make VERSION=v1.2.3 release` 覆盖。

### 运行

最小可运行示例：

```bash
echo "hello" | rotatelogs 'logs/app-%Y-%m-%d.log'
```

stdin EOF 后优雅退出（exit 0），日志落在 `logs/app-YYYY-MM-DD.log`（父目录自动创建）。

常用组合：

```bash
# 按天切分，保留 30 个归档并 gzip 压缩
myapp 2>&1 | rotatelogs --max-backups 30 --compress '/var/log/myapp/app-%Y-%m-%d.log'

# 固定文件名按 50MB 切分，保留 7 天
myapp 2>&1 | rotatelogs -t 0 -s 50 -a 7 '/var/log/myapp/app.log'
```

## 用法

```
rotatelogs [flags] <filename-pattern>
```

单一命令，无子命令。`filename-pattern` 为必选位置参数，使用 strftime 占位符命名日志文件；父目录不存在时自动创建。

### Flags

| Flag              | 短写 | 类型     | 默认值 | 说明                               |
| ----------------- | ---- | -------- | ------ | ---------------------------------- |
| `--max-size`      | `-s` | uint     | 100    | 单个日志文件最大大小(MB)。0=不限制 |
| `--rotation-time` | `-t` | duration | 24h    | 时间切分周期。0=禁用时间切分；未显式指定且 pattern 无占位符时降级为仅按大小切分（输出一行警告） |
| `--max-backups`   | `-b` | int      | 0      | 保留最大备份文件数。0=不删除       |
| `--max-age`       | `-a` | int      | 0      | 保留最大天数。0=不删除             |
| `--compress`      | `-z` | bool     | false  | 切分后异步 gzip 压缩旧文件         |
| `--symlink`       | `-l` | string   | ""     | 维护指向当前文件的符号链接路径     |
| `--verbose`       | `-v` | bool     | false  | 输出切分事件到 stderr              |
| `--local-time`    |      | bool     | false  | 使用本地时间（默认 UTC）           |

约束：

- `--max-size` 与 `--rotation-time` 至少一项 > 0
- `--rotation-time` > 0 时 pattern 必须含时间占位符；保持隐式默认（24h）且 `--max-size` > 0 时，该矛盾自动降级为仅按大小切分并输出一行警告——显式传入 `--rotation-time` 时仍为启动错误
- `--max-age` 上界 365000 天（≈1000 年），超出启动即拒绝（防止天数→秒换算回绕）
- `--rotation-time` 支持标准 Go duration（`h`/`m`/`s`，如 `12h`、`1h30m`）

### filename-pattern（strftime 格式）

与 Apache rotatelogs / cronolog / logrotate 通用的 strftime 占位符：

| 占位符 | 含义        | 示例输出 |
| ------ | ----------- | -------- |
| `%Y`   | 四位年      | 2026     |
| `%m`   | 两位月      | 11       |
| `%d`   | 两位日      | 13       |
| `%H`   | 两位时(24h) | 14       |
| `%M`   | 两位分      | 30       |
| `%S`   | 两位秒      | 00       |

仅支持上述六个占位符，**不支持 `%%` 转义**：`%%` 原样输出，未识别的 `%X` 保持原样。从 Apache rotatelogs 迁移时注意。

示例：

- `/var/log/app/app-%Y-%m-%d.log` → 按天：`app-2026-11-13.log`
- `/var/log/app/app-%Y-%m-%dT%H.log` → 按小时：`app-2026-11-13T14.log`
- `/var/log/app/%Y/%m/app-%d.log` → 按目录归档：`2026/11/app-13.log`
- `logs/app-%Y%m%d-%H%M%S.log` → 精确到秒：`app-20261113-143000.log`

pattern 不含占位符时使用固定文件名，切分产物追加序号后缀：`app.log` → `app.log.1` → `app.log.2` … 显式仅按大小切分用 `-t 0`；`--rotation-time` 保持隐式默认（未显式指定）时同样仅按大小切分（降级并输出一行警告）。

**限制**：

- pattern 字面量部分不得含 glob 元字符 `*`、`?`、`[`（Unix 另禁 `\`），否则启动拒绝——归档清理按 glob 匹配，元字符导致清理失效或误删
- 启用清理时，pattern 基名必须以字面量开头（"前缀锚"）——以占位符开头会被拒绝。例：`app-%Y-%m-%d.log` 通过；`%Y-%m-%d.log` 被拒绝（glob 变为 `*-*-*.log`，误删风险）

**选型建议**：优先使用含时间占位符的 pattern，或配置 `--max-backups`/`--max-age`——两者都使序号冲突探测有界。固定文件名 + `-t 0` 且无清理时，序号探测成本随归档数线性增长（约 +2.3µs/归档/次切分）。

## 使用示例

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

## 退出码

| 退出码 | 含义                                 |
| ------ | ------------------------------------ |
| 0      | 正常退出（stdin EOF 或收到终止信号） |
| 1      | 启动失败（参数错误、无法创建文件等） |
| 2      | 运行时 IO 错误（磁盘满、权限问题等） |

错误信息以 `rotatelogs: ` 前缀输出到 stderr。附属操作（清理/压缩/symlink）失败仅在 `--verbose` 下输出警告。

## 信号行为

| 信号    | 行为                                 | 平台      |
| ------- | ------------------------------------ | --------- |
| SIGHUP  | 强制切分：置位标志，下一次写入时触发 | Unix only |
| SIGTERM | 优雅关停：flush → close → exit(0)    | Unix only |
| SIGINT  | 优雅关停：flush → close → exit(0)    | 全平台    |

说明：

- stdin EOF 同样触发优雅关停
- 主循环阻塞在 stdin 读取上，信号在当前读取解除阻塞时执行

## 切分行为要点

- 时间切分对齐自然边界：`-t 24h` 在午夜、`-t 1h` 在整点（非"启动时刻 + 周期"）
- 单次写入跨越 `--max-size` 边界时按边界分片，保证每文件 ≤ max-size
- 文件名冲突时自动追加 `.1`/`.2` 序号；淘汰的序号会被复用
- 初始文件追加模式打开，重启幂等
- cronolog 目录形态（如 `%Y/%m/%d/access.log`）的空目录自动逐级回收
- 心跳实时 flush：缓冲数据 ≤2s 内落盘（300ms 心跳、2s 静默阈值），`tail -f` 近实时可见；洪峰由 64KB 缓冲自动批处理

**已知限制**：

- DST 切换日切分点漂移 1 小时（不累积，UTC 不受影响）
- 时钟回拨后归档 mtime 被钳制为当前时间参与保留判定
- `--compress` 硬杀可能残留损坏 `.gz`，配置清理策略可自然淘汰
- 活跃文件被外部删除后写入落入孤儿 inode，静默至下次切分
- 同一目录契约单实例，多实例并发行为未定义
- 归档清理按 glob 形状匹配，**勿将 pattern 指向与其他系统混居的目录**
- 硬杀最多丢失 ~2s 缓冲数据（心跳 idle flush 压缩丢失窗口；优雅关停全量 flush）

## 平台差异

| 功能             | Linux | macOS | Windows                     |
| ---------------- | ----- | ----- | --------------------------- |
| stdin pipe       | ✅    | ✅    | ✅                          |
| SIGHUP 强制切分  | ✅    | ✅    | ❌ (静默忽略)               |
| SIGTERM 优雅退出 | ✅    | ✅    | ❌ (仅 SIGINT)              |
| symlink          | ✅    | ✅    | ⚠️ (需权限，失败时静默跳过) |
| gzip 压缩        | ✅    | ✅    | ✅                          |
| 文件清理         | ✅    | ✅    | ✅                          |

## 开发

```bash
go test ./... -race     # 全部测试（竞态检测），覆盖率要求 > 80%
go vet ./...            # 静态检查
gofmt -l .              # 格式检查（应无输出）

go test ./... -covermode=atomic -coverpkg=./... -coverprofile=cover.out \
  && go tool cover -func=cover.out | tail -1   # 覆盖率

scripts/smoke/run-all.sh   # 集成冒烟（真实二进制 + FIFO 管道，smoke-1~9）
scripts/gates/run-all.sh   # 非功能门禁（内存 / 零丢失 / 跨平台编译 / 覆盖率）
```

## DeepWiki

- <https://deepwiki.com/shengyanli1982/rotatelogs>

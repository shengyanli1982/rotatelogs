# rotatelogs

cli tool: receives process output from stdin pipe, writes byte-for-byte to rotating log files. No parsing, filtering, or modification; fixed memory, near-real-time heartbeat flush; Linux / macOS / Windows.

```bash
myapp 2>&1 | rotatelogs [flags] <filename-pattern>
```

## Quick Start

### Installation

```bash
# Requires Go 1.25+
go install github.com/shengyanli1982/rotatelogs@latest
```

Or build from source:

```bash
git clone https://github.com/shengyanli1982/rotatelogs.git
cd rotatelogs
go build -o rotatelogs .
```

Or precompile releases with the Makefile (requires the `zip` command):

```bash
make release
```

`make build` produces binaries for 4 platforms to `dist/`; `make release` additionally packs zips. Version defaults to `git describe`, overridable with `make VERSION=v1.2.3 release`.

### Running

A minimal runnable example:

```bash
echo "hello" | rotatelogs 'logs/app-%Y-%m-%d.log'
```

It exits gracefully (exit 0) on stdin EOF, and the log lands in `logs/app-YYYY-MM-DD.log` (parent directories are created automatically).

Common combinations:

```bash
# Daily rotation, keep 30 archives with gzip compression
myapp 2>&1 | rotatelogs --max-backups 30 --compress '/var/log/myapp/app-%Y-%m-%d.log'

# Fixed file name, rotate at 50 MB, keep 7 days
myapp 2>&1 | rotatelogs -t 0 -s 50 -a 7 '/var/log/myapp/app.log'
```

## Usage

```
rotatelogs [flags] <filename-pattern>
```

A single command with no subcommands. `filename-pattern` is a required positional argument that uses strftime placeholders to name the log files; missing parent directories are created automatically.

### Flags

| Flag              | Short | Type     | Default | Description                                                                                             |
| ----------------- | ----- | -------- | ------- | ------------------------------------------------------------------------------------------------------- |
| `--max-size`      | `-s`  | uint     | 100     | Maximum size in MB of a single log file; 0 disables the size limit.                                     |
| `--rotation-time` | `-t`  | duration | 24h     | Time-based rotation period; 0 disables time-based rotation. With a placeholder-free pattern, an implicit default downgrades to size-only (warning). |
| `--max-backups`   | `-b`  | int      | 0       | Maximum number of rotated log files to retain; 0 retains all files.                                     |
| `--max-age`       | `-a`  | int      | 0       | Maximum number of days to retain rotated log files; 0 retains files of any age.                         |
| `--compress`      | `-z`  | bool     | false   | Compress rotated log files asynchronously with gzip.                                                   |
| `--symlink`       | `-l`  | string   | ""      | Maintain a symbolic link at this path pointing to the current log file.                                 |
| `--verbose`       | `-v`  | bool     | false   | Print rotation events to stderr.                                                                        |
| `--local-time`    |       | bool     | false   | Use local time instead of the default UTC.                                                              |

Constraints:

- At least one of `--max-size` and `--rotation-time` must be > 0
- When `--rotation-time` > 0, pattern must contain a time placeholder; left at its implicit default (24h) with `--max-size` > 0, the mismatch downgrades to size-only rotation with a one-line warning — an explicitly passed `--rotation-time` remains a startup error
- `--max-age` upper bound: 365000 days (≈1000 years), rejected at startup to prevent overflow
- `--rotation-time` accepts standard Go durations (`h`/`m`/`s`, e.g. `12h`, `1h30m`)

### filename-pattern (strftime format)

strftime placeholders shared with Apache rotatelogs / cronolog / logrotate:

| Placeholder | Meaning              | Example output |
| ----------- | -------------------- | -------------- |
| `%Y`        | Four-digit year      | 2026           |
| `%m`        | Two-digit month      | 11             |
| `%d`        | Two-digit day        | 13             |
| `%H`        | Two-digit hour (24h) | 14             |
| `%M`        | Two-digit minute     | 30             |
| `%S`        | Two-digit second     | 00             |

Only the six placeholders above are supported; **`%%` escaping is not supported**: `%%` is emitted verbatim, unrecognized `%X` is left as-is. Take care when migrating from Apache rotatelogs.

Examples:

- `/var/log/app/app-%Y-%m-%d.log` → daily: `app-2026-11-13.log`
- `/var/log/app/app-%Y-%m-%dT%H.log` → hourly: `app-2026-11-13T14.log`
- `/var/log/app/%Y/%m/app-%d.log` → archive by directory: `2026/11/app-13.log`
- `logs/app-%Y%m%d-%H%M%S.log` → second precision: `app-20261113-143000.log`

When the pattern contains no placeholders, a fixed file name is used, and rotated outputs get a sequence-number suffix: `app.log` → `app.log.1` → `app.log.2` … Use `-t 0` for explicit size-only rotation; leaving `--rotation-time` at its implicit default also runs size-only (downgraded with a warning).

**Limitations**:

- Pattern literals must not contain glob metacharacters `*`, `?`, `[` (plus `\` on Unix) — startup rejected (cleanup uses glob matching, metacharacters cause failures or accidental deletions)
- When cleanup is enabled, pattern base name must start with a literal ("prefix anchor") — names starting with a placeholder are rejected. Example: `app-%Y-%m-%d.log` passes; `%Y-%m-%d.log` rejected (glob becomes `*-*-*.log`)

**Pattern selection advice**: Prefer patterns with time placeholders, or configure `--max-backups`/`--max-age` — both bound sequence-number probing. Fixed file name + `-t 0` without cleanup: probing cost grows linearly with archive count (≈ +2.3µs/archive/rotation).

## Usage Examples

```bash
# Basic usage: daily rotation with a 100 MB size cap
myapp 2>&1 | rotatelogs '/var/log/myapp/app-%Y-%m-%d.log'

# Full setup
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

# Size-only rotation (time-based rotation disabled)
myapp 2>&1 | rotatelogs -t 0 -s 50 '/var/log/myapp/app.log'

# Time-only rotation (size-based rotation disabled)
myapp 2>&1 | rotatelogs -s 0 -t 1h '/var/log/myapp/app-%Y-%m-%dT%H.log'

# Archive by directory (cronolog-style)
myapp 2>&1 | rotatelogs '/var/log/myapp/%Y/%m/%d/access.log'
```

## Exit Codes

| Exit code | Meaning                                                        |
| --------- | -------------------------------------------------------------- |
| 0         | Normal exit (stdin EOF or a termination signal received)       |
| 1         | Startup failure (invalid arguments, cannot create files, etc.) |
| 2         | Runtime I/O error (disk full, permission problems, etc.)       |

Error messages are printed to stderr with `rotatelogs: ` prefix. Auxiliary operation failures (cleanup/compression/symlink) only warn under `--verbose`.

## Signal Behavior

| Signal  | Behavior                                                 | Platform      |
| ------- | -------------------------------------------------------- | ------------- |
| SIGHUP  | Force rotation: sets a flag, triggered on the next write | Unix only     |
| SIGTERM | Graceful shutdown: flush → close → exit(0)               | Unix only     |
| SIGINT  | Graceful shutdown: flush → close → exit(0)               | All platforms |

Notes:

- stdin EOF also triggers graceful shutdown
- Main loop blocks on stdin read; signals take effect when the current read unblocks

## Rotation Behavior

- Time rotation aligns to natural boundaries: `-t 24h` at midnight, `-t 1h` on the hour (not "start + period")
- Single writes crossing `--max-size` are split at the boundary, keeping files ≤ max-size
- File name collisions get `.1`/`.2` suffixes; released numbers are reused
- Initial file opened in append mode, restarts are idempotent
- Cronolog-style empty directories (e.g. `%Y/%m/%d/access.log`) are reclaimed automatically
- Heartbeat idle flush: buffered data hits disk within ≤2s (300ms heartbeat, 2s idle timeout) — `tail -f` works in near real time; bursts are automatically batched by the 64KB buffer

**Known limitations**:

- DST switch days: rotation points drift by 1 hour (not cumulative, UTC unaffected)
- Clock step-back: archive mtime clamped to current time for retention evaluation
- `--compress` hard-kill may leave corrupt `.gz`, cleanup policies age them out naturally
- Active file deleted externally: writes go to orphaned inode until next rotation
- Single instance per directory contract, concurrent behavior undefined
- Cleanup uses glob shape matching — **do not point patterns at directories shared with other systems**
- Hard-kill loses at most ~2s of buffered data (heartbeat idle flush bounds the loss window; graceful shutdown flushes everything)

## Platform Differences

| Feature               | Linux | macOS | Windows                                               |
| --------------------- | ----- | ----- | ----------------------------------------------------- |
| stdin pipe            | ✅    | ✅    | ✅                                                    |
| SIGHUP force rotation | ✅    | ✅    | ❌ (silently ignored)                                 |
| SIGTERM graceful exit | ✅    | ✅    | ❌ (SIGINT only)                                      |
| symlink               | ✅    | ✅    | ⚠️ (requires privileges, silently skipped on failure) |
| gzip compression      | ✅    | ✅    | ✅                                                    |
| file cleanup          | ✅    | ✅    | ✅                                                    |

## Development

```bash
go test ./... -race     # All tests (race detection); coverage requirement > 80%
go vet ./...            # Static analysis
gofmt -l .              # Format check (should produce no output)

go test ./... -covermode=atomic -coverpkg=./... -coverprofile=cover.out \
  && go tool cover -func=cover.out | tail -1   # Coverage

scripts/smoke/run-all.sh   # Integration smoke tests (real binary + FIFO pipes, smoke-1~9)
scripts/gates/run-all.sh   # Non-functional gates (memory / zero-loss / cross-platform compilation / coverage)
```

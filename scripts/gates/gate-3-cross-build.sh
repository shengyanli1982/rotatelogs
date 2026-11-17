#!/usr/bin/env bash
# gate-3 跨平台编译门禁（§14.2-3）：
# GOOS=linux/darwin/windows 三平台逐一 go build 真实二进制产物，全部 exit 0。
# 说明：不用 runbook 原文的 `go build ./...`（会在仓库根落下未忽略的产物）；
# 以 -o 输出到临时目录，main 包链接全部依赖（cmd + internal/rotator，
# 含 windows/unix 双 build-tag 文件），覆盖面等价。

set -euo pipefail
export LC_ALL=C

GATE_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$GATE_DIR/../.." && pwd)"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

cd "$REPO_ROOT"

build_one() { # $1=GOOS $2=GOARCH $3=输出名
	echo "  building $1/$2 …"
	GOOS="$1" GOARCH="$2" go build -o "$OUT/$3" .
	[ -s "$OUT/$3" ] || { echo "GATE3 FAIL: $1/$2 产物为空" >&2; exit 1; }
	file "$OUT/$3" 2>/dev/null || true
}

build_one linux amd64 rotatelogs-linux-amd64
build_one darwin arm64 rotatelogs-darwin-arm64
build_one windows amd64 rotatelogs-windows-amd64.exe

echo "GATE3 PASS: linux/amd64 + darwin/arm64 + windows/amd64 三平台编译通过"

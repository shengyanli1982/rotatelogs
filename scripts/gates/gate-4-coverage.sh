#!/usr/bin/env bash
# gate-4 测试通过 + 覆盖率门禁（§14.2-4）：
# a) go test ./... -race -covermode=atomic 全 PASS + 本包覆盖率 total
# b) go test ./... -coverpkg=./...（合并统计，dispatch 口径）total > 80%
# 产物 cover*.out 为临时文件（.gitignore 已含 *.out），跑完即删。

set -euo pipefail
export LC_ALL=C

GATE_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$GATE_DIR/../.." && pwd)"
cd "$REPO_ROOT"
trap 'rm -f cover.out cover-all.out' EXIT

echo "=== gate-4a: go test ./... -race -covermode=atomic（本包口径）==="
go test ./... -race -covermode=atomic -coverprofile=cover.out -count=1
total_self=$(go tool cover -func=cover.out | tail -1)
echo "  $total_self"

echo "=== gate-4b: go test ./... -coverpkg=./...（合并口径，门禁值）==="
go test ./... -covermode=atomic -coverpkg=./... -coverprofile=cover-all.out -count=1
total_all=$(go tool cover -func=cover-all.out | tail -1)
echo "  $total_all"

pct=$(printf '%s' "$total_all" | awk '{gsub(/%/,"",$NF); print $NF}')
if awk -v p="$pct" 'BEGIN { exit !(p > 80) }'; then
	echo "GATE4 PASS: 全测试 PASS（-race）；合并覆盖率 ${pct}% > 80%（本包口径: $(printf '%s' "$total_self" | awk '{print $NF}')）"
else
	echo "GATE4 FAIL: 合并覆盖率 ${pct}% <= 80%" >&2
	exit 1
fi

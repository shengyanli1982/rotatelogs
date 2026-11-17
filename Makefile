# Makefile for rotatelogs — 跨平台优化编译与 zip 发布打包。
# 平台矩阵：在 gate-3（scripts/gates/gate-3-cross-build.sh）三元组基础上扩展 darwin/amd64。
# 版本号默认取 git describe（本仓无 tag 时自动落 short SHA），可用 make VERSION=v1.2.3 release 覆盖。
# 兼容性：GNU Make 3.81+（macOS 自带版本）；recipe 为 POSIX sh 语法。

BINARY := rotatelogs
DIST := dist
PLATFORMS := linux/amd64 darwin/amd64 darwin/arm64 windows/amd64
LDFLAGS := -s -w
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help build release clean check-zip

help:
	@printf "rotatelogs Makefile\n\n"
	@printf "Usage:\n"
	@printf "  make build     Build optimized binaries for all platforms into $(DIST)/\n"
	@printf "  make release   Build and package one zip archive per platform into $(DIST)/\n"
	@printf "  make clean     Remove the $(DIST)/ directory\n"
	@printf "  make help      Show this help (default target)\n\n"
	@printf "Version override (default: git describe --tags --always --dirty):\n"
	@printf "  make VERSION=v1.2.3 release\n"

# 交叉编译全部平台二进制到 dist/（CGO 禁用 + strip 符号表/DWARF + trimpath）。
build:
	@mkdir -p $(DIST)
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		bin="$(DIST)/$(BINARY)-$$os-$$arch"; \
		if [ "$$os" = windows ]; then bin="$$bin.exe"; fi; \
		printf "==> building %s/%s -> %s\n" "$$os" "$$arch" "$$bin"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" go build -trimpath -ldflags "$(LDFLAGS)" -o "$$bin" .; \
	done

# 打包发布：每平台一个 zip（GitHub release 风格）。
# zip 内条目为短名（rotatelogs / rotatelogs.exe），通过临时副本 + cd 打包实现。
# 打包前 rm -f 旧 zip，避免 zip 更新语义残留陈旧条目（幂等）。
release: check-zip build
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		bin="$(DIST)/$(BINARY)-$$os-$$arch"; \
		inner="$(BINARY)"; \
		if [ "$$os" = windows ]; then bin="$$bin.exe"; inner="$$inner.exe"; fi; \
		zipfile="$(BINARY)-$(VERSION)-$$os-$$arch.zip"; \
		rm -f "$(DIST)/$$zipfile"; \
		cp "$$bin" "$(DIST)/$$inner"; \
		printf "==> packaging %s/%s -> %s\n" "$$os" "$$arch" "$(DIST)/$$zipfile"; \
		(cd "$(DIST)" && zip -q "$$zipfile" "$$inner"); \
		rm -f "$(DIST)/$$inner"; \
	done

# zip 依赖前置检查：缺失时清晰报错并退出非零（须先于 build 执行以快速失败）。
check-zip:
	@command -v zip >/dev/null 2>&1 || { printf "error: 'zip' not found in PATH; install it before running 'make release'\n" >&2; exit 1; }

# 清理构建产物目录。
clean:
	rm -rf "$(DIST)"

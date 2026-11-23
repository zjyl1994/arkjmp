# arkjmp
BINARY      ?= arkjmp
PKG         := ./cmd/arkjmp
VERSION     ?= 3.0.0
GO          ?= go
# 纯 Go 依赖，关掉 cgo 得到静态二进制（可随便拷贝到别的机器）
CGO_ENABLED ?= 0

# 安装位置：默认就是现在软连接所在目录
BINDIR      ?= $(HOME)/.local/bin
CONFDIR     ?= $(HOME)/.config/arkjmp

LDFLAGS     := -s -w -X 'github.com/zjyl1994/arkjmp/internal/cli.Version=$(VERSION)'

.DEFAULT_GOAL := build
.PHONY: all build test test-race bench check fmt-check vet fmt lint staticcheck deadcode install install-config uninstall clean run deps

all: build

## 编译出静态可执行文件 bin/arkjmp
build:
	@mkdir -p bin
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)
	@echo "==> bin/$(BINARY) ($(VERSION))"

## 单元测试（离线，不碰真实 mihomo）
test:
	$(GO) test ./...

## 竞态检测（慢一点，提交前跑）
test-race:
	$(GO) test -race ./...

## 表格/宽度这些热路径的基准
bench:
	$(GO) test ./internal/... -run '^$$' -bench . -benchmem

## 提交前一把梭：格式 + vet + 测试
check: fmt-check vet test

fmt-check:
	@out=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$out" ]; then echo "这些文件需要 gofmt: $$out"; exit 1; fi

vet:
	$(GO) vet ./...

## 直接/间接依赖一览
deps:
	$(GO) list -m -f '{{.Path}} {{.Version}}{{if .Indirect}} (indirect){{end}}' all

## gofmt
fmt:
	gofmt -w ./cmd ./internal

lint: vet staticcheck deadcode

## 静态检查（首次运行会下载工具）
staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

## 找出没人调用的函数
deadcode:
	$(GO) run golang.org/x/tools/cmd/deadcode@latest ./...

## 装到 $(BINDIR)，并把 .env 放到 $(CONFDIR)
install: build install-config
	@mkdir -p $(BINDIR)
	@# 先删掉旧软连接/旧二进制，避免 install 顺着软连接写到别处
	@rm -f $(BINDIR)/$(BINARY)
	install -m 755 bin/$(BINARY) $(BINDIR)/$(BINARY)
	@echo "==> $(BINDIR)/$(BINARY)"
	@command -v $(BINARY) >/dev/null 2>&1 && echo "==> 已在 PATH 中，直接跑 $(BINARY) --help" || echo "==> 注意: $(BINDIR) 不在 PATH 里"

## 生成配置文件（已有则不覆盖）
install-config:
	@mkdir -p $(CONFDIR)
	@if [ ! -f $(CONFDIR)/.env ]; then \
		if [ -f .env ]; then \
			install -m 600 .env $(CONFDIR)/.env; \
			echo "==> 已复制当前 .env 到 $(CONFDIR)/.env"; \
		else \
			install -m 600 .env.example $(CONFDIR)/.env; \
			echo "==> 已写入模板 $(CONFDIR)/.env，记得填 ARKJMP_SECRET"; \
		fi; \
	else \
		echo "==> $(CONFDIR)/.env 已存在，保持不动"; \
	fi

uninstall:
	@rm -f $(BINDIR)/$(BINARY)
	@echo "==> 已删除 $(BINDIR)/$(BINARY)（$(CONFDIR) 保留）"

clean:
	@rm -rf bin
	@echo "==> 已清理 bin/"

## make run ARGS="groups --json"
run: build
	./bin/$(BINARY) $(ARGS)

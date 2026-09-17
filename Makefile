BIN   := mem
PKG   := ./cmd/mem
GOBIN ?= $(shell go env GOPATH)/bin

# 默认构建：CGO_ENABLED=0 —— 纯 Go，单文件静态二进制，可交叉编译
build:
	CGO_ENABLED=0 go build -o bin/$(BIN) $(PKG)

install:
	CGO_ENABLED=0 go install $(PKG)

test:
	CGO_ENABLED=0 go test ./...

# 端到端测试（黑盒）：跑真实二进制，覆盖 安装→初始化→使用→维护→优化→评测→卸载 全生命周期动线。
# 独立于 go test ./...（e2e 目录带 build tag，默认构建不参与覆盖统计）。
test-e2e:
	go test -tags e2e -count=1 -v ./test/e2e

# 覆盖率报告：终端给函数级明细 + 汇总，HTML 给逐行可视化
cover:
	CGO_ENABLED=0 go test ./... -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -html=coverage.out -o coverage.html
	@echo "逐函数明细：go tool cover -func=coverage.out"
	@echo "逐行可视化：coverage.html"

# 覆盖率门禁：任一包低于下限即非零退出。阈值见 coverage-policy.txt（单一真理源）。
cover-gate:
	@./scripts/cover-gate.sh

# 刷新下限（棘轮：只允许上调）。确需降标时显式写：make cover-update FORCE=1
cover-update:
	@./scripts/cover-gate.sh --update $(if $(FORCE),--force,)

# ---------- 发布 / 评测门禁 ----------

# 发布门禁：跑正式版 eval（gold 金标准集）并与 eval-policy.txt 的 recall5_min 比。
# 不达标 → 脚本打印 RED 并返回非零（CI 借此阻止发版）。只判据，不推导版本。
eval-gate:
	@./scripts/release-eval.sh --gate-only

# 预演：只打印下一个将推导出的版本号（不打 tag、不跑 eval）。
release-dry:
	@./scripts/derive-version.sh --repo . --last $$(git describe --tags --abbrev=0 2>/dev/null || echo v0.1.0)

# 发版（本地可预演全流程）：先过覆盖率门禁 → 正式版 eval 门禁（含 recall5）→
# 推导版本 → 写 .mem/reports/v<tag>.md → 本地打 tag。
# 强制升档可传 OVERRIDE=major|minor|patch。
release:
	@make cover-gate
	@TAG=$$(./scripts/release-eval.sh $(if $(OVERRIDE),--override $(OVERRIDE),)); \
	echo "→ 发布版本: $$TAG"; \
	git tag "$$TAG"; \
	echo "已打 tag $$TAG（推送远端：git push origin $$TAG）"

# 交叉编译示例：Go 内建支持，无需交叉工具链
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/$(BIN)-linux-amd64 $(PKG)

build-win:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/$(BIN)-win.exe $(PKG)

# ⚠️ ONNX 本地嵌入（M5b）**未实现**，此处刻意不提供目标。
#
# 曾有一个 `build-onnx` 目标，但代码里没有任何文件使用 `local_onnx` 构建标签，
# 于是它产出的二进制与默认构建**逐字节相同**（实测均 11383042 字节）——
# 一个名字承诺了 ONNX、实际什么也没多出来的目标。按本仓库「预留须显式标注」的
# 原则，宁可没有目标，也不留一个静默存在的假目标。
#
# 现状与决策依据见 DESIGN.md §13：纯 Go 的 sqlite-vec 绑定当前不可用，
# 且向量还需要 embedding 来源；建议先做零包体成本的 M5b-0（复用已有 MinHash 签名）。

smoke: build
	@DB=/tmp/v2mem-smoke.db; rm -f "$$DB" "$$DB-wal" "$$DB-shm"; \
	./bin/$(BIN) add    --db "$$DB" --kind decision "记忆库数据必须放在 ~/.v2mem"; \
	./bin/$(BIN) search --db "$$DB" "记忆库"; \
	./bin/$(BIN) stats  --db "$$DB"

clean:
	rm -rf bin

.PHONY: build install test cover cover-gate cover-update build-linux build-win smoke clean eval-gate release-dry release

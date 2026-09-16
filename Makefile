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

# 交叉编译示例：Go 内建支持，无需交叉工具链
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/$(BIN)-linux-amd64 $(PKG)

build-win:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/$(BIN)-win.exe $(PKG)

# M5 可选增强分支：需要 CGO + libonnxruntime，会失去静态分发与便捷交叉编译
build-onnx:
	CGO_ENABLED=1 go build -tags local_onnx -o bin/$(BIN)-onnx $(PKG)

smoke: build
	@DB=/tmp/v2mem-smoke.db; rm -f "$$DB" "$$DB-wal" "$$DB-shm"; \
	./bin/$(BIN) add    --db "$$DB" --kind decision "记忆库数据必须放在 ~/.v2mem"; \
	./bin/$(BIN) search --db "$$DB" "记忆库"; \
	./bin/$(BIN) stats  --db "$$DB"

clean:
	rm -rf bin

.PHONY: build install test build-linux build-win build-onnx smoke clean

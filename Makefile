# KASandbox 源码仓 Makefile
#
# e2b 编排栈（packages/*）与上游云部署工具（iac/ terraform）已移除，
# 剩余目标只服务 KASandbox 自有模块：cri-multiplex 与 e2b-webhook。

.PHONY: test
test:
	cd cri-multiplex && go test ./...
	cd e2b-webhook && go test ./...

.PHONY: fmt
fmt:
	golangci-lint fmt

.PHONY: lint
lint:
	cd cri-multiplex && golangci-lint run ./...
	cd e2b-webhook && golangci-lint run ./...

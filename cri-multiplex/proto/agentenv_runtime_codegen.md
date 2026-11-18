# AgentENV Runtime 契约生成

唯一契约源位于 AgentENV Runtime 源码树的 `proto/agentenv/runtime/v1/runtime.proto`。`AENV_REPO` 和 `KA_REPO` 分别指这两份源码树的绝对路径；使用与 Runtime RPM 0.2.4-15.10 配套的 AgentENV 源码和补丁，不依赖开发者个人目录。

## Go 消息

当前生成文件使用 protoc 25.1（文件头显示 v4.25.1）、protoc-gen-go v1.36.11。准备对应工具后执行：

```bash
cd "$AENV_REPO"
protoc -I proto \
  --go_out="$KA_REPO/cri-multiplex" \
  --go_opt=module=github.com/cri-multiplex \
  --go_opt=Magentenv/runtime/v1/runtime.proto=github.com/cri-multiplex/pkg/agentenv/runtime/v1 \
  proto/agentenv/runtime/v1/runtime.proto
cd "$KA_REPO/cri-multiplex"
GOWORK=off go test ./pkg/agentenv/runtime/v1
```

`client.go` 使用 `ClientConn.Invoke` 实现轻量 client；它不是生成文件。变更 RPC 时同步更新 method full name 和 client 测试。不得手工修改 `runtime.pb.go`。

## Rust 与变更规则

AgentENV 根 `build.rs` 生成 server/client 和 `agentenv_runtime_descriptor.bin`，通过 `agentenv::runtime_contract::v1` 使用。修改 proto 后重新生成两侧代码并运行跨语言契约测试。

字段编号只增不改；删除字段必须 `reserved`。不得在 KASandbox 复制第二份 proto 事实源。`SandboxAdminService.Remediate` 变更也须同步生成文件、手写 AdminClient 与契约测试。

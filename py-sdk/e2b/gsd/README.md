# GSD — Checkpoint/Restore SDK
SDK 内部通过 Connect-RPC 协议与 VM 内的 GSD 守护进程通信，提供沙箱状态的快照与恢复能力。
## 设计要点
- **独立于 envd**：GSD 使用独立端口(49984)和独立 proto，envd 代码无任何修改
- **checkpoint 不持久化**：数据存储在 VM 内，VM 销毁后丢失（不同于 E2B snapshot API）
## SDK 模块结构
```
e2b/
├── gsd/                              # 底层通信层
│   ├── api.py                        # HTTP 错误映射 + /health 路由常量
│   └── checkpoint/
│       ├── checkpoint_pb2.py         # Protobuf 消息定义
│       ├── checkpoint_pb2.pyi        # Protobuf 类型桩
│       ├── checkpoint_connect.py     # Connect-RPC 客户端
├── sandbox/
│   └── checkpoint/
│       └── types.py                  # 用户面向类型：CheckpointInfo
├── sandbox_sync/
│   └── checkpoint.py                 # 同步 Checkpoint 类
├── sandbox_async/
│   └── checkpoint.py                 # 异步 AsyncCheckpoint 类
├── connection_config.py              # checkpointd_port=49984 + get_checkpointd_url()
├── sandbox/main.py                   # SandboxBase 增加 checkpointd_api_url 属性
```
各层职责：
| 层 | 文件 | 职责 |
|---|---|---|
| 通信层 | `gsd/checkpoint/` | protobuf 编解码 + Connect-RPC HTTP 通信 |
| HTTP 错误处理 | `gsd/api.py` | `/health` 端点的 HTTP 状态码→E2B 异常映射 |
| 用户面向类型 | `sandbox/checkpoint/types.py` | `CheckpointInfo` Python 类 |
| 业务方法 | `sandbox_sync/checkpoint.py` / `sandbox_async/checkpoint.py` | `create()`/`restore()`/`list()`/`delete()`/`is_running()` |
通信层与 envd 共用：`e2b.envd.rpc.handle_rpc_exception()`（RPC 错误映射）和 `ConnectionConfig.sandbox_headers`（认证 header）。
## API 参考
### CheckpointInfo
checkpoint 的元数据对象，由 `create()` 和 `list()` 返回。
| 字段 | 类型 | 说明 |
|---|---|---|
| `checkpoint_id` | `str` | checkpoint ID |
| `name` | `Optional[str]` | 创建时指定的名称 |
| `created_at` | `Optional[int]` | 创建时间戳 |
### 方法
| 方法 | 参数 | 返回类型 | 说明 |
|---|---|---|---|
| `create(name?, timeout?)` | `name`: 可选名称 | `CheckpointInfo` | 创建 checkpoint |
| `restore(checkpoint_id, timeout?)` | `checkpoint_id`: 要恢复的 ID | `bool` | 恢复到指定 checkpoint |
| `list(timeout?)` | 无 | `List[CheckpointInfo]` | 列出所有 checkpoint |
| `delete(checkpoint_id, timeout?)` | `checkpoint_id`: 要删除的 ID | `bool` | 删除指定 checkpoint |
| `is_running(timeout?)` | 无 | `bool` | 检查 GSD 是否运行 |
同步调用：`sandbox.checkpoint.create()`
异步调用：`await sandbox.checkpoint.create()`
## 使用示例
### 同步
```python
from e2b import Sandbox
sandbox = Sandbox.create()
cp = sandbox.checkpoint.create(name="step-1")
print(cp.checkpoint_id)
sandbox.commands.run("pip install numpy")
cp2 = sandbox.checkpoint.create(name="after-install")
sandbox.checkpoint.restore(cp.checkpoint_id)
checkpoints = sandbox.checkpoint.list()
sandbox.checkpoint.delete(cp2.checkpoint_id)
sandbox.checkpoint.is_running()  # True
```
### 异步
```python
from e2b import AsyncSandbox
sandbox = await AsyncSandbox.create()
cp = await sandbox.checkpoint.create(name="step-1")
await sandbox.checkpoint.restore(cp.checkpoint_id)
checkpoints = await sandbox.checkpoint.list()
await sandbox.checkpoint.delete(cp.checkpoint_id)
await sandbox.checkpoint.is_running()
```
## 错误处理
### checkpoint 方法错误
通过 `handle_rpc_exception()` 映射 Connect-RPC 错误码：
| Connect Code | E2B Exception |
|---|---|
| `invalid_argument` | `InvalidArgumentException` |
| `unauthenticated` | `AuthenticationException` |
| `not_found` | `NotFoundException` |
| `unavailable` | 沙箱超时异常 |
| `resource_exhausted` | `RateLimitException` |
| `canceled` / `deadline_exceeded` | `TimeoutException` |
### `is_running()` HTTP 错误
通过 `handle_gsd_api_exception()` 映射 HTTP 状态码：
| HTTP 状态码 | E2B Exception |
|---|---|
| 400 | `InvalidArgumentException` |
| 401 | `AuthenticationException` |
| 404 | `NotFoundException` |
| 502 | 沙箱超时异常（`is_running()` 内部直接返回 `False`，不抛异常） |
| 其他 | `SandboxException("GSD error {code}: {message}")` |
## 注意事项
1. **checkpoint 不持久化**：VM 销毁后所有 checkpoint 数据丢失，不同于 E2B snapshot API（持久化到存储）
2. **与 snapshot API 独立**：checkpoint 是内存级快照，snapshot 是存储级快照，两者互不影响
3. **Proto 代码生成**：`checkpoint_pb2.py`、`checkpoint_pb2.pyi` 由 protoc 从 `checkpoint.proto` 生成，`checkpoint_connect.py` 为手写 Connect-RPC 客户端

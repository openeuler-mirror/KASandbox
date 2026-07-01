# cri-multiplex gRPC E2B 接口验证套件

> 目录：`/home/zrj/cri-multiplex/e2b-verify/`
> 用途：对 cri-multiplex 暴露的 CRI v1 gRPC 接口进行自动化验证，覆盖 PodSandbox / Container 生命周期、镜像管理、Exec / ExecSync / Attach 等能力。

---

## 一、整体架构

### 1.1 目录结构

```
e2b-verify/
├── run_all.sh                # 全量顺序执行调度脚本（入口）
├── 00_setup.sh               # 环境准备：crictl / grpcurl / proto / e2b-pod.json
├── 01_start_multiplex.sh     # 构建 & 启动 cri-multiplex 进程
├── 02_lifecycle.sh           # PodSandbox + Container 生命周期验证
├── 03_image.sh               # ImageService 接口验证
├── 04_exec.sh                # Exec 及附加接口验证
├── 05_execsync.sh            # ExecSync 同步执行验证
├── 06_attach.sh              # Attach 能力验证
├── 07_kubelet_pod_running.sh # kubelet 对接：Pod 保持 Running 状态验证
├── lib/
│   └── common.sh             # 公共函数库（日志 / grpc_call / snapshot 修复 / run_pod_sandbox）
├── test_stream_client.py     # 流式 URL 连接客户端（Exec/Attach 交互）
├── build_prod.py             # 模板构建脚本（snapshot 修复用）
├── test.py                   # Sandbox 创建脚本（snapshot 修复用）
└── .env                      # E2B 环境变量（API KEY / DOMAIN / API URL）
```

### 1.2 分层设计

```
┌─────────────────────────────────────────────────┐
│           run_all.sh（调度层）                   │
│   顺序执行各模块 + 汇总最终报告                  │
└────────────────────┬────────────────────────────┘
                     │ source
┌────────────────────▼────────────────────────────┐
│           lib/common.sh（公共层）                │
│  - 颜色 / 日志（log_info/pass/fail/step）       │
│  - grpc_call：grpcurl 统一封装                  │
│  - handle_snapshot_error：snapshot 错误自愈     │
│  - run_pod_sandbox：带重试的 RunPodSandbox      │
│  - create_and_start_container                   │
│  - cleanup_container / cleanup_pod              │
│  - print_summary                                │
└────────────────────┬────────────────────────────┘
                     │ source
┌────────────────────▼────────────────────────────┐
│        00 ~ 07 验证脚本（用例层）                │
│  每个脚本独立可运行，依赖 common.sh 提供的能力   │
└─────────────────────────────────────────────────┘
```

### 1.3 关键配置（lib/common.sh）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `SOCKET` | `/tmp/cri-multiplex.sock` | cri-multiplex unix socket |
| `PROTO_DIR` | `/tmp/cri-proto` | CRI API proto 文件目录 |
| `POD_JSON` | `/tmp/e2b-pod.json` | Pod 沙箱配置文件 |
| `TEST_PY` | `<dir>/test.py` | snapshot 修复用 sandbox 创建脚本 |
| `BUILD_PROD_PY` | `<dir>/build_prod.py` | snapshot 修复用模板构建脚本 |
| `BUILD_IMAGE_NAME` | `ubuntu:22.04-custom` | 构建模板使用的镜像名 |
| `MULTIPLEX_DIR` | `/home/zrj/cri-multiplex` | cri-multiplex 源码目录 |
| `CONTAINERD_SOCKET` | `/run/containerd/containerd.sock` | containerd socket |
| `POD_UID` | `irlkuj9aask5hmw37uc51` | 测试用 Pod UID（固定） |
| `CONTAINER_ID` | `${POD_UID}-c` | 测试用 Container ID |

---

## 二、模块说明

### 00_setup.sh — 环境准备

检查并准备验证所需的所有工具和配置文件：

1. **检查 crictl** — CRI 命令行工具
2. **检查/安装 grpcurl** — gRPC 调试工具（未安装时通过 `go install` 安装）
3. **准备 CRI API proto** — 下载到 `/tmp/cri-proto/api.proto`
4. **准备 e2b-pod.json** — 默认 Pod 配置（含 E2B 必填 annotations）
5. **检查 kubectl** — snapshot 修复需要
6. **检查 test.py / build_prod.py** — snapshot 修复脚本
7. **检查 .env** — E2B API 凭证

### 01_start_multiplex.sh — 启动 cri-multiplex

1. **检查运行状态** — 进程存在且 socket 可连通则跳过
2. **构建二进制** — `go build ./cmd/cri-multiplex`（如二进制不存在）
3. **启动进程** — 使用 `setsid` 脱离会话，避免脚本退出时进程被杀
   ```bash
   setsid ./cri-multiplex -socket "${SOCKET}" -containerd-socket "${CONTAINERD_SOCKET}" \
       > /tmp/cri-multiplex.log 2>&1 < /dev/null &
   ```
4. **等待 socket 就绪** — 轮询 10 次检查 socket 文件

### 02_lifecycle.sh — 生命周期管理验证（22 个用例）

**一、基础接口**
- 1.1 Version — 验证 RuntimeName=e2b-cri-shim
- 1.2 Status — 验证 RuntimeReady + NetworkReady
- 1.3 UpdateRuntimeConfig — 返回空 {}

**二、PodSandbox 生命周期**
- 2.2 RunPodSandbox — 创建沙箱（带 snapshot 自愈）
- 2.3 幂等测试 — 重复创建返回相同 Pod ID
- 2.4 PodSandboxStatus — state=SANDBOX_READY
- 2.5 ListPodSandbox — 列表包含目标 Pod（前缀匹配）
- 2.6 StopPodSandbox — 停止沙箱
- 2.7 停止后状态 — state=SANDBOX_NOTREADY

**三、Container 生命周期**
- 3.1 重新创建 Pod（容器测试需要运行中的 Pod）
- 3.2 CreateContainer — 创建容器
- 3.3 StartContainer — 启动容器
- 3.4 ContainerStatus — 返回有效 state（E2B 沙箱可能立即退出）
- 3.5 ListContainers — 容器列表查询（容错处理）
- 3.6 StopContainer — 停止容器
- 3.7 停止后状态 — state=CONTAINER_EXITED
- 3.8 RemoveContainer — 删除容器
- 3.9 移除后列表 — 确认已删除

**四、边界条件**
- 6.1 缺失必填 annotation — 返回错误
- 6.2 删除不存在的 Pod — 幂等返回

### 03_image.sh — 镜像管理验证（5 个用例）

- 4.1 PullImage — 拉取镜像
- 4.2 ImageStatus（存在）— 返回镜像信息
- 4.4 ImageStatus（不存在）— 返回空 {}
- 4.5 RemoveImage — 删除镜像
- 4.6 ImageFsInfo — 文件系统信息

### 04_exec.sh — Exec 能力验证（9 个用例）

- 5.2 Exec — 返回流式 URL
- 5.5 UpdateContainerResources — Unimplemented（预期）
- 5.6 CheckpointContainer — Unimplemented（预期）
- 5.7 ContainerStats — 返回统计
- 5.8 ListContainerStats — 返回统计列表
- 5.9 ListMetricDescriptors — Unimplemented（预期）
- 5.10 ListPodSandboxStats — Unimplemented（预期）
- 5.11 ReopenContainerLog — 重开日志
- 5.12 PortForward — Unimplemented（预期）

### 05_execsync.sh — ExecSync 能力验证（4 个用例）

- 5.1.1 echo hello — 验证 stdout
- 5.1.2 多条命令 — sh -c 多行输出
- 5.1.3 exitCode — 检查退出码字段
- 5.1.4 cat /etc/os-release — 验证文件读取

### 06_attach.sh — Attach 能力验证（3 个用例）

- 5.3.1 Exec 启动 sh — 获取 Exec URL 保持 sh 运行
- 5.3.2 调用 Attach — 获取 Attach URL
- 5.3.3 连接 Attach URL — 用 test_stream_client.py 验证输出

### 07_kubelet_pod_running.sh — kubelet 对接 Pod 保持 Running 验证（8 个用例）

通过 kubelet + RuntimeClass=e2b 端到端验证 Pod 生命周期，重点验证修复 `io.kubernetes.container.hash` 缺失导致 kubelet 反复 StopContainer 的问题。

- 1.1 前置检查 — refresh_build_id.sh / cri-multiplex 进程 / RuntimeClass e2b
- 1.2 清理旧 Pod — 删除同名 Pod 并清空日志
- 2.1 刷新 build_id — 执行 `/home/zrj/refresh_build_id.sh` 重建模板并生成 YAML
- 3.1 通过 kubelet 创建 Pod — kubectl apply
- 3.2 等待 Pod 进入 Running — 60s 内轮询 phase=Running 且 ready=true
- 4.1 观察 30s 稳定性 — 持续 Running 且 RESTARTS=0
- 4.2 验证无 Stop 调用 — 日志中无 StopContainer/StopPodSandbox
- 4.3 验证 CreateContainer 仅一次 — 无重复创建（hash 匹配）
- 4.4 验证 sandbox created 仅一次
- 5.1 删除 Pod 验证沙箱销毁 — RemovePodSandbox 被调用

详细修复背景见 [kubelet-pod-running-fix.md](./kubelet-pod-running-fix.md)。

---

## 三、核心机制

### 3.1 Snapshot 错误自愈流程

**触发条件**：RunPodSandbox 返回 `Put "http://localhost/snapshot/load": EOF` 错误。

**原因**：StopContainer / StopPodSandbox 后，E2B 后端的 snapshot 状态被破坏，下次启动沙箱会失败。

**修复流程**（`handle_snapshot_error` 函数）：

```
检测到 snapshot/load EOF 错误
        │
        ▼
Step 1: python3 build_prod.py ubuntu:22.04-custom
        （重新构建模板，生成新 build）
        │
        ▼
Step 2: python3 test.py
        （创建新 Sandbox，使 snapshot 就绪）
        │
        ▼
Step 3: kubectl logs api-8cfbf9cfd-vbfw6 -ne2b | grep "base_template_id" | tail -1
        （获取最新日志）
        │
        ▼
Step 4: 提取 build_id（grep -oP '"build_id":\s*"\K[^"]+'）
        │
        ▼
Step 5: sed -i 更新 /tmp/e2b-pod.json 中的 e2b.dev/build-id
        │
        ▼
返回 0，RunPodSandbox 重试
```

**重试策略**（`run_pod_sandbox` 函数）：
- 最多重试 5 次
- snapshot 修复只执行一次（`snapshot_fixed` 标志）
- 修复后若仍失败，等待 5 秒后重试

### 3.2 进程管理

**问题**：脚本退出时 cri-multiplex 进程被杀。

**解决**：
```bash
setsid ./cri-multiplex ... < /dev/null &
```
- `setsid` 创建新会话，脱离脚本进程组
- `< /dev/null` 关闭 stdin，避免等待输入

**精确进程匹配**：
```bash
pkill -9 -f "cri-multiplex -socket"
```
- 只匹配带 `-socket` 参数的实际进程
- 避免误杀脚本自身（脚本路径也含 "cri-multiplex"）

### 3.3 日志输出机制

所有日志函数输出到 **stderr**：
```bash
log_info()  { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
log_pass()  { echo -e "${GREEN}[PASS]${NC}  $*" >&2; ... }
```

**原因**：`run_all.sh` 用 `$(...)` 捕获子脚本输出解析计数，如果日志输出到 stdout 会被捕获导致解析错误。输出到 stderr 保证日志正常显示，stdout 只用于返回值。

### 3.4 E2B 特性适配

| 特性 | 处理方式 |
|------|----------|
| Container 启动后可能立即退出 | ContainerStatus 检查返回有效 state 即可，不强制 RUNNING |
| Container 不在 crictl ps 列表 | ListContainers 用 grpcurl 验证 + 容错处理 |
| crictl 截断 ID 到 12 位 | ListPodSandbox/ListContainers 用 `${ID:0:12}` 前缀匹配 |
| ExecSync 不严格传递非零退出码 | exitCode 检查字段存在即可 |
| 部分接口未实现 | Unimplemented 作为预期行为通过 |

---

## 四、使用方式

### 4.1 全量验证

```bash
cd /home/zrj/cri-multiplex/e2b-verify
bash ./run_all.sh
```

**输出示例**：
```
╔══════════════════════════════════════════════════════════════════╗
║                    全量验证最终汇总报告                          ║
╠══════════════════════════════════════════════════════════════════╣
║ [00]  验证工具安装与环境准备                    PASS              ║
║ [01]  启动 cri-multiplex                        PASS              ║
║ [02]  生命周期管理验证                          PASS(22/0/0)      ║
║ [03]  镜像管理验证                              PASS(5/0/0)       ║
║ [04]  Exec 能力验证                             PASS(9/0/0)       ║
║ [05]  ExecSync 能力验证                         PASS(4/0/0)       ║
║ [06]  Attach 能力验证                           PASS(3/0/0)       ║
║ [07]  kubelet 对接 Pod 保持 Running 验证        PASS(8/0/0)       ║
╠══════════════════════════════════════════════════════════════════╣
║  PASS: 51     FAIL: 0      SKIP: 0      TOTAL: 51               ║
╚══════════════════════════════════════════════════════════════════╝

✓ 全部验证通过！
```

### 4.2 单独验证某个模块

```bash
# 只运行 02 生命周期验证
bash ./run_all.sh --only 02

# 只运行 04 Exec 验证
bash ./run_all.sh --only 04

# 跳过环境准备（已安装过工具）
bash ./run_all.sh --skip-setup
```

### 4.3 直接运行单个脚本

```bash
bash ./02_lifecycle.sh
bash ./03_image.sh
bash ./04_exec.sh
```

### 4.4 查看帮助

```bash
bash ./run_all.sh --help
```

---

## 五、验证接口清单

### RuntimeService（runtime.v1.RuntimeService）

| 接口 | 验证模块 | 验证内容 |
|------|----------|----------|
| Version | 02 | RuntimeName=e2b-cri-shim |
| Status | 02 | RuntimeReady + NetworkReady |
| UpdateRuntimeConfig | 02 | 返回空 {} |
| RunPodSandbox | 02 | 创建沙箱（带 snapshot 自愈） |
| PodSandboxStatus | 02 | SANDBOX_READY / SANDBOX_NOTREADY |
| ListPodSandbox | 02 | 列表包含目标 Pod |
| StopPodSandbox | 02 | 停止沙箱 |
| RemovePodSandbox | 02 | 删除沙箱 + 幂等测试 |
| CreateContainer | 02 | 返回 containerId |
| StartContainer | 02 | 返回空 |
| ContainerStatus | 02 | 返回有效 state |
| ListContainers | 02 | 列表查询 |
| StopContainer | 02 | 返回空 |
| RemoveContainer | 02 | 返回空 |
| ExecSync | 05 | stdout / exitCode |
| Exec | 04 | 返回流式 URL |
| Attach | 06 | 返回流式 URL + 连接验证 |
| UpdateContainerResources | 04 | Unimplemented（预期） |
| CheckpointContainer | 04 | Unimplemented（预期） |
| ContainerStats | 04 | 返回统计 |
| ListContainerStats | 04 | 返回统计列表 |
| ListMetricDescriptors | 04 | Unimplemented（预期） |
| ListPodSandboxStats | 04 | Unimplemented（预期） |
| ReopenContainerLog | 04 | 成功 |
| PortForward | 04 | Unimplemented（预期） |

### ImageService（runtime.v1.ImageService）

| 接口 | 验证模块 | 验证内容 |
|------|----------|----------|
| PullImage | 03 | 拉取镜像 |
| ImageStatus | 03 | 存在/不存在两种情况 |
| RemoveImage | 03 | 删除镜像 |
| ImageFsInfo | 03 | 文件系统信息 |

### kubelet 端到端链路（07 模块）

| 接口 | 验证模块 | 验证内容 |
|------|----------|----------|
| RunPodSandbox | 07 | kubelet 经 RuntimeClass=e2b 触发，沙箱创建一次 |
| CreateContainer | 07 | hash/restartCount 从 annotations 搬运到 labels，仅调用一次 |
| StartContainer | 07 | kubelet 启动容器 |
| ListContainers / ListPodSandbox | 07 | PLEG 周期查询，hash 匹配无重建 |
| ContainerStatus / PodSandboxStatus | 07 | 返回 Running，RESTARTS=0 |
| StopContainer / StopPodSandbox | 07 | 不被误调用（保持 Running） |
| RemovePodSandbox | 07 | Pod 删除时触发沙箱销毁 |

---

## 六、依赖文件说明

### 6.1 e2b-pod.json

Pod 沙箱配置文件，包含 E2B 必填的 annotations：
- `e2b.dev/template-id` / `e2b.dev/base_template_id` — 模板 ID
- `e2b.dev/build-id` — 构建 ID（snapshot 修复时自动更新）
- `e2b.dev/vcpu` / `e2b.dev/ram-mb` — 资源规格
- `e2b.dev/envd-access-token` — 访问令牌
- `e2b.dev/execution-id` — 执行 ID

### 6.2 build_prod.py

模板构建脚本，调用 `e2b.Template.build()` 重新构建模板：
```bash
python3 build_prod.py ubuntu:22.04-custom
```
- 镜像名 `:` 和 `.` 替换为 `-` 作为 alias
- Dockerfile: `FROM harbor:443/e2b-orchestration/{image_name}`

### 6.3 test.py

Sandbox 创建脚本，调用 `e2b.Sandbox.create()`：
```python
sbx = Sandbox.create("ubuntu-22-04-custom")
sbx.commands.run("whoami")  # 验证可用
sbx.kill()
```
- 读取 `.env` 中的 E2B_ACCESS_TOKEN / E2B_API_KEY
- 创建后立即 kill，仅用于触发 snapshot 就绪

### 6.4 .env

E2B 环境变量配置：
```env
E2B_ACCESS_TOKEN="sk_e2b_..."
E2B_API_KEY="e2b_..."
E2B_DOMAIN="e2b.app"
E2B_API_URL="http://193.13.1.2:3000"
E2B_HTTP_SSL="false"
```

### 6.5 test_stream_client.py

流式 URL 连接客户端，用于 Exec/Attach 交互验证：
- 解析 HTTP URL，建立 TCP 连接
- 发送 GET 请求升级连接
- 从 stdin 读取输入转发到 socket
- 实时输出 socket 返回的数据

```bash
echo "echo hello" | python3 test_stream_client.py http://host:port/exec/xxx 5
```

---

## 七、扩展指南

### 7.1 新增验证脚本

1. 创建 `07_xxx.sh`，头部 source common.sh：
   ```bash
   #!/bin/bash
   set -euo pipefail
   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
   source "${SCRIPT_DIR}/lib/common.sh"
   log_section "07 — 新验证模块"
   # ... 用例 ...
   print_summary
   exit 0
   ```

2. 在 `run_all.sh` 的 `SCRIPTS` 数组中添加：
   ```bash
   "07|新验证模块|${SCRIPT_DIR}/07_xxx.sh"
   ```

### 7.2 新增测试用例

在对应脚本中使用 `log_pass` / `log_fail` / `log_skip`：
```bash
log_step "用例名称"
output=$(grpc_call "runtime.v1.RuntimeService/XXX" '{"...":"..."}') || true
if echo "${output}" | grep -q "期望内容"; then
    log_pass "用例通过"
else
    log_fail "用例失败: ${output}"
fi
```

### 7.3 调整配置

通过环境变量覆盖默认配置：
```bash
SOCKET=/tmp/custom.sock bash ./run_all.sh
BUILD_IMAGE_NAME=ubuntu:24.04-custom bash ./run_all.sh
```

---

## 八、故障排查

### 8.1 cri-multiplex 启动失败

```bash
# 查看日志
cat /tmp/cri-multiplex.log

# 手动启动排查
cd /home/zrj/cri-multiplex
./cri-multiplex -socket /tmp/cri-multiplex.sock -containerd-socket /run/containerd/containerd.sock
```

### 8.2 snapshot/load EOF 持续失败

手动执行修复流程：
```bash
cd /home/zrj/cri-multiplex/e2b-verify
python3 build_prod.py ubuntu:22.04-custom
python3 test.py
# 获取最新 build_id
kubectl logs api-8cfbf9cfd-vbfw6 -ne2b | grep "base_template_id" | tail -1
# 更新 e2b-pod.json
sed -i 's|"e2b.dev/build-id":.*|"e2b.dev/build-id": "新build_id"|' /tmp/e2b-pod.json
```

### 8.3 进程残留清理

```bash
pkill -9 -f "cri-multiplex -socket"
rm -f /tmp/cri-multiplex.sock
```

### 8.4 grpcurl proto 报错

```bash
# 重新下载 proto
rm -rf /tmp/cri-proto
bash ./00_setup.sh
```

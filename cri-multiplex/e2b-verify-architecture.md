# cri-multiplex gRPC E2B 接口验证套件

> 目录：`cri-multiplex/e2b-verify/`（脚本按自身位置解析仓库路径）
> 用途：对 cri-multiplex 暴露的 CRI v1 gRPC 接口进行自动化验证，覆盖 PodSandbox / Container 生命周期、镜像管理、Exec / ExecSync / Attach，以及 kubelet / CNI / Android / 状态恢复 / Pause-Checkpoint-Resume 等链路。

---

## 一、整体架构

### 1.1 目录结构

```
e2b-verify/
├── run_all.sh                # 全量顺序执行调度脚本（入口，00–23）
├── 00_setup.sh               # 环境准备：工具 / proto / E2B 模板 / Pod fixture / 能力检查
├── 01_start_multiplex.sh     # 构建 & 启动 cri-multiplex（非 CNI / CNI / Android / 池化）
├── 02_lifecycle.sh           # PodSandbox + Container 生命周期 + annotation 校验
├── 03_image.sh               # ImageService 接口验证
├── 04_exec.sh                # 直连 CRI Exec 及附加接口验证
├── 05_execsync.sh            # 直连 CRI ExecSync 同步执行验证
├── 06_attach.sh              # 直连 CRI Attach + WebSocket 客户端验证
├── 07_kubelet_pod_running.sh # kubelet 对接：Pod 保持 Running 状态验证
├── 08_exec_kubelet.sh        # kubelet Exec + 附加接口 + SIGTERM
├── 09_execsync_kubelet.sh    # kubelet probe/lifecycle hook 触发 ExecSync
├── 10_attach_kubelet.sh      # kubelet Attach（kubectl attach）
├── 11_cni_podip_access.sh    # CNI PodIP / netns / envd health / 外网访问
├── 12_android_kubelet_sandbox.sh # Android RuntimeClass 单实例
├── 13_android_multi_sandbox.sh   # Android 多实例
├── 14_cni_service_dns.sh     # CNI Service / EndpointSlice / DNS
├── 15_cni_networkpolicy_ingress.sh # NetworkPolicy ingress
├── 16_cni_networkpolicy_egress.sh  # 沙箱出向隔离 / NetworkPolicy egress
├── 17_android_cni_podip.sh   # Android CNI netns / ADB / guest 网络
├── 18_state_restore.sh       # cri-multiplex 重启恢复验证
├── 19_state_persistence_matrix.sh # 状态持久化矩阵
├── 20_cleanup_orphan_recovery.sh  # 清理 / 孤儿回收 / 回滚
├── 21_mux_multi_runtime_routing.sh # 多 Runtime 路由
├── 22_pause_checkpoint_resume.sh   # Pause / Checkpoint / Resume
├── 23_restart_multiplex_hide_direct.sh # 隐藏 direct sandbox
├── 24_bulk_direct_concurrency.sh      # 性能：100/200 并发（无池）
├── 25_bulk_direct_concurrency_pool.sh # 性能：100/200 并发（带池）
├── p01_bulk_cni_start_100.sh          # 性能：kubelet 批量创建（默认 95）
├── configure_network_mode.sh # 切换 non-CNI / CNI
├── switch_cni_pool.sh        # 切换 CNI 预热池
├── lib/
│   ├── common.sh             # 公共函数库（日志 / grpc_call / 进程 / Pod/Container）
│   ├── e2b_env.sh            # E2B 凭证、模板解析、Pod fixture 生成
│   ├── systemd_mux.sh        # systemd 接管/恢复
│   ├── cni_behavior_common.sh# Service/DNS/NetworkPolicy 公共断言
│   ├── bulk_direct.sh        # 并发批次/耗时统计/预热池等待
│   └── refresh_build_id.sh   # 生成当前 live 模板的 Pod YAML
├── test_stream_client.py     # 流式 URL 连接客户端（Exec/Attach 交互）
├── build_prod.py             # 可选：模板重建（snapshot 修复回退）
├── test.py                   # 可选：Sandbox 创建冒烟（snapshot 修复回退）
├── .env                      # E2B 环境变量（API KEY / DOMAIN / API URL）
└── logs/                     # 历史回归日志归档
```

### 1.2 分层设计

```
┌─────────────────────────────────────────────────┐
│  环境层：kubelet → /run/cri-multiplex.sock       │
│  接管 systemd 实例，只换二进制，不改 kubelet     │
└────────────────────┬────────────────────────────┘
                     │
┌────────────────────▼────────────────────────────┐
│           run_all.sh（调度层）                   │
│   顺序执行 00–23 + 汇总最终报告                  │
│   按用例切换运行模式 / 准备共享 E2B fixture      │
└────────────────────┬────────────────────────────┘
                     │ source
┌────────────────────▼────────────────────────────┐
│           lib/common.sh（公共层）                │
│  - 颜色 / 日志（log_info/pass/fail/step）       │
│  - grpc_call：grpcurl 统一封装                  │
│  - handle_snapshot_error：刷新 live 模板元数据  │
│  - run_pod_sandbox：带重试的 RunPodSandbox      │
│  - create_and_start_container                   │
│  - cleanup_container / cleanup_pod              │
│  - print_summary                                │
│  - 子库：e2b_env / systemd_mux /                │
│    cni_behavior_common / bulk_direct            │
└────────────────────┬────────────────────────────┘
                     │ source
┌────────────────────▼────────────────────────────┐
│        00 ~ 23 验证脚本（用例层）                │
│  每个脚本独立可运行，依赖 common.sh 提供的能力   │
│  24/25/p01 性能脚本独立运行，不进入 run_all      │
└─────────────────────────────────────────────────┘
```

### 1.3 关键配置（lib/common.sh / lib/e2b_env.sh）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `SOCKET` | kubelet 的 runtime endpoint（通常 `/run/cri-multiplex.sock`） | 与 kubelet 共用同一 socket，不接受环境变量覆盖 |
| `STATE_DIR` | `/tmp/cri-multiplex-state` | 状态持久化目录（所有用例共用） |
| `MULTIPLEX_LOG` | `/tmp/cri-multiplex.log` | cri-multiplex 进程日志 |
| `PROTO_DIR` | `/tmp/cri-proto` | CRI API proto 文件目录 |
| `POD_JSON` | `/tmp/e2b-pod.json` | Pod 沙箱配置文件 |
| `TEST_PY` / `BUILD_PROD_PY` | `<dir>/test.py` / `<dir>/build_prod.py` | 可选回退脚本；默认流程不调用 |
| `BUILD_IMAGE_NAME` | `ubuntu:22.04-custom` | 显式重建模板时使用的镜像名 |
| `MULTIPLEX_DIR` | `e2b-verify/..` | cri-multiplex 源码目录（自动解析） |
| `MULTIPLEX_BIN` | `${MULTIPLEX_DIR}/cri-multiplex` | 本地构建产物；可回退 `/opt/e2b-infra/bin/cri-multiplex` |
| `CONTAINERD_SOCKET` | `/run/containerd/containerd.sock` | containerd socket |
| `ORCHESTRATOR_ADDRESS` / `ORCHESTRATOR_PROXY_ADDRESS` | `localhost:5008` / `localhost:5007` | E2B orchestrator / proxy 地址 |
| `CNI_CONF_DIR` | `/etc/cni/net-e2b.d` | E2B 专用 CNI 配置目录 |
| `E2B_API_URL` / `E2B_TEMPLATE_ALIAS` | `http://127.0.0.1:3000` / `ubuntu-22-04-custom-1` | E2B API 与模板别名 |
| `ANDROID_ARTIFACTS_DIR` | 自动探测 | launch_cvd artifacts 目录 |
| `HIDE_SANDBOX_LABEL` | 空 | 传给 `-hide-sandbox-label`（23 使用） |
| `POD_UID` / `CONTAINER_ID` | `irlkuj9aask5hmw37uc51` / `${POD_UID}-c` | 默认值；直连用例会生成唯一 UID |

---

## 二、模块说明

### 00_setup.sh — 环境准备

检查并准备验证所需的所有工具和配置文件：

1. **检查 crictl** — CRI 命令行工具
2. **检查/安装 grpcurl** — gRPC 调试工具（未安装时通过 `go install` 安装）
3. **准备 CRI API proto** — 下载到 `/tmp/cri-proto/api.proto`
4. **解析 E2B 模板并生成 e2b-pod.json** — 从 E2B API `/templates` 动态解析 template/build/team/规格，
   生成含 E2B 必填 annotations 的 Pod JSON
5. **检查 kubectl** — kubelet / CNI / 状态用例需要
6. **检查 test.py / build_prod.py** — 可选回退脚本，新流程默认不依赖
7. **检查 .env** — E2B API 凭证；`/root/.e2b/config.json` 的 `teamApiKey` 优先生效
8. **能力检查** — CNI 配置目录、Android artifacts、systemd `cri-multiplex.service` 状态

### 01_start_multiplex.sh — 启动 cri-multiplex

1. **检查运行状态与模式** — socket 可连通且 `cmdline` 中的 CNI/Android 模式匹配则复用；
   `E2B_FORCE_RESTART=1` 或模式不一致时强制重启
2. **systemd 接管** — 仅当 `E2B_FORCE_RESTART=1` 且 `SOCKET` 命中 `/run/cri-multiplex.sock` 时，
   先 `systemctl stop cri-multiplex.service` 并写 marker；恢复用 `--restore`
3. **构建二进制** — `GOWORK=off go build -o ${MULTIPLEX_BIN} ./cmd/cri-multiplex`；源码更新或
   `E2B_FORCE_BUILD=1` 时重建，`E2B_SKIP_BUILD=1` 时只检查已有二进制
4. **启动进程** — 使用 `setsid` 脱离会话，避免脚本退出时进程被杀
   ```bash
   setsid "${MULTIPLEX_BIN}" -socket "${SOCKET}" -containerd-socket "${CONTAINERD_SOCKET}" \
       -orchestrator-address "${ORCHESTRATOR_ADDRESS}" -state-dir "${STATE_DIR}" \
       > "${MULTIPLEX_LOG}" 2>&1 < /dev/null &
   ```
   CNI / Android / 池化 / hide-direct 以及 orphan 对账参数按需追加
5. **等待 socket 就绪** — 最多轮询 15 次 `crictl info`

### 02_lifecycle.sh — 生命周期管理验证（含 annotation 校验）

前置：调用 `start_non_cni_multiplex` 强制非 CNI 模式；直连用例通过 `prepare_direct_pod_json`
生成独立 UID，避免互相覆盖。

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
- 3.3 StartContainer — 启动容器（带重试）
- 3.4 ContainerStatus — 返回有效 state（E2B 沙箱可能立即退出）
- 3.5 ListContainers — 容器列表查询（grpcurl + crictl 容错）
- 3.6 StopContainer — 停止容器
- 3.7 停止后状态 — state=CONTAINER_EXITED
- 3.8 RemoveContainer — 删除容器
- 3.9 移除后列表 — 确认已删除

**四、边界条件**
- 6.1 缺失必填 annotation — 返回错误
- 6.2 只保留必填 annotation — 使用默认值创建成功
- 6.3 annotation 校验 — `vcpu=0`、`ram-mb=-1`、`max-sandbox-length=0`、
  `total-disk-size-mb=0`、`allow-internet=invalid`、`huge-pages=invalid`、
  `auto-pause=invalid`、`snapshot=invalid`、`auto-resume` 非法 JSON / 非法 policy
- 6.4 删除不存在的 Pod — 幂等返回

### 03_image.sh — 镜像管理验证

- 先通过 `refresh_or_reuse_e2b_yaml` 解析当前 live 模板，拼出 `e2b.dev/<template-id>:<build-id>`，
  并把模板信息同步回 `${POD_JSON}`
- 4.1 PullImage — 拉取镜像
- 4.2 ImageStatus（存在）— 返回镜像信息
- 4.4 ImageStatus（不存在）— 返回空 {}
- 4.5 RemoveImage — 删除镜像
- 4.6 ImageFsInfo — 文件系统信息

### 04_exec.sh — Exec 能力验证（9 个用例）

前置：切换到非 CNI 模式并创建独立 Pod + Container。

- 5.2 Exec — 返回流式 URL
- 5.5 UpdateContainerResources — Unimplemented（预期）
- 5.6 CheckpointContainer — Unimplemented（预期；真正的 checkpoint 见 22 号 Admin API）
- 5.7 ContainerStats — 返回统计
- 5.8 ListContainerStats — 返回统计列表
- 5.9 ListMetricDescriptors — Unimplemented（预期）
- 5.10 ListPodSandboxStats — Unimplemented（预期）
- 5.11 ReopenContainerLog — 重开日志
- 5.12 PortForward — Unimplemented（预期）

### 05_execsync.sh — ExecSync 能力验证（4 个用例）

每个用例都新建 Pod + Container，避免上一个容器退出影响下一个。

- 5.1.1 echo hello — 验证 stdout
- 5.1.2 多条命令 — sh -c 多行输出
- 5.1.3 exitCode — 检查退出码字段（非零退出码不严格时也接受）
- 5.1.4 cat /etc/os-release — 验证文件读取

### 06_attach.sh — Attach 能力验证（3 个用例）

- 5.3.1 创建 Pod + Container（失败最多重建 3 次）
- 5.3.2 调用 Attach — 获取 Attach URL
- 5.3.3 连接 Attach URL — 用 test_stream_client.py 按 `v5.channel.k8s.io` 协议发送 stdin，
  验证收到 `attach_test_ok` 回显

### 07_kubelet_pod_running.sh — kubelet 对接 Pod 保持 Running 验证

通过 kubelet + RuntimeClass=e2b 端到端验证 Pod 生命周期，重点验证修复 `io.kubernetes.container.hash` 缺失导致 kubelet 反复 StopContainer 的问题。前置通过 `ensure_controlled_cni_multiplex` 准备 CNI 实例。

- 1.1 前置检查 — refresh_build_id.sh / cri-multiplex 进程 / RuntimeClass e2b
- 1.2 清理旧 Pod — 删除同名 Pod 并清空日志
- 2.1 刷新 build_id — 执行 `lib/refresh_build_id.sh` 解析 live 模板并生成 YAML
- 3.1 通过 kubelet 创建 Pod — kubectl apply
- 3.2 等待 Pod 进入 Running — 60s 内轮询 phase=Running 且 ready=true
- 4.1 观察 30s 稳定性（可配 `OBSERVE_SECONDS`）— 持续 Running 且 RESTARTS=0
- 4.2 验证无 Stop 调用 — 当前 Pod 的日志中无 StopContainer/StopPodSandbox
- 4.3 验证 CreateContainer 仅一次 — 无重复创建（hash 匹配）
- 4.4 验证 sandbox created 仅一次
- 5.1 删除 Pod 验证沙箱销毁 — 观察到 RemoveContainer 被调用

详细修复背景见 [kubelet-pod-running-fix.md](./kubelet-pod-running-fix.md)。

### 08_exec_kubelet.sh — kubelet Exec 验证

- 通过 kubelet 创建 RuntimeClass=e2b Pod，等待 Running
- 5.2 `kubectl exec` 验证 CRI Exec streaming，输出 `exec_test_ok`
- 5.5–5.11 直接 gRPC 验证附加接口；容器已不存在时部分项按 SKIP 处理
- 5.12 长耗时 exec 期间发送 `SIGTERM`，验证 exec 及时退出、进程停止并重启实例

### 09_execsync_kubelet.sh — kubelet 触发 ExecSync 验证

- 注入 startup/readiness/liveness `exec` probe 和 postStart/preStop `exec` hook
- 等待日志出现 `[GrpcE2BEngine] ExecSync: ... <marker>`，验证 kubelet 真实触发 ExecSync
- 检查 `kubectl describe` 无 probe failed / PostStartHookError
- 用 `kubectl exec` 验证非交互命令、退出码、文件读取（属于 CRI Exec streaming）
- 删除 Pod 后等待 preStop 对应的 ExecSync 日志

### 10_attach_kubelet.sh — kubelet Attach 验证

- 生成 Pod YAML 后改为 `stdin: true`、`tty: true`、`command: ["sh"]`
- Pod Ready 后执行 `kubectl attach -i`，发送 `echo attach_test_ok`，验证回显

### 11_cni_podip_access.sh — CNI PodIP 访问 E2B 沙箱

- CNI 模式下创建 E2B Pod，校验 PodIP 与 CRI `PodSandboxStatus.Network.Ip` 一致
- CRI annotations 包含 `e2b.dev/cni-enabled`、`e2b.dev/pod-ip`、`e2b.dev/cni-netns`
- netns 内 `eth0=<PodIP>`、`tap0=169.254.0.22/30`
- 宿主机访问 `PodIP:49983/health`，并验证沙箱内 DNS/HTTPS 外网访问
- 删除 Pod 后 netns 清理

### 12_android_kubelet_sandbox.sh — Android RuntimeClass 单实例

- 前置检查 `launch_cvd`、`cvd`、`/dev/kvm`、`/dev/net/tun`
- 启动 CNI+Android 模式，创建 `RuntimeClass android` 和 Android Pod
- 校验 CRI status 的 `android.dev/cni-enabled`、`pod-ip`、`cni-netns`、`adb-url`、`adb-port`
- 等待 ADB 端口可连接，删除 Pod 后等待 Pod/CRI/netns/ADB/PGID 清理

### 13_android_multi_sandbox.sh — Android 多实例

- 同时创建 `base_instance_num=1/ADB 6520` 和 `base_instance_num=2/ADB 6521` 两个 Pod
- 分别校验 CRI status 与 ADB 端口，删除后等待资源收敛

### 14_cni_service_dns.sh — E2B CNI Service / DNS

- 创建 E2B Pod、Service、普通 client Pod；EndpointSlice 必须包含 E2B PodIP
- 宿主机与 client Pod 均可访问 E2B PodIP / Service DNS
- E2B VM 内 Kubernetes DNS 为 best-effort；验证 VM 访问节点 IP 不被出向隔离误伤

### 15_cni_networkpolicy_ingress.sh — NetworkPolicy ingress

- baseline：allowed/denied client 均可访问 E2B PodIP
- 应用 deny-all ingress，期望阻断；若未阻断则记 SKIP（CNI POC 边界）并回归入向保留
- 若阻断，再应用 allow selected client，验证只放行 allowed client

### 16_cni_networkpolicy_egress.sh — 沙箱出向隔离 / NetworkPolicy egress

- 前置：orchestrator 注入 `SANDBOX_DENIED_POD_CIDR`
- 启用隔离时验证沙箱 netns 的 `filtered_always_denylist` 包含 Pod CIDR
- 启用隔离时验证沙箱 A 不能访问沙箱 B PodIP、不能访问 Pod CIDR 内普通 Pod
- 临时向 `filtered_always_allowlist` 放通目标 IP，验证例外生效
- 验证 `allow-internet=false` 的 TCP 出向被阻断；deny-all egress policy 为 best-effort

### 17_android_cni_podip.sh — Android CNI PodIP / Netns

- 校验 Android CRI status 的 CNI/guest 字段（`guest-ip`、`guest-gateway`、`guest-tap`）
- 校验 netns、ADB 监听、`adb connect`、guest eth1 与 tap 配置
- 验证 Android guest 经 CNI 访问集群 TCP 服务，以及 PodIP 访问 guest 内服务
- 验证 guest DNS；删除 Pod 后 netns 清理

### 18_state_restore.sh — 重启恢复验证

- 清空并启动 CNI+Android 模式，创建 E2B Pod，校验 `state.json` 已持久化
- 重启 cri-multiplex 后 Pod 仍 Running/Ready，`kubectl exec` 仍可用
- 删除 Pod，等待 `RemovePodSandbox`，确认 state/routes 清理

### 19_state_persistence_matrix.sh — 状态持久化矩阵

- 非法 JSON 启动容错；单 Pod/Container 状态持久化
- 重启恢复、12 并发 get + 5 并发 exec、delete 后清理
- 并发创建/删除 10 个 E2B Pod；5 个 E2B + 5 个普通 Pod 混合
- `kill -9` 后恢复；缺少 template-id 的失败可观测；非法 state-dir 启动失败

### 20_cleanup_orphan_recovery.sh — 清理与孤儿回收

- Orchestrator / CNI 不可用时的错误诊断与无残留
- E2B 删除幂等（state/routes、netns、HostPort 清理）
- 重启后回收 missing sandbox；创建失败回滚
- Android 删除幂等/端口复用、进程异常退出、重启孤儿回收
- 非 owner 目录和 iptables 规则保护（`skip unknown owner`）

### 21_mux_multi_runtime_routing.sh — 多 Runtime 路由

- 无 RuntimeClass Pod → containerd；Android Pod → Android；E2B Pod → E2B
- `ListPodSandbox` / `ListContainers` 多后端合并
- Exec/Attach 按 containerID 路由，Android `PodSandboxStatus` 按 sandboxID 路由
- Android artifacts 缺失时只验证 containerd + E2B 双 runtime

### 22_pause_checkpoint_resume.sh — Pause / Checkpoint / Resume

- §0 preflight：ADMIN_TOKEN、鉴权、goose 迁移版本、残留 Pod
- §1 创建：稳定 sandbox-id，`GetSandboxRuntime` 双向核对
- §2 Pause：API 预分配 → `PauseSandbox` → 幂等重试 → 删 Pod → build success/409
- §3 Resume：`last-snapshot` → 新 Pod（snapshot=true，同一 sandbox-id）
- §4 Checkpoint：`snapshot-templates` → `CheckpointSandbox` → resolve → 源 Pod 不重建
- §5 从 Checkpoint 创建新沙箱；§6 删除、幂等、404/409 断言

### 23_restart_multiplex_hide_direct.sh — 隐藏 direct sandbox

- 以 `-hide-sandbox-label flux-sandbox.io/direct=true` 重启
- 创建带 direct label 的 Pod；默认 `ListPodSandbox` 看不到，带 label 查询可见

### 24 / 25 / p01 — 性能脚本（不进入 run_all）

- `24_bulk_direct_concurrency.sh`：无预热池，并发创建 100/200 个 direct sandbox，统计耗时分布
- `25_bulk_direct_concurrency_pool.sh`：开启 `-cni-pool-enabled -cni-pool-size 200`，
  等待 `CNI pool warm: ready=N/M` 后执行同样批次
- `p01_bulk_cni_start_100.sh`：通过 kubelet 批量 apply（默认 95 个），统计 apply/Ready 总耗时

### 环境与模式切换脚本

- `configure_network_mode.sh non-cni|cni`：只切换 CNI 开关
- `switch_cni_pool.sh on [size]|off|status`：切换 CNI 预热池并保留既有参数

---

## 三、核心机制

### 3.1 Snapshot 错误自愈流程

**触发条件**：RunPodSandbox 返回包含 `snapshot/load` 的错误（典型为
`Put "http://localhost/snapshot/load": EOF`）。

**原因**：StopContainer / StopPodSandbox 后，E2B 后端的 snapshot 状态被破坏，下次启动沙箱会失败。

**修复流程**（`handle_snapshot_error` 函数）：

```
检测到 snapshot/load 错误
        │
        ▼
若 E2B_REBUILD_ON_SNAPSHOT=1：执行 build_prod.py（可选，默认不执行）
        │
        ▼
resolve_e2b_template --force
（从 E2B API /templates 重新解析 live template/build）
        │
        ▼
ensure_default_pod_json
（刷新 /tmp/e2b-pod.json 中的模板元数据）
        │
        ▼
返回 0，RunPodSandbox 重试
```

**重试策略**（`run_pod_sandbox` 函数）：
- 最多重试 5 次
- snapshot 修复只执行一次（`snapshot_fixed` 标志）
- 修复后若仍失败，等待 5 秒后重试
- `crictl runp` 使用 `-T 900s`，避免高并发 CNI ADD 下过早取消

> `test.py` 不再参与自愈流程；`build_prod.py` 仅在显式设置 `E2B_REBUILD_ON_SNAPSHOT=1`
> 时执行。

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
- 执行前确认不需要保留生产/systemd 实例

**systemd 接管**：`SOCKET` 命中 `/run/cri-multiplex.sock`（默认，跟随 kubelet）时，
`claim_systemd_socket` 会先停止 `cri-multiplex.service` 并写 marker，避免 `Restart=always`
干扰停止/重启/kill -9 用例；`run_all.sh` 通过 EXIT trap 自动恢复，也可手动执行
`01_start_multiplex.sh --restore`。

**环境约定**：验证节点专用于测试，kubelet 的 runtime endpoint 始终是
`/run/cri-multiplex.sock`。脚本只替换二进制（先把当前仓编译好，再停 systemd 实例，
在同一个 socket 上启动受控实例），不修改 kubelet 配置，也不重启 kubelet。

### 3.3 日志输出机制

所有日志函数输出到 **stderr**：
```bash
log_info()  { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
log_pass()  { echo -e "${GREEN}[PASS]${NC}  $*" >&2; ... }
```

**原因**：`run_all.sh` 用 `$(...)` 捕获子脚本输出解析计数，如果日志输出到 stdout 会被捕获导致解析错误。输出到 stderr 保证日志正常显示，stdout 只用于返回值。

`run_all.sh` 同时兼容 `PASS: 12`（`print_summary`）和 `PASS=12`（22 号自包含汇总）两种格式；
全量执行时按 01→非 CNI、07→CNI+Android、23→hide-direct 的顺序切换，`--only` 时按用例
预切换（18/19/20 在用例内部自行启动）。

### 3.4 E2B 特性适配

| 特性 | 处理方式 |
|------|----------|
| Container 启动后可能立即退出 | ContainerStatus 检查返回有效 state 即可，不强制 RUNNING |
| Container 不在 crictl ps 列表 | ListContainers 用 grpcurl 验证 + 容错处理 |
| crictl 截断 ID 到 12 位 | ListPodSandbox/ListContainers 用 `${ID:0:12}` 前缀匹配 |
| ExecSync 不严格传递非零退出码 | exitCode 检查字段存在即可 |
| 部分接口未实现 | Unimplemented 作为预期行为通过 |
| E2B 停止/删除破坏 snapshot | 刷新 live 模板元数据 + 可选重建 |
| CNI netns 短哈希命名 | 从日志/state 解析，不按 UID 猜测 |
| Android artifacts 缺失 | 自动降级 CNI-only / 用例 SKIP |
| 多端点/多 state-dir | `SOCKET` 跟随 kubelet endpoint；state-dir 固定独立 `/tmp/cri-multiplex-state` |
| direct sandbox 被 kubelet GC | `-hide-sandbox-label` 从 List 中剔除 |

### 3.5 CNI 与 Android 模式

- CNI 配置使用 `CNI_CONF_DIR`（默认 `/etc/cni/net-e2b.d`），不能用 Calico 的 `/etc/cni/net.d`
- E2B netns 形如 `e2b-<shortID>`（UID 派生短哈希，池化命中可能为 `e2b-pool*`），
  脚本从 `CNI ADD: ... netns=` 日志或 `state.json` 的 `cni_record` 解析真实路径
- Android artifacts 缺失时 `start_cni_android_multiplex` 降级为 CNI-only；Android-only 用例
  输出 SKIP 后以 0 退出
- CNI 预热池通过 `-cni-pool-enabled -cni-pool-size N` 提前创建 netns/veth/IPAM，
  日志 `CNI pool warm: ready=N/M` 表示预热进度

### 3.6 状态持久化与清理

- `${STATE_DIR}/state.json` 保存 `routes`、`e2b.pods`、`e2b.images`、`android.pods` 等
- 进程重启/kill -9 后恢复 Pod/Container 路由与 CNI 记录；非法 JSON 降级为空状态
- 孤儿对账由 `-orphan-reconcile-enabled/-interval/-grace-period` 控制；删除需幂等，
  并清理 CNI netns、HostPort iptables 规则和 Android workdir/PGID
- 无 owner 的相似命名资源不会被误删，输出 `skip unknown owner`

### 3.7 Pause / Checkpoint / Resume

- E2B API 负责预分配/回写快照元数据；Admin gRPC
  `unix:///run/cri-multiplex/admin.sock` 执行 `PauseSandbox` / `CheckpointSandbox` /
  `GetSandboxRuntime`
- 同一 sandbox 的 resume/checkpoint 新 Pod 必须复用同一个 envd token
- 写操作按 `operation_id` 幂等；Checkpoint 为原地快照，源 Pod UID 不变

### 3.8 隐藏 direct sandbox 与性能脚本

- `-hide-sandbox-label flux-sandbox.io/direct=true` 让 direct sandbox 不出现在
  `ListPodSandbox`/`ListContainers`，避免 kubelet PLEG/orphan GC 误清理
- 24/25 使用 `lib/bulk_direct.sh` 统计批量 `RunPodSandbox` 与 `cni_add_ms` 分布；
  25 在批次前等待预热池灌满；p01 通过 kubelet 批量 apply

---

## 四、使用方式

### 4.1 全量验证

测试节点专用于验证，kubelet 与 cri-multiplex 都使用 `/run/cri-multiplex.sock`。
脚本会先编译当前仓，再接管该 socket 上的 systemd 实例，**不改 kubelet 配置**。
以下操作需要 root：

```bash
cd cri-multiplex/e2b-verify
bash ./run_all.sh
```

`run_all.sh` 结束（含失败 / Ctrl-C）时自动把 systemd 实例拉回来；需要手动恢复时：

```bash
./01_start_multiplex.sh --restore
```

**输出示例**（数量随环境、Android/SKIP 变化）：
```
╔══════════════════════════════════════════════════════════════════╗
║                    全量验证最终汇总报告                          ║
╠══════════════════════════════════════════════════════════════════╣
║ [00]  验证工具安装与环境准备                    PASS              ║
║ [01]  启动 cri-multiplex                        PASS              ║
║ [02]  生命周期管理验证                          PASS(36/0/0)      ║
║ [03]  镜像管理验证                              PASS(8/0/0)       ║
║ [04]  Exec 能力验证                             PASS(11/0/0)      ║
║ [05]  ExecSync 能力验证                         PASS(5/0/0)       ║
║ [06]  Attach 能力验证                           PASS(3/0/0)       ║
║ [07]  kubelet 对接 Pod 保持 Running 验证        PASS(12/0/1)      ║
║ ...                                                              ║
║ [23]  重启 cri-multiplex 并隐藏 direct sandbox  PASS(5/0/0)       ║
╠══════════════════════════════════════════════════════════════════╣
║  PASS: <n>    FAIL: <n>   SKIP: <n>    TOTAL: <n>                ║
╚══════════════════════════════════════════════════════════════════╝
```

### 4.2 单独验证某个模块

```bash
# 只运行 02 生命周期验证（自动切非 CNI）
bash ./run_all.sh --only 02

# 只运行 16 沙箱出向隔离验证（自动切 CNI+Android）
bash ./run_all.sh --only 16

# 跳过环境准备（已安装过工具）
bash ./run_all.sh --skip-setup
```

`--only` 会按编号预切换模式；18/19/20 在用例内部自行启动，23 自己重启。

### 4.3 直接运行单个脚本

```bash
bash ./02_lifecycle.sh
bash ./07_kubelet_pod_running.sh
bash ./22_pause_checkpoint_resume.sh
```

直接运行时不会自动做 `run_all.sh` 的全局模式切换；多数 kubelet/CNI 用例会自行准备实例。

### 4.4 查看帮助

```bash
bash ./run_all.sh --help
```

### 4.5 切换运行模式

```bash
# 只切 CNI 开关
bash ./configure_network_mode.sh non-cni
bash ./configure_network_mode.sh cni

# 切 CNI 预热池（会强制重启并保留 hide-sandbox-label 等参数）
bash ./switch_cni_pool.sh on 200
bash ./switch_cni_pool.sh off
bash ./switch_cni_pool.sh status
```

Android 模式通过 `01_start_multiplex.sh` 的环境变量控制：
`ANDROID_ENABLED=1 E2B_FORCE_RESTART=1 ./01_start_multiplex.sh`。

### 4.6 性能脚本

```bash
bash ./24_bulk_direct_concurrency.sh
bash ./25_bulk_direct_concurrency_pool.sh
COUNT=20 bash ./p01_bulk_cni_start_100.sh
```

性能脚本默认不进入 `run_all.sh`；p01 需要当前实例处于 CNI 模式。

---

## 五、验证接口清单

### RuntimeService（runtime.v1.RuntimeService）

| 接口 | 验证模块 | 验证内容 |
|------|----------|----------|
| Version | 02 | RuntimeName=e2b-cri-shim |
| Status | 02 | RuntimeReady + NetworkReady |
| UpdateRuntimeConfig | 02 | 返回空 {} |
| RunPodSandbox | 02 / 07 / 21 | 直连创建、kubelet 创建、路由 |
| PodSandboxStatus | 02 / 11 / 12 / 13 / 17 / 21 | READY/NOTREADY、IP/CNI/Android 注解 |
| ListPodSandbox | 02 / 21 / 23 | 列表、多后端合并、hide direct |
| StopPodSandbox | 02 | 停止后 NOTREADY |
| RemovePodSandbox | 02 / 18 / 19 / 20 | 删除、幂等、状态清理 |
| CreateContainer | 02 / 07 / 21 | 返回 containerId、仅调用一次、路由 |
| StartContainer | 02 / 07 | 返回空 |
| ContainerStatus | 02 | 返回有效 state |
| ListContainers | 02 / 21 | 列表查询、多后端合并 |
| StopContainer | 02 | 返回空 |
| RemoveContainer | 02 / 07 | 返回空、kubelet 删除 Pod 时触发 |
| ExecSync | 05 / 09 | 直连 stdout/exitCode；kubelet probe/hook 触发 |
| Exec | 04 / 08 / 21 | 返回流式 URL；kubectl exec；路由 |
| Attach | 06 / 10 / 21 | 返回流式 URL + 连接验证；kubectl attach；路由 |
| UpdateContainerResources | 04 / 08 | Unimplemented（预期） |
| CheckpointContainer | 04 / 08 | Unimplemented（预期） |
| ContainerStats | 04 / 08 | 返回统计 |
| ListContainerStats | 04 / 08 | 返回统计列表 |
| ListMetricDescriptors | 04 / 08 | Unimplemented（预期） |
| ListPodSandboxStats | 04 / 08 | Unimplemented（预期） |
| ReopenContainerLog | 04 / 08 | 成功 |
| PortForward | 04 | Unimplemented（预期） |
| GetContainerEvents | 未覆盖 | 当前 multiplexer 不支持 |

### ImageService（runtime.v1.ImageService）

| 接口 | 验证模块 | 验证内容 |
|------|----------|----------|
| PullImage | 03 | 拉取镜像 |
| ImageStatus | 03 | 存在/不存在两种情况 |
| RemoveImage | 03 | 删除镜像 |
| ImageFsInfo | 03 | 文件系统信息 |

### kubelet 端到端链路（07–10 / 21 模块）

| 接口 | 验证模块 | 验证内容 |
|------|----------|----------|
| RunPodSandbox | 07 / 21 | kubelet 经 RuntimeClass=e2b 触发，沙箱创建一次 |
| CreateContainer | 07 / 21 | hash/restartCount 从 annotations 搬运到 labels，仅调用一次 |
| StartContainer | 07 | kubelet 启动容器 |
| ListContainers / ListPodSandbox | 07 / 21 | PLEG 周期查询，hash 匹配无重建、多后端合并 |
| ContainerStatus / PodSandboxStatus | 07 / 21 | 返回 Running，RESTARTS=0 |
| StopContainer / StopPodSandbox | 07 | 不被误调用（保持 Running） |
| RemoveContainer | 07 | Pod 删除时触发容器清理 |
| RemovePodSandbox | 18 / 19 / 20 | 删除后 state/routes 清理 |
| Exec / ExecSync / Attach | 08 / 09 / 10 | kubelet Exec、probe/hook ExecSync、kubectl attach |

### Admin gRPC（crimultiplex.admin.v1.E2BSandboxAdminService）

| 接口 | 验证模块 | 验证内容 |
|------|----------|----------|
| GetSandboxRuntime | 22 | cri/e2b sandbox id 双向索引、Running |
| PauseSandbox | 22 | 快照持久化、operation_id 幂等 |
| CheckpointSandbox | 22 | 原地快照、persisted + vm_resumed |

Admin socket：`unix:///run/cri-multiplex/admin.sock`（22 号固定使用）。

### E2B API internal 接口（22 号）

| 方法与路径 | 验证点 |
|------------|--------|
| `POST /internal/sandboxes/<id>/pause-snapshot` | 预分配 template/build |
| `GET /internal/sandboxes/<id>/snapshot-state` | paused / build 匹配 |
| `GET /internal/sandboxes/<id>/last-snapshot` | Resume 元数据、origin_node |
| `POST /internal/sandboxes/<id>/snapshot-templates` | Checkpoint 元数据、snapshot_id |
| `GET /internal/snapshots/<snapshot>/resolve` | template/build 解析 |
| `POST /internal/builds/<build>/status` | success 幂等、终态 409 |
| `DELETE /internal/snapshot-templates/<snapshot>` | 204 幂等 |
| `DELETE /internal/templates/<template>` | 有引用时 409 |
| `DELETE /internal/sandboxes/<id>/snapshot` | 204；重复删除当前实现 404 |

### CNI / Android 行为（11–17 / 21）

| 能力 | 验证模块 | 验证内容 |
|------|----------|----------|
| CNI PodIP | 11 / 14 / 17 | PodIP、eth0/tap0、envd health、外网 |
| Service / DNS | 14 | EndpointSlice、Service DNS、guest DNS |
| NetworkPolicy ingress | 15 | deny-all、allow selected、入向保留 |
| 沙箱出向隔离 | 16 | Pod CIDR deny/allowlist、sandbox 互访、allow-internet=false |
| Android RuntimeClass | 12 / 13 / 17 / 21 | 创建、多实例、ADB、guest 网络、路由 |

---

## 六、依赖文件说明

### 6.1 e2b-pod.json

Pod 沙箱配置文件，由 `00_setup.sh` / `write_default_pod_json` 按 live 模板动态生成，包含 E2B 必填的 annotations：
- `e2b.dev/template-id` / `e2b.dev/base_template_id` — 模板 ID
- `e2b.dev/team-id` — 团队 ID
- `e2b.dev/build-id` — 构建 ID（每次按 live 模板刷新）
- `e2b.dev/vcpu` / `e2b.dev/ram-mb` / `e2b.dev/total-disk-size-mb` — 资源规格
- `e2b.dev/envd-access-token` — 访问令牌
- `e2b.dev/execution-id` — 执行 ID
- `e2b.dev/allow-internet` / `e2b.dev/snapshot` / `e2b.dev/auto-resume` 等运行参数

### 6.2 build_prod.py

可选的模板重建脚本，调用 `e2b.Template.build()`：
```bash
python3 build_prod.py ubuntu:22.04-custom
```
- 镜像名 `:` 和 `.` 替换为 `-` 作为 alias
- Dockerfile: `FROM ${E2B_TEMPLATE_IMAGE_REGISTRY:-193.12.7.2:30443/e2b-orchestration}/{image_name}`
- 仅在 `E2B_REBUILD_ON_SNAPSHOT=1` 或 `refresh_build_id.sh` 的回退路径中使用

### 6.3 test.py

可选的 Sandbox 创建冒烟脚本，调用 `e2b.Sandbox.create()`：
```python
sbx = Sandbox.create("ubuntu-22-04-custom")
sbx.commands.run("whoami")  # 验证可用
sbx.kill()
```
- 读取 `.env` 中的 E2B_ACCESS_TOKEN / E2B_API_KEY
- 创建后立即 kill，仅用于手动排查；不再参与 snapshot 自愈流程

### 6.4 .env

E2B 环境变量配置：
```env
E2B_ACCESS_TOKEN="sk_e2b_..."
E2B_API_KEY="e2b_..."
E2B_DOMAIN="e2b.app"
E2B_API_URL="http://127.0.0.1:3000"
E2B_HTTP_SSL="false"
E2B_TEMPLATE_ALIAS="ubuntu-22-04-custom-1"
E2B_TEAM_ID="..."
```

> `/root/.e2b/config.json` 的 `teamApiKey` / `accessToken` 优先生效；不要把真实 key 提交到仓库。

### 6.5 test_stream_client.py

流式 URL 连接客户端，用于 Exec/Attach 交互验证：
- 解析 HTTP URL，建立 TCP 连接
- 按 `v5.channel.k8s.io` 协议发送 WebSocket Upgrade
- 从 stdin 读取输入，按 channel 0（stdin）发送
- 读取 channel 1/2（stdout/stderr）并实时输出

```bash
echo "echo hello" | python3 test_stream_client.py http://host:port/exec/xxx 5
```

### 6.6 lib/e2b_env.sh

E2B 凭证、模板元数据与 fixture 生成入口：
- `load_e2b_env` / `resolve_e2b_template` — 加载 `.env`、`config.json` 并解析 live 模板
- `write_default_pod_json` / `ensure_default_pod_json` — 生成/刷新直连 Pod JSON
- `write_e2b_pod_yaml` — 生成 kubelet Pod YAML
- `_new_execution_id` / `_new_envd_token` — 随机 execution-id 与 envd token
- `e2b_api` — 带 API Key 的 curl 封装（401/403 时重载 config.json 重试）

### 6.7 lib/refresh_build_id.sh

生成当前 live 模板的 kubelet Pod YAML：
```bash
bash e2b-verify/lib/refresh_build_id.sh [pod_name]
```
- 默认 `E2B_SKIP_BUILD=1`，直接解析模板
- 输出 `${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}`，stdout 打印 build_id
- 仅当 `E2B_SKIP_BUILD=0` 且 API 无模板时回退 `build_prod.py`

### 6.8 lib/cni_behavior_common.sh / lib/bulk_direct.sh

- `cni_behavior_common.sh`：Service/EndpointSlice/DNS/NetworkPolicy 公共断言与清理
- `bulk_direct.sh`：24/25 的批量 direct sandbox 创建、`.time` 耗时统计、`cni_add_ms`
  统计、预热池 `CNI pool warm` 等待

---

## 七、扩展指南

### 7.1 新增验证脚本

1. 创建 `NN_xxx.sh`，头部 source common.sh：
   ```bash
   #!/bin/bash
   set -euo pipefail
   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
   source "${SCRIPT_DIR}/lib/common.sh"
   log_section "NN — 新验证模块"
   # ... 用例 ...
   print_summary
   if [ "${FAIL_COUNT}" -eq 0 ]; then exit 0; fi
   exit 1
   ```

2. 在 `run_all.sh` 的 `SCRIPTS` 数组中添加：
   ```bash
   "NN|新验证模块|${SCRIPT_DIR}/NN_xxx.sh"
   ```

3. 如需特定运行模式，在 `run_all.sh` 的 `--only` / 全量切换逻辑中补充分支，
   或让脚本自己调用 `start_non_cni_multiplex` / `ensure_controlled_cni_multiplex` /
   `start_cni_android_multiplex`。

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
STATE_DIR=/tmp/custom-state bash ./run_all.sh
E2B_TEMPLATE_ALIAS=my-template bash ./run_all.sh
ANDROID_ARTIFACTS_DIR=/path/to/cf bash ./run_all.sh --only 12
OBSERVE_SECONDS=60 bash ./07_kubelet_pod_running.sh
```

---

## 八、故障排查

### 8.1 cri-multiplex 启动失败

```bash
# 查看日志
cat /tmp/cri-multiplex.log

# 手动启动排查（必须先停 systemd，否则两个实例会抢同一个 socket）
systemctl stop cri-multiplex.service
cd cri-multiplex
GOWORK=off go build -o cri-multiplex ./cmd/cri-multiplex
./cri-multiplex -socket /run/cri-multiplex.sock \
  -containerd-socket /run/containerd/containerd.sock \
  -orchestrator-address localhost:5008 \
  -state-dir /tmp/cri-multiplex-state
# 排查完恢复
./e2b-verify/01_start_multiplex.sh --restore
```

### 8.2 snapshot/load EOF 持续失败

```bash
# 重新解析 live 模板并刷新 fixture（推荐）
bash ./00_setup.sh

# 或手动刷新 kubelet YAML
bash ./lib/refresh_build_id.sh e2b-kubelet-test

# 仅在需要重建模板时
E2B_REBUILD_ON_SNAPSHOT=1 bash ./02_lifecycle.sh
```

排查 API：`curl -sS http://127.0.0.1:3000/health`，确认
`/root/.e2b/config.json` 的 `teamApiKey` 有效。

### 8.3 进程残留清理

```bash
# 确认不需要保留 systemd 实例后再执行
pkill -9 -f "cri-multiplex -socket"
rm -f /tmp/cri-multiplex.sock /run/cri-multiplex.sock
rm -rf /tmp/cri-multiplex-state
```

### 8.4 grpcurl proto 报错

```bash
# 重新下载 proto
rm -rf /tmp/cri-proto
bash ./00_setup.sh
```

### 8.5 kubelet endpoint / socket / state 不一致

```bash
# 确认 kubelet 的 endpoint 与脚本解析出的 socket 一致
grep -oP -- '--container-runtime-endpoint=\K\S+' /var/lib/kubelet/kubeadm-flags.env
ls -l /run/cri-multiplex.sock
# 恢复 systemd 实例
./01_start_multiplex.sh --restore
```

典型症状：kubelet 用例查到 systemd 实例的 Pod，日志却写在 `/tmp/cri-multiplex.log`
（说明接管没生效，检查 `01_start_multiplex.sh` 的 `ADOPT_SYSTEMD` 分支）；
或 18/19/20 看不到测试 Pod（`STATE_DIR` 被覆盖）。

### 8.6 Android / CNI 用例失败

```bash
ls "$ANDROID_ARTIFACTS_DIR/bin/launch_cvd"
ls -l /dev/kvm /dev/net/tun
HOME="$ANDROID_ARTIFACTS_DIR" "$ANDROID_ARTIFACTS_DIR/bin/cvd" stop

# CNI netns / 日志
ip netns list | grep e2b
grep -aoP 'CNI ADD: sandbox=.*?netns=\K[^ ]+' /tmp/cri-multiplex.log | tail
```

- Android artifacts 缺失时 12/13/17 及 20 的 Android 部分按 SKIP 处理
- CNI 用例确认 `CNI_CONF_DIR=/etc/cni/net-e2b.d`，不要误用 Calico 的 `/etc/cni/net.d`
- 14/15/16 需要 Service/EndpointSlice/Calico 或 bridge 数据面能力

### 8.7 16 号沙箱出向隔离失败

确认 orchestrator 已注入 `SANDBOX_DENIED_POD_CIDR=<podCIDR>`，并检查：

```bash
ip netns exec <netns> nft list set inet slot-firewall filtered_always_denylist
ip netns exec <netns> nft list set inet slot-firewall filtered_always_allowlist
```

未注入时 16 号的 3.2/3.3/3.4 会 SKIP；3.5 的 `allow-internet=false` 仍会断言，若部署版本
未包含用户级 deny 覆盖 TCP 的修复，该断言会失败。

# webhook 准入控制器验证套件

面向 `e2b-webhook`（Mutating Admission Webhook）的自动化验证脚本设计。6 个类别脚本 + 1 个入口，覆盖：环境准备、镜像替换/部署、准入契约、降级与安全、真实 K8S 集成、e2b 沙箱端到端、回滚清理。

设计目标只有三个：

1. **能独立跑**：一条命令从“没部署”跑到“完整沙箱可用”，不需要人工预置。
2. **失败能归因**：webhook 的问题报 webhook，CRI/orchestrator/API 的问题报 UNSUPPORTED，不混在一起。
3. **不绑后端**：e2b 和 android 的差异收在一张 `case` 表里，加后端只加分支，不动核心逻辑。

---

## 一、整体架构

### 1.1 目录结构

```
webhook-verify/
├── run_all.sh                  # 入口：00 → 01 → 02 → 03 → 04 → 05，汇总
├── 00_prepare.sh               # 环境检查 + 构建/部署/替换 + 基线
├── 01_admission_contract.sh    # /mutate 契约 + Pod/BatchSandbox 注入 + provider 选路
├── 02_degraded_security.sh     # API 异常降级 + TLS/日志/非法请求
├── 03_k8s_integration.sh       # 真实 apiserver + 并发/可用性
├── 04_e2b_e2e.sh               # e2b 沙箱端到端（发布 Pod → 沙箱 → 验证 → 删除）
├── 05_rollback_cleanup.sh      # 回滚旧镜像/副本、删测试资源、汇总
├── lib/
│   ├── common.sh               # 日志 / 计数 / 断言 / 退出码 / state.env
│   ├── k8s.sh                  # kubeconfig / 集群 / namespace / YAML 渲染 / 等待
│   ├── webhook.sh              # Deployment/Service/MWC/Secret + 构建/替换/回滚
│   ├── admission.sh            # AdmissionReview 构造 / 调用 / patch 解码与应用
│   ├── provider.sh             # provider case 表 + 选路断言
│   └── mock_api.py             # 可控 transform API（降级用例用，单文件）
├── e2b_sdk.py                  # E2B SDK 验证（probe/template；pause 已并入 probe）
├── fixtures/
│   ├── pod.yaml                # Pod 模板（占位符渲染）
│   └── batchsandbox.yaml       # BatchSandbox CR 模板
└── .env.example
```

共 17 个文件。没有 `providers/` 目录、没有 `mock/` 目录、没有每家一个 fixtures 子目录，原因见 1.5。

### 1.2 分层

| 层 | 脚本 | 依赖 | 失败语义 |
|----|------|------|----------|
| 准备层 | `00` | 仓库、Harbor、K8S | FAIL 即终止后续（`--skip-deploy` 可绕过） |
| 准入层 | `01`、`02` | 只需 webhook 可达（port-forward） | FAIL 归 webhook |
| 集成层 | `03` | 真实 apiserver/kubelet | FAIL 归 webhook，或 SKIP（能力不足） |
| 端到端 | `04` | CRI/orchestrator/API/envd | 依赖缺失 UNSUPPORTED，运行失败 FAIL |
| 收尾 | `05` | — | 只清理/恢复，不判定被测功能 |

`run_all.sh` 负责顺序、超时、聚合、异常兜底（EXIT/INT/TERM 时调 `05 --emergency`）。

### 1.3 关键配置

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `KUBECONFIG` | `~/.kube/config` | kubectl 上下文 |
| `WV_NAMESPACE` | `e2b` | webhook 部署 namespace |
| `WV_DEPLOYMENT` | `e2b-webhook` | Deployment 名（历史命名，可覆盖） |
| `WV_CONTAINER` | `e2b-webhook` | 容器名 |
| `WV_SERVICE` | `e2b-webhook` | Service（443→8443） |
| `WV_MWC` | `e2b-webhook` | MutatingWebhookConfiguration |
| `WV_TLS_SECRET` | `e2b-webhook-tls` | TLS Secret |
| `WV_APIKEY_SECRET` | `e2b-api-key` | provider API Key Secret |
| `WV_IMAGE_REPO` | `${REGISTRY_URL}/e2b-webhook` | 推送目标 |
| `WV_IMAGE_TAG` | `verify-$(date +%Y%m%d-%H%M%S)-$(git rev-parse --short HEAD)` | 唯一 tag，防 `IfNotPresent` 复用旧层 |
| `WV_TEST_NS` | `webhook-verify` | 测试 Pod/CR namespace |
| `WV_POD_LABEL` | `batch-sandbox.sandbox.opensandbox.io/pod-index` | Pod webhook 的 `objectSelector` 依赖 |
| `WV_MOCK_PORT` | `18080` | mock API 端口 |
| `WV_MOCK_HOST` | 空（取节点 InternalIP） | webhook 从 Pod 网络访问 mock 的地址 |
| `WV_PROBE_IMAGE` | `<registry>/busybox:latest` | 集群内探针镜像（mock 可达性、envd 健康） |
| `WV_E2E` | `1` | 是否跑 `04` |
| `WV_E2E_TIMEOUT` | `300` | Pod Ready + 沙箱操作超时（秒） |
| `WV_E2E_TEMPLATE` | `ubuntu-22-04-custom-1` | e2b 模板 alias |
| `WV_E2E_IMAGE` | `e2b.dev/<template-id>:<build-id>` | 自动由 `/templates` 解析 |
| `WV_SDK` | `1` | 是否跑 E8 SDK 用例（`--no-sdk` 置 0） |
| `WV_SDK_TIMEOUT` | `300` | SDK 命令超时 |
| `WV_SDK_API_URL` | 空 | 显式 API 地址；空则按 3.6 的顺序探测 |
| `WV_E2B_CONFIG` | `/root/.e2b/config.json` | SDK 凭证（`teamApiKey` 必填） |
| `WV_RESTORE_ON_EXIT` | `1` | 结束恢复旧镜像/副本 |
| `WV_KEEP` | `0` | 保留现场 |
| `WV_STRICT` | `0` | 已知差异从 SKIP 改为 FAIL |
| `WV_MODULE_TIMEOUT` | `3600` | 单个类别脚本超时（秒） |
| `UNSUPPORTED_EXIT_CODE` | `77` | 环境不具备能力 |

### 1.4 覆盖与不覆盖

| 范围 | 覆盖 | 不覆盖 |
|------|------|--------|
| HTTP 入口 | `/mutate` 方法/Content-Type/JSON 解码/patch 响应、`/healthz`、`:8443` TLS | 限流、鉴权、请求体上限（webhook 未实现） |
| 注入语义 | runtimeClass、注解、用户优先、幂等、JSON Pointer 转义、200KB 边界 | 各 CRD 控制器自身业务 |
| 选路 | 显式 runtimeClass、legacy 默认/信号、未知值放行 | provider 外部 API 内部实现 |
| 部署/升级 | 未部署时部署、已部署时替换、回滚、证书不被轮转 | K8S 集群安装（`deploy/k8s-deploy.sh`） |
| K8S 集成 | server dry-run、真实创建、并发、滚动期间可用性 | 沙箱底层 CRI/CNI 细节 |
| 完整沙箱 | Pod → webhook → CRI → 沙箱 → exec/文件/SDK → 删除 | Firecracker/orchestrator 内部 |
| 单元测试 | `go vet`/`go build`/`go test` | 覆盖率门禁 |


---

## 二、模块说明

### 00_prepare.sh — 环境检查、部署/替换、基线

**流程**

```
工具/集群检查 → 探测 webhook 现状 → 先落基线(旧镜像/副本/deploy.env/caBundle)
  → 构建镜像(唯一 tag) → 推送
  → 未部署? deploy.sh deploy-webhook : kubectl set image
  → 等 rollout(2 副本) → 校验 spec.image == 唯一 tag 且 imageID 已变化
  → 建测试 NS
```

| 编号 | 用例 | 断言 |
|------|------|------|
| P1 | 工具齐全 | `kubectl`、`curl`、`jq`、`python3`、`openssl` 可用 |
| P2 | 集群可达 | `cluster-info` 成功；至少 1 个节点 Ready |
| P3 | webhook 现状探测 | Deployment/Service/MWC/Secret/Endpoints 写入 `state.env`；已部署时还要求 Service/MWC/TLS Secret 齐全，缺任一判 FAIL |
| P4 | 构建镜像 | `./build.sh -i e2b-webhook` 成功；产物 digest 记录；构建前删旧 `bin/<arch>/e2b-webhook` |
| P5 | 推送镜像 | push `${WV_IMAGE_REPO}:${WV_IMAGE_TAG}` 成功 |
| P6 | 部署或替换 | 未部署走 `deploy.sh deploy-webhook`；已部署只 `set image` + `rollout status`（300s）；2 副本 Ready |
| P7 | 确实换了新镜像 | Pod `spec.image` 为唯一 tag，且 `imageID` 与基线不同（防 `IfNotPresent` 复用旧层） |
| P8 | 基线已保存 | 旧镜像/副本数/`deploy.env` 备份/caBundle 指纹/`/healthz` 200 全部落盘 |
| P9 | 单元测试（可跳） | `go vet` + `go build` + `go test ./...` 通过；`--skip-unit` 跳过 |

**关键约束**

- **健康实例只 `set image`**：不重跑完整 `deploy-webhook`。完整部署会删后重建 TLS Secret 与 caBundle，滚动窗口内会出现 caBundle 与证书不匹配的假失败（9.6）。
- **必须唯一 tag**：Deployment 是 `imagePullPolicy: IfNotPresent`，`:latest` 不会重拉。构建前删 `bin/<arch>/e2b-webhook`，否则 `build.sh` 会跳过编译。
- 已部署时替换：`kubectl set image deploy/$WV_DEPLOYMENT $WV_CONTAINER=$WV_IMAGE_REPO:$WV_IMAGE_TAG`。
- 未部署时：`ENABLE_WEBHOOK=true ./deploy.sh deploy-webhook`（负责证书、api-key Secret、MWC），之后同样 `set image` 到唯一 tag。
- 基线落盘 `${WV_TMP}/baseline/`：`old_image`、`old_replicas`、`deploy.env.bak`、`cabundle.sha256`。

---

### 01_admission_contract.sh — 准入契约

直接打 `/mutate`（port-forward 到 Service），必要时用真实 apiserver。**核心是：注入什么、什么时候不注入、用户已有值怎么办。**

**A. 入口（3）**

| 编号 | 用例 | 断言 |
|------|------|------|
| A1 | `/healthz` | GET/HEAD 200 |
| A2 | 正常 Pod | 200；`allowed=true`；patch 含 `/spec/runtimeClassName` + transform 映射出的全部注解（基线模板 22 个，含动态 `sandbox-id`），逐 key 与 transform 响应一致 |
| A3 | patch 语义 | 全部 `op=add`；无 `replace`/`remove`；path 正确 |

**B. Pod 注入（6）**

| 编号 | 用例 | 断言 |
|------|------|------|
| A4 | 用户已有注解 | 用户值不被覆盖；`runtimeClass=e2b` 且其余 provider 注解补齐（补齐数量不足 21 个判 FAIL） |
| A5 | 幂等 | 第一次 patch 结果作为 existing 再准入 → 无 patch |
| A6 | 已有 runtimeClass | 不再 patch runtimeClass；注解照常 |
| A7 | 缺 `TEMPLATE_NAME` | 仍补 runtimeClass，无 provider 注解（`Resolve` 失败降级） |
| A8 | `TEMPLATE_NAME` 为空串 | 同 A7 |
| A9 | 多容器 | 只读 `containers[0].env`；其它容器同名 env 不影响 |

**C. 选路（6）**

| 编号 | 用例 | 断言 |
|------|------|------|
| A10 | 显式 `e2b` | 注解注入 |
| A11 | 显式 `android` | 无 `e2b.dev/*` 注解；runtimeClass 保持 `android` |
| A12 | 无 runtimeClass + `ANDROID_SANDBOX=true` | runtimeClass=android |
| A13 | 无 runtimeClass 无信号 | runtimeClass=e2b（legacy 默认） |
| A14 | 未知 runtimeClass（如 `kata`） | allowed，无 patch |
| A15 | runtimeClass 大小写/空格 | 记录实际行为（9.3）；`--strict` 下 FAIL |

**D. 工作负载与选择器（4）**

| 编号 | 用例 | 断言 |
|------|------|------|
| A16 | BatchSandbox | 直连 `/mutate`，不依赖 CRD；patch 路径为 `spec.template.*`，注解与 transform 一致 |
| A17 | 无 label Pod | `objectSelector` 不命中，无 patch；同一轮先用带 label 的 Pod 做正向对照，对照不命中则三条选择器用例都判 FAIL |
| A18 | 部署 namespace 内带 label | `namespaceSelector` 排除，无 patch（同上正向对照） |
| A19 | `kube-system` 内带 label | 同上，无 patch（同上正向对照） |

**E. 边界（2）**

| 编号 | 用例 | 断言 |
|------|------|------|
| A21 | 注解 key 含 `/`、`~` | JSON Pointer 转义为 `~1`、`~0`，patch 能被 apiserver 接受 |
| A23 | UPDATE 操作 | 直连 `/mutate` 发 `operation=UPDATE` 的 Pod（无 runtimeClass、无注解）：allowed 且 0 patch |

> 注解来源见 `providers/transform.go:toE2BAnnotations`：15 个稳定键 + `allow-internet`/`envd-access-token`/`network`/`env-vars`/`volume-mounts`/`auto-resume`（共 21 个），`metadata`/`sandbox-id` 为空时不设置。`sandbox-id`、`execution-id` 是动态值，**不跨调用比较**。

---

### 02_degraded_security.sh — 降级与安全

做法：整轮只做一次 `kubectl set env deploy/$WV_DEPLOYMENT E2B_API_URL=http://<nodeIP>:$WV_MOCK_PORT`（切到 mock 并等 rollout），之后每条用例通过 mock 控制面（`WV_MOCK_PORT+1`，`POST /mode`）在运行时切换行为，不再逐条滚动 Deployment；模块结束统一恢复一次。

> 早期实现是“每条用例切一次 env、跑完再切回”，D1–D6 共 12 次滚动，02 需要 3m29s；改成运行时切行为后只需 2 次滚动，02 降到约 42s。A22 已移至 02 的 mock 阶段，01 不再为该边界用例单独切换/恢复 mock。2026-11-06 的真实性审计又收紧了断言：每条降级用例都要用 mock 的 `/last-response` 证明故障响应真的发出，D6 要证明请求计数递增且**所有副本**日志都无密钥。

| 编号 | 用例 | 方法 | 断言 |
|------|------|------|------|
| D1 | API 500 | mock 模式 `status:500` | mock 确认返回 500；allowed；只补 runtimeClass；无 5xx |
| D2 | API 超时 | mock 模式 `delay:7`（`E2B_API_TIMEOUT` 代码默认 5s、helm 部署为 4s） | mock 确认延迟 7s 已生效；超时值 + 3s 内返回；只补 runtimeClass |
| D3 | 连接拒绝 | mock 模式 `refuse`：关闭数据面监听（同一 URL） | 本机确认端口已关闭；allowed；只补 runtimeClass |
| D4 | 非法 JSON | mock 模式 `body:not-json` | mock 确认返回 8B 非 JSON；allowed；只补 runtimeClass |
| D5 | 超大响应 | mock 模式 `size:1048576` | mock 确认返回 ≥1MB；不崩、不被 OOM；allowed 且只补 runtimeClass |
| D6 | API Key 透传 | mock 模式 `normal` + `/last-headers` + `/count` | transform 计数递增；请求带 `X-API-Key: <key>`；**所有副本**日志都不打印 key |
| A22 | 合并后超 200KB（从 01 移入） | mock 模式 `body_file`：210KB body | mock 实际返回 >200KB（`/last-response` 校验）；只保留 runtimeClassName（patch=1、provider 注解=0），不返回 5xx |
| D7 | TLS 与 MWC | Secret `tls.crt`、port-forward 真实 TLS 握手得到的服务端证书、MWC caBundle 三者比对；明文请求 | 三者指纹一致；明文不返回 200；`failurePolicy`/`timeout`/`admissionReviewVersions` 与设计一致 |
| D8 | 非法请求 | 非 JSON body、GET `/mutate`、未知路径 | 400/405/404，不 5xx |
| D9 | 证书 SAN/有效期/链 + TLS 版本 | Secret 证书、MWC caBundle、port-forward 真实握手 | SAN 含 `svc.ns.svc`；7 天内不过期；caBundle 可验证链；TLS1.2 成功且证书与 Secret 一致；TLS1.1 被拒 |
| D10 | MWC 静态契约 | 两个 webhook 的 rules/sideEffects/matchPolicy/clientConfig | operations 均为 `["CREATE"]`，resources/scope 精确，sideEffects=None，matchPolicy=Equivalent，service 指向 `svc:443/mutate` |

**恢复**：模块结束（D6/A22 之后）统一还原 `E2B_API_URL`/`E2B_API_TIMEOUT` 并等一次 rollout；异常退出时 `trap` 也会还原。

---

### 03_k8s_integration.sh — 真实 apiserver + 可用性

| 编号 | 用例 | 方法 | 断言 |
|------|------|------|------|
| K1 | server dry-run | `kubectl create --dry-run=server -f fixtures/pod.yaml` | 返回对象里已有注解/runtimeClass；未持久化 |
| K2 | 真实创建 | `kubectl apply` 后等待 Ready | 持久化后注解仍在；失败记 SKIP 并附诊断（沙箱问题不在本模块判 FAIL） |
| K3 | 已有用户注解 + 真实创建 | 同上 | 用户值保留；runtimeClass 与其余 21+ 个 provider 注解仍注入 |
| K4 | BatchSandbox dry-run | CRD 存在时 | 路径正确；CRD 缺失 UNSUPPORTED |
| K5 | 50 并发 dry-run | 50 个并发 `--dry-run=server` | 成功率 100%；无 5xx；p95 < 5s |
| K6 | 删一个 webhook Pod | `delete pod` | 仍有 Endpoint；准入不中断；副本自动补齐 |
| K7 | 滚动重启期间准入 | `rollout restart` 同时打 dry-run | 成功率 100%；结束后 2 副本 Ready |

---

### 04_e2b_e2e.sh — e2b 沙箱端到端

**链路**

```
kubectl apply fixtures/pod.yaml（不写 annotation/runtimeClass）
  → apiserver → MWC → webhook: TEMPLATE_NAME → transform → 注解 + runtimeClassName
  → Pod 持久化 → kubelet → cri-multiplex → orchestrator → Firecracker/envd
  → Pod Ready → kubectl exec / envd / E2B SDK
  → 幂等更新 → 删除 → 沙箱与资源清理
```

**前置**（缺任一项 → UNSUPPORTED，不报 FAIL）

| 依赖 | 检查 |
|------|------|
| RuntimeClass `e2b` | `kubectl get runtimeclass e2b` |
| cri-multiplex | `$WV_CRI_SOCKET`（默认 `/run/cri-multiplex.sock`）可连通 |
| orchestrator | `localhost:5008` 可连通 |
| E2B API | `/health` 200、`/templates` 能按 alias 解析出 `templateID/buildID` |
| E2B SDK（可选） | `python3 -c "import e2b"` + `teamApiKey`；缺失只 SKIP E8 |

**fixture**（`fixtures/pod.yaml` 渲染后）

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: webhook-e2e-sandbox
  namespace: webhook-verify
  labels:
    batch-sandbox.sandbox.opensandbox.io/pod-index: "0"
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: e2b.dev/<template-id>:<build-id>
      imagePullPolicy: IfNotPresent
      command: ["sleep", "3600"]
      env:
        - name: TEMPLATE_NAME
          value: "<template-alias>"
```

不写 `annotations`/`runtimeClassName` —— 让 webhook 注入，这才是端到端。固定 `metadata.name` 便于 `kubectl apply` 幂等；真实 spotbox 用 `generateName`，准入逻辑相同（`generateName` 对象的注入由 A2 直连用例覆盖）。

**用例**

| 编号 | 用例 | 断言 |
|------|------|------|
| E1 | webhook 注入 | 持久化对象含 `runtimeClassName=e2b` 与 transform 映射出的全部注解（基线模板 22 个）；用户 env/image 未变 |
| E2 | 调度 | `PodScheduled=True`；`spec.nodeName` 非空 |
| E3 | 运行 | Pod Ready=True（记录 `restartCount`） |
| E4 | 沙箱存在 | `e2b.dev/sandbox-id` 非空 |
| E5 | envd 健康 | `PodIP:49983/health` 返回 200/204（envd 的 GET /health 是 204），或经集群内探针可达 |
| E6 | kubectl exec | `echo e2e_ok` 退出码 0、输出正确 |
| E7 | 文件读写 | `/tmp/e2e` 写入读出一致 |
| E8 | SDK 基线（不依赖 Pod） | `Sandbox.create(alias)` 拿到 id；`commands.run("whoami")` rc=0；文件读写一致；`pause()` 后 state=paused；`connect()` 自动 resume 且命令可执行；`kill()` True |
| E10 | 删除 | Pod 删除成功；删除前记录该 Pod 的 CRI sandbox id，删除后该记录不再是 `SANDBOX_READY`（允许 GC 前的 NOTREADY 墓碑）；拿不到 id 判 FAIL |
| E11 | 资源清理 | CRI 中该 Pod UID 无 `SANDBOX_READY` 记录；删除前捕获的 Firecracker 进程 PID 在删除后已退出。netns 是 orchestrator 预创建的 slot 池，不作为 per-sandbox 残留判据 |
| E12 | BatchSandbox 路径（可选） | `--e2e-batchsandbox`；CR 创建的 Pod 继承注解且可执行 |
| E14 | 失败归因 | 构造无效 template/停掉依赖时，错误落在正确层（webhook/CRI/API），不误报 |

**失败规则**

| 情况 | 结果 |
|------|------|
| RuntimeClass/CRI/orchestrator/API 缺失 | UNSUPPORTED（整模块） |
| Pod 创建成功但 Pending/Ready 超时 | FAIL（附 describe/events/CRI/orchestrator 日志） |
| 注解缺失/错误 | FAIL（归准入） |
| exec/envd 失败 | FAIL（沙箱运行） |
| SDK 缺失/凭证缺失 | E8 SKIP（E1–E7 照常） |
| E8 失败但 Pod 侧全 PASS | FAIL 归 SDK/控制面，注明与本 webhook 无关 |

**为什么用 SDK 自建沙箱做 E8**：Pod 通道的沙箱不会在 API 落库（`POST /sandboxes/transform` 只返回创建参数），SDK 侧永远看不到它；因此跨通道用例（原 E8b/E8c）和 Pod 沙箱 pause（原 E13）已移除。E8 改为在 SDK 自建沙箱上覆盖 create→exec→文件→pause→resume→kill，用“E8 PASS 而 E1–E7 FAIL”来区分控制面问题与 K8s→CRI→orchestrator 链路问题。

**为什么移除 E9**：MWC 的 `rules[].operations` 只有 `CREATE`，`kubectl annotate` 触发的 UPDATE 根本不会进入 webhook；原判据（UID/sandbox-id 不变）在 webhook 被删除时也会通过，属于构造的成功。UPDATE 不注入改由 A23 直连 `/mutate` 验证：同一对象 CREATE 会产生 patch，UPDATE 必须 0 patch。

---

### 05_rollback_cleanup.sh — 回滚与清理

| 编号 | 动作 | 断言 |
|------|------|------|
| Z1 | 恢复旧镜像 | `set image` 回 `${OLD_IMAGE}`；所有 Pod 的 `spec.image` 都等于旧镜像且 ready ≥ 1 |
| Z2 | 恢复副本数 | `spec.replicas`、Ready、Endpoints 三项都恢复到 `${OLD_REPLICAS}` |
| Z3 | 恢复 `.env` | 从 `baseline/deploy.env.bak` 还原；diff 为空；运行期 `E2B_API_URL` 与基线一致 |
| Z4 | 删测试资源 | Pod/CR/namespace |
| Z5 | 清理辅助进程 | 先记录 pid 再 stop；校验进程已退出且 `${WV_PORT}`/`${WV_MOCK_PORT}` 不再 LISTEN；`--cleanup-fresh` 删除本轮新部署 |
| Z6 | 最终一致性 | Secret 证书、port-forward 真实 TLS 握手得到的服务端证书、caBundle 三者一致；`/healthz` 200；2 副本 Ready |

> Z7（“汇总”伪用例）已移除：它只是把计数再打印一遍并记 PASS，不产生任何功能反馈；汇总由 `finish`/`run_all` 输出。

---

## 三、核心机制

### 3.1 环境状态机

```
探测 → [未部署] → deploy.sh deploy-webhook → set image(唯一 tag) → 验证
     → [已部署] → set image(唯一 tag) → rollout → 验证
     → 基线落盘 → 04 前再确认 /healthz
```

`--skip-deploy` 时只做探测与验证；`--check-only` 只做 P1/P2/P3/P8。

### 3.2 镜像替换

- tag 唯一：`verify-<时间戳>-<commit>`；避免 `IfNotPresent` + `:latest` 的“看起来成功其实没换”。
- 构建前删 `bin/<arch>/e2b-webhook`（`build.sh` 里 `ensure_webhook_binary` 有则跳过）。
- 换完校验 `imageID` 含新 tag；旧镜像记入基线供 Z1 回滚。

### 3.3 AdmissionReview 直连

`lib/admission.sh`：

- `build_review <kind> <ns> <name> <json-pod>` 生成 v1 AdmissionReview；
- `post_mutate` 走 `kubectl port-forward svc/$WV_SERVICE $WV_PORT:443`，`curl -k --cacert` 或 `-k`；
- `decode_patch` 用 `base64 -d | jq` 转成可读 JSON，`apply_patch` 用 `jq` 把 `add` 应用到对象上，供 A4/A5 复用。

### 3.4 provider case 表

`lib/provider.sh` 里唯一出现后端差异的地方：

```bash
# name|runtimeClass|annotation_prefix|requires_api|e2e
PROVIDER_TABLE='
e2b|e2b|e2b.dev/|1|1
android|android||0|0
'
```

| 函数 | 作用 |
|------|------|
| `provider_list` | 遍历启用后端 |
| `provider_by_runtimeclass <rc>` | 显式选路；未命中 → 放行 |
| `provider_default` | 无 runtimeClass 时：`ANDROID_SANDBOX=true` → android，否则 e2b |
| `provider_field <name> <field>` | 取字段（注解前缀、是否调 API、是否有 E2E） |

核心脚本只出现 `provider_*` 调用，不出现 `e2b.dev/`、`ANDROID_SANDBOX`、`E2B_API_URL`。

> 例外：`00` 的部署命令、`02` 里把 API 指向 mock 的 `E2B_API_URL`、`04` 的 `e2b_*` 函数，属于“当前实例/当前后端”的具体实现，集中在一处并注明。

### 3.5 patch 语义

- 只 `add`：用户已有的 key 不覆盖，不产生 `replace`。
- 幂等：把第一次注入结果当 existing 再准入，应无 patch（A5）。
- 只处理 CREATE：UPDATE AdmissionReview 直接放行、0 patch（A23）；MWC 的 rules 也只含 CREATE。
- 200KB：合并后超限时丢弃 provider 注解，绝不返回 5xx（A22）。
- JSON Pointer：`/`→`~1`，`~`→`~0`（A21）。
- 空值：`interface{}` + `omitempty` 对空字符串仍输出 `value: ""`（映射层把零值当有效值，见 9.1）。

### 3.6 e2b 沙箱端到端与 SDK

E2E 断言分层：

| 层 | 断言 | 归因 |
|----|------|------|
| 准入 | 注解/runtimeClass | webhook |
| 调度 | PodScheduled/NodeName | scheduler/kubelet |
| 运行 | Pod Ready | CRI/orchestrator/沙箱 |
| 数据面 | envd/exec/文件/SDK | 沙箱/envd/网络 |
| 清理 | Pod/sandbox/netns | CRI/控制器 |

SDK 侧（`e2b_sdk.py`，写法参考 `deploy/create_template.py` 的 `setup_e2b_env()` 与 `deploy/create_sandbox.py`）：

```python
os.environ["E2B_API_URL"] = resolve_api_url()   # 见下方顺序
os.environ["E2B_HTTP_SSL"] = "false"
os.environ["E2B_DOMAIN"] = "e2b.app"
key = os.environ.get("E2B_API_KEY") or cfg.get("teamApiKey")   # 必填
if token := (os.environ.get("E2B_ACCESS_TOKEN") or cfg.get("accessToken")):
    os.environ["E2B_ACCESS_TOKEN"] = token                     # 空 token 不设置
```

API URL 解析顺序（实测本机 `10.10.10.10:3000` 不可达，ClusterIP/`localhost:3000` 可达，所以不能硬编码参考脚本的默认 IP）：

1. `WV_SDK_API_URL` → 2. `E2B_API_URL` → 3. `kubectl get svc -n $WV_NAMESPACE api` 的 ClusterIP → 4. `http://localhost:3000`；每个候选先 `curl -sf /health`，全失败 → E8 整体 SKIP。

`e2b_sdk.py` 五个子命令，stdout 只输出 `K=V`，日志走 stderr：

| 子命令 | 调用链 | 用途 |
|--------|--------|------|
| `probe` | `create` → `commands.run("whoami")` → 写/读文件 → `pause()` → `get_info().state=paused` → `connect()`（自动 resume）→ 命令 → `kill` | E8 |
| `template` | `Template.build(..., skip_cache=True)` | 现场建模板（默认不跑） |

退出码 `0`/`1`/`77`（77=SKIP），bash 侧 `eval` 采集。

### 3.7 日志与计数

- 日志写 stderr；`print_summary` 输出 `PASS/FAIL/SKIP/UNSUPPORTED`，`run_all` 解析 `PASS: n`。
- 计数与耗时按模块聚合；失败用例单独列出并指向 `logs/`、`artifacts/`。
- 全 PASS → 0；有 FAIL → 1；只有 UNSUPPORTED → 77。

---

## 四、使用方式

### 4.1 前置

- K8S 就绪，`e2b` namespace 存在（或允许 `00` 创建）。
- 能推送镜像到 Harbor（`REGISTRY_URL` 可从 `deploy/.env` 解析）。
- `04` 额外需要 cri-multiplex/orchestrator/E2B API/envd；`E8` 额外需要 `python3`+`e2b`+`/root/.e2b/config.json`。

### 4.2 全量

```bash
cd e2b-webhook/webhook-verify
bash ./run_all.sh
```

### 4.3 只检查 / 只部署 / 跳过重链路

```bash
bash ./run_all.sh --check-only    # 只探测，不改集群
bash ./run_all.sh --deploy-only   # 只做 00 的部署/替换
bash ./run_all.sh --skip-deploy   # 已部署，只验证
bash ./run_all.sh --skip-e2e      # 不跑 04
bash ./run_all.sh --no-sdk        # 04 里跳过 E8
```

### 4.4 只跑某类

```bash
bash ./run_all.sh --only contract     # 01
bash ./run_all.sh --only e2e          # 04
bash ./run_all.sh --only 01,02        # 按编号
```

类别名：`prepare`、`contract`、`degraded`、`k8s`、`e2e`、`cleanup`。

### 4.5 参数

| 参数 | 说明 |
|------|------|
| `--check-only` / `--deploy-only` / `--skip-deploy` | 见上 |
| `--skip-e2e` / `--no-sdk` | 跳 `04` / 跳 E8 |
| `--only <类/编号>` | 只跑指定类别 |
| `--e2e-batchsandbox` | 启用 E12；`--e2e-advanced` 仅为兼容旧参数保留（E13 已并入 E8） |
| `--mock` | 只用 mock API 跑降级/边界（隐含 `--skip-e2e`、`--no-sdk`） |
| `--strict` | 已知差异 SKIP → FAIL |
| `--keep` | 保留资源与镜像 |
| `--cleanup-fresh` | 结束删除本轮新部署 |
| `--skip-unit` | 跳过单元测试 |
| `--help` | 用法 |

### 4.6 输出示例

```
╔══════════════════════════════════════════════════════════╗
║              webhook 验证汇总（66 条用例）               ║
╠══════════════════════════════════════════════════════════╣
║ [00] 环境/部署/基线                          PASS(9/0/0) ║
║ [01] 准入契约                               PASS(21/0/0) ║
║ [02] 降级/安全                               PASS(11/0/0) ║
║ [03] K8S 集成/并发                PASS(6/0/0) UNSUP(1)   ║
║ [04] e2b 沙箱端到端              PASS(11/0/0) UNSUP(1)   ║
║ [05] 回滚/清理                               PASS(6/0/0) ║
╠══════════════════════════════════════════════════════════╣
║ PASS: 64   FAIL: 0   SKIP: 0   UNSUP: 2   TOTAL: 66      ║
╚══════════════════════════════════════════════════════════╝
```

---

## 五、用例清单

| 模块 | 编号 | 条数 | 主要依赖 | 典型失败归因 |
|------|------|------|----------|--------------|
| 00 | P1–P9 | 9 | K8S/Harbor/构建工具 | 环境或镜像 |
| 01 | A1–A23（A20 已移除、A22 已移入 02） | 21 | webhook 可达 | webhook 契约 |
| 02 | D1–D10 + A22 | 11 | webhook + mock | 降级/TLS/证书 |
| 03 | K1–K7 | 7 | apiserver/kubelet | 准入/可用性 |
| 04 | E1–E14（E8b/E8c/E13/E9 已移除） | 12 | CRI/orchestrator/API/envd | 沙箱链路 |
| 05 | Z1–Z6（Z7 已移除） | 6 | — | 清理/恢复 |
| **合计** | | **66** | | |

已知差异用例：无（原 A20 已移除；`--strict` 保留为通用开关，当前无此类用例）。
环境不具备能力（UNSUPPORTED，退出码 77）：K4/E12（BatchSandbox CRD 缺失；A16 直连 `/mutate`，不需要 CRD）。

已移除用例及原因：E8b（SDK 连接 Pod 沙箱）、E8c（exec 写 / SDK 读）、E13（Pod 沙箱 pause）——`POST /sandboxes/transform` 只返回创建参数、不在 API 落库，Pod 通道创建的沙箱对 SDK 永远不可见；pause/resume 能力已并入 E8（SDK 自建沙箱，本环境实测可通过）。E9（`kubectl annotate` UPDATE 幂等）——MWC 只拦 CREATE，UPDATE 不进 webhook，判据在 webhook 被删除时也会通过，改由 A23 直连验证。Z7（汇总）——只打印计数，不是功能用例。

---

## 六、依赖文件

| 文件 | 说明 |
|------|------|
| `lib/common.sh` | `log_*`、`pass/fail/skip/unsupported`、`state_set`、`run_all` 用的计数与退出码 |
| `lib/k8s.sh` | 集群/节点/命名空间探测；`render`（占位符填充 fixtures）；`wait_pod_ready` |
| `lib/webhook.sh` | Deployment/Service/MWC/Secret 状态；`build_image`/`push`/`set_image`/`rollout`/`restore` |
| `lib/admission.sh` | AdmissionReview 构造、`post_mutate`、patch 解码与应用 |
| `lib/provider.sh` | provider case 表与选路断言 |
| `lib/mock_api.py` | 单文件 mock：数据面 `POST /sandboxes/transform` + `/healthz`、`/count`、`/last-headers`、`/last-body`；控制面 `--control-port`（默认 `WV_MOCK_PORT+1`）的 `POST/GET /mode` 可运行时切 `normal/status/body/body_file/delay/size/refuse` |
| `e2b_sdk.py` | `probe`/`connect`/`cross`/`pause`/`template` 五个子命令；无第三方依赖除 `e2b` |
| `fixtures/pod.yaml` | Pod 模板：`__NAME__`、`__NS__`、`__LABELS__`、`__ANNOTATIONS__`、`__RUNTIMECLASS__`、`__IMAGE__`、`__COMMAND__`、`__ENV__` |
| `fixtures/batchsandbox.yaml` | CR 模板，同套占位符 |
| `state.env` | 运行期状态：`IMAGE_REF`/`IMAGE_TAG`、`TEMPLATE_ID`/`BUILD_ID`、`FRESH_DEPLOY`、`BASELINE_*`、`STOCK_API_URL`；不写凭证 |
| `logs/` `artifacts/` | 运行期生成：模块日志、失败 Pod describe/events/YAML、webhook/CRI/orchestrator 日志 |

`mock_api.py` 端点：数据面 `POST /sandboxes/transform`（按当前 mode 返回）＋ `GET /healthz` / `/count` / `/last-headers` / `/last-body`；控制面 `POST /mode` / `GET /mode`（只监听本机，测试脚本用）。`refuse` 模式会关闭再重开数据面监听，用来制造真实的 connection refused，且不需要改 webhook 的 env。

mock 与 webhook 不在同一网络命名空间：mock 监听宿主机 `0.0.0.0`，`WV_MOCK_HOST` 默认取节点 InternalIP，并用一次性 busybox Pod 做一次真实可达性探测；不可达时相关用例记 UNSUPPORTED，而不是误判成降级逻辑失败。

---

## 七、扩展指南

### 7.1 新增 provider（如 kata）

1. 前提：webhook 代码里已实现并注册该 Provider。
2. `lib/provider.sh` 的 `PROVIDER_TABLE` 加一行：`kata|kata||0|0`。
3. 若它调 API：加 `requires_api=1`，并在 `02` 的 mock 场景里复用（无需新代码）。
4. 若要做完整 E2E：`04` 里加 `kata_*` 函数并在分发处加一个 `case` 分支；不需要新目录。
5. 若它没有注解：A11 那类“无注解 provider”断言自动覆盖。

### 7.2 新增用例

在对应类别脚本里加函数与编号（`A24`/`D9`/…），用 `pass/fail/skip`，失败时落 artifacts。不要在 `01` 里加“通用 HTTP 服务测试”。

### 7.3 新增工作负载

1. `fixtures/` 加模板；2. `lib/k8s.sh` 加渲染分支；3. `01` 加 patch 路径断言；4. `05` 清理。

### 7.4 CI

- PR：`go test ./...` + `--check-only`；
- 日常：`--skip-e2e`；
- 发布前/夜间：全量（含 04）。

---

## 八、故障排查

| 现象 | 排查 |
|------|------|
| 集群不可达 | `kubectl cluster-info`、`get nodes`、`config current-context` |
| webhook 没起来 | `get deploy/svc/endpoints/pods`、events、Harbor 拉取、nodeAffinity、证书 |
| 镜像没换 | Pod `imageID` 是否含新 tag；`IfNotPresent`；旧 `bin/` 未删；架构不匹配 |
| TLS 报错 | Secret `tls.crt` 与 MWC `caBundle` 指纹；SAN/过期 |
| 注解缺失 | label、namespace、runtimeClass、`containers[0]`、用户已有值、API 失败、200KB |
| API 异常 | `/health`、`/templates`、transform；401/403/404/500 |
| Pod Pending | nodeAffinity、资源、镜像拉取、`describe pod` |
| Ready 超时 | `runtimeClassName`、`sandbox-id` 注解、`journalctl -u kubelet`、cri-multiplex/orchestrator 日志、模板 build 过期、CNI/IPAM |
| exec/envd 失败 | PodIP 可达、49983、NetworkPolicy、envd token |
| SDK 失败 | `ModuleNotFoundError` → 未装；`AuthenticationException` → key；连接超时 → 按 3.6 试 ClusterIP；`TemplateException` → alias；`SandboxNotFoundException` → 已回收 |
| 删除残留 | finalizer、CRI Stop/Remove、orchestrator Delete、netns/IPAM |

---

## 九、边界与已知问题

### 9.1 空 `sandbox` 响应注入空值注解（`[e2b]`，用例已移除）

API 返回 200 `{}` 时，`sandboxConfig` 为零值，仍注入 15 个空字符串 + 6 个默认值，共 21 个注解（`ram-mb=0`、`vcpu=0`、空模板 ID 等）。根因：`transform.go` 的 `callTransformAPI` 无条件返回 `&apiResp.Sandbox`，`{}`/`null`/缺 key 都变成“非 nil 的零值结构”，`Resolve` 的 `cfg == nil` 判空永远不命中。理想行为是“无注解/仅 runtimeClass”。

原 A20 覆盖该场景，因属产品缺陷且暂不修复，已按评审从套件删除（不保留固定 SKIP）；缺陷本身仍存在于业务代码，修复后建议恢复用例。

### 9.2 Content-Type 严格相等（通用）

`application/json; charset=utf-8` 会被拒（415）。A2 只发精确 `application/json`；带 charset 的行为记录在 artifacts。

### 9.3 runtimeClass 大小写/空格（通用）

`E2B` 视为未注册 → 放行；` e2b ` 可能命中但 `runtimeClassSet=true` → 不 patch。A15 记录实际结果。

### 9.4 镜像 tag（通用）

`IfNotPresent` + `:latest` 不会重拉，这是“代码更新了但没生效”的最常见原因；P4/P7 强制唯一 tag。

### 9.5 完整部署会轮转证书（通用）

`deploy-webhook` 会删后重建 TLS Secret 并重写 caBundle，滚动窗口内存在不匹配窗口。健康实例只 `set image`；`00` 只在“未部署”时走完整部署。

### 9.6 SDK 只能验证“SDK 可见的沙箱”（`[e2b]`）

cri-multiplex 直连 orchestrator 创建的沙箱不会出现在 `/sandboxes` 或 `get_info` 里（transform 端点只返回创建参数、不落库）。因此不设计“SDK 连接 Pod 沙箱”的用例：本环境永远不可达，只会产生固定 SKIP；跨通道一致性目前在 E1–E7（exec/envd）侧覆盖。另外 `Sandbox.create` 亚秒返回，**create 返回不等于就绪**，判据放在首条命令的退出码。

### 9.7 netns 是预创建的 slot 池（通用）

orchestrator 在启动时就创建了 `ns-0…ns-N` 的 slot 池（本环境 400+ 个），sandbox 复用其中的 slot，删除 sandbox 后 netns/veth 仍保留在池里。因此 **不能用“netns 数量/新增”判断 per-sandbox 残留**；E11 改为检查该 Pod UID 的 CRI 记录不再是 `SANDBOX_READY`，以及删除前捕获的 Firecracker 进程（cmdline 含 `e2b.dev/sandbox-id`）已退出。

### 9.8 不覆盖

限流、鉴权、请求体上限未实现，不测；Firecracker/orchestrator 内部、CRD 控制器全链路交给各自套件。

---

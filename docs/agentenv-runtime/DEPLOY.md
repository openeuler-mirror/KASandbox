# 安装部署与日常使用

## 1. 前置条件和制品

目标是已有健康原生 v0.2.4、可用模板、KVM/Firecracker/ublk 的 Linux 节点；原生首次安装遵循 AgentENV README 和 scripts/install.sh，本补丁不会自动创建这些基础设施。历史版本在 aarch64 完成过安装态验收；本轮新提交只完成构建/mock/契约验证，x86_64 实机验收仍暂缓。

准备三个版本配套制品：Runtime 0.2.4-15.10 RPM、原生网络 mux、支持 AgentENV 的 Webhook；附 SHA256、构建来源、旧版本及已验证的原生备份。不要以文件名代替校验。RPM 当前无组织签名，摘要通过不等于可信发布签名。

Linux 工具：systemd、rpm、Python 3.11+、kubectl、crictl；构建另需对应仓库 Rust/Go 工具链、rpmbuild、jq。安装态测试模板必须有 /bin/sh、sleep、printf、chmod、nohup 和支持 nc -lk -p PORT -e 的 nc。准备节点 kubeconfig、AgentENV 本地 credentials、TLS 证书和 API server 可达的 Webhook 地址。

```bash
export AENV_REPO=/path/to/AgentENV
export PKG_REPO=/path/to/src-openeuler-AgentENV
export KA_REPO=/path/to/KASandbox
export RELEASE_DIR=/path/to/release
```

## 2. 构建与校验（构建机，非 root）

```bash
# Runtime 使用 src-openeuler/AgentENV 的 agentenv.spec、Source0–3 和 Patch0/Patch1。
# 按该仓 docs/OFFLINE-BUILD.md 安装 BuildRequires 并准备独立 rpmbuild 目录。
# 不使用旧 build-package.sh/build-rpm.sh 生成正式交付 RPM。
CARGO_BUILD_JOBS=2 rpmbuild -ba --define '_topdir /path/to/rpmbuild' /path/to/rpmbuild/SPECS/agentenv.spec
cd "$KA_REPO/cri-multiplex"
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go build -o "$RELEASE_DIR/cri-multiplex-agentenv" ./cmd/cri-multiplex
GOWORK=off go build -o "$RELEASE_DIR/portforward-probe" ./e2b-verify/portforward-probe
cd "$KA_REPO/e2b-webhook"
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go build -o "$RELEASE_DIR/e2b-webhook-agentenv" .
```

以上普通 mux 构建仅适用于不要求 Cube 的节点。共存制品需保留原 Cube 后端及装配，完整测试后交付；当前组合源码与差异已归档于工作区 test-results/native-expanded-20261116/coexist-evidence。重新构建的字节不自动具有历史验收结论，也不得冒用旧制品 SHA256。

## 3. 上线前检查与备份

协调维护窗口，停止 AgentENV 新请求，确认无运行/paused Sandbox、在途操作、待恢复事务。共享 mux 重启影响全部 CRI 连接，AgentENV 维护屏障不覆盖 Cube/E2B/Android；必须同时协调这些后端的使用方。不要全局 drain 或重启节点来代替检查。

```bash
systemctl is-active aenv containerd kubelet
systemctl cat cri-multiplex
agentenv-runtime-package status   # 首次装 RPM 前无此命令，可跳过此项
/usr/local/bin/server runtime-health --socket /run/aenv/agentenv-runtime.sock  # 原生版无此接口
kubectl get pods -A -o wide
kubectl get node
crictl --runtime-endpoint=unix:///run/cri-multiplex.sock pods
aenv list --output json
df -h / /home
journalctl -k --since '-5 min' --no-pager
```

单独保存部署前二进制 SHA256、unit/drop-in、环境文件、RuntimeClass、Webhook 配置、kubelet endpoint、mux state-dir 和业务 Pod/service PID 基线。备份目录 root 专用，配置可能含凭据，不公开分发。至少保留已验证原生备份及前一 mux/Webhook。若出现内核 corruption、磁盘不足或业务不空，停止变更。

## 4. Runtime 安装

优先使用 RPM：先暂存，再激活。已有 tar 补丁首次接管必须指定真实原生 v0.2.4 备份；纯原生节点先在已停止调用方的维护窗口执行 tar 补丁 install.sh --dry-run 与 install.sh，生成原生备份；验证 rollback.sh 可恢复原生，再由 RPM 接管该备份。tar 安装/回退都会正常重启 aenv，必须先完成空基线检查。详见 AgentENV scripts/release/agentenv-cri-runtime-patch/RPM.md。

```bash
rpm -Uvh /path/to/agentenv-cri-runtime-0.2.4-15.10.aarch64.rpm
agentenv-runtime-package activate --drained --legacy-drained \
  --native-backup /var/lib/aenv-patch-backups/<已验证的原生备份>
# 后续激活已固定原生备份，可省略 --native-backup。
agentenv-runtime-package status
rpm -V agentenv-cri-runtime
/usr/local/bin/server runtime-health --socket /run/aenv/agentenv-runtime.sock
```

RPM Release 可能含打包修订，如 15.10 或发行版后缀，使用实际制品名。首次原生没有维护屏障，--legacy-drained 只是操作者确认调用方已停止，不能自动排空。aenv.service 会正常重启；HTTP API 和原数据保留。

## 5. mux 配置：独立节点与共存节点

独立节点可将已校验 mux 安装到 /opt/e2b-infra/bin，创建 cri-multiplex.service，Type=simple、Restart=on-failure、After=containerd.service aenv.service，ExecStart 示例：

```text
/opt/e2b-infra/bin/cri-multiplex-agentenv -socket=/run/cri-multiplex.sock -admin-socket=/run/cri-multiplex/admin.sock -containerd-socket=/run/containerd/containerd.sock -state-dir=/var/lib/cri-multiplex/state -agentenv-enabled=true -agentenv-runtime-address=unix:///run/aenv/agentenv-runtime.sock -agentenv-network-mode=runtime-managed
```

独立新节点的完整 unit 示例（/etc/systemd/system/cri-multiplex.service）：

```ini
[Unit]
Description=AgentENV CRI multiplexer
After=containerd.service aenv.service
[Service]
Type=simple
ExecStart=/opt/e2b-infra/bin/cri-multiplex-agentenv -socket=/run/cri-multiplex.sock -admin-socket=/run/cri-multiplex/admin.sock -containerd-socket=/run/containerd/containerd.sock -state-dir=/var/lib/cri-multiplex/state -agentenv-enabled=true -agentenv-runtime-address=unix:///run/aenv/agentenv-runtime.sock -agentenv-network-mode=runtime-managed
Restart=on-failure
RestartSec=3
[Install]
WantedBy=multi-user.target
```

在独立新节点安装受管二进制、创建目录并保存 unit 后执行 systemctl daemon-reload 和 systemctl enable --now cri-multiplex。现有共享节点不得覆盖基础 unit，按下述共存流程操作。

预先创建二进制目录和 state-dir，仅向受信任管理员开放 Runtime/admin socket。仅独立新节点可按本节点 kubelet 安装方式将 containerRuntimeEndpoint 配为 unix:///run/cri-multiplex.sock，并在已授权窗口重启 kubelet；现有共享节点 endpoint 不变，不能直接套用新节点步骤。

Cube 共存节点保留 30-cube.conf、Cube 二进制、共享 state-dir 和 socket。历史组合制品 40-agentenv-cube-native.conf 的配置示例为：

```ini
[Service]
ExecStart=
ExecStart=/opt/e2b-infra/bin/cri-multiplex-agentenv-cube-native -socket=/run/cri-multiplex.sock -admin-socket=/run/cri-multiplex/admin.sock -containerd-socket=/run/containerd/containerd.sock -state-dir=/var/lib/cri-multiplex/state -cube-enabled=true -cubelet-address=127.0.0.1:9999 -agentenv-enabled=true -agentenv-runtime-address=unix:///run/aenv/agentenv-runtime.sock -agentenv-network-mode=runtime-managed
```

这是历史节点示例，不覆盖其他节点原有后端参数。候选必须支持 cube-enabled/cubelet-address。systemd 以后序 drop-in 的 ExecStart 为准，旧基础 unit 的 CNI 参数不能重新成为有效配置。

完成备份、排空、候选校验后，先进入 AgentENV 维护，再将同目录暂存二进制原子替换到受管路径，正常重启 mux；配置有改动才 daemon-reload。运行健康检查，通过后退出维护并做安装态测试。失败按 [回退指导](ROLLBACK.md) 处理。不要删除 state.json，不迁移仍含 cni_record/非空 host_ports 的存量记录；先用旧版本正常清理，确认网络资源归属后再升级。

## 6. Webhook、RuntimeClass 与准入

### Helm / deploy 方式

`webhook.agentenv.enabled` 默认关闭（部署环境变量 `ENABLE_AGENTENV_WEBHOOK=false`）。启用需同时设置 `ENABLE_WEBHOOK=true`、`ENABLE_AGENTENV_WEBHOOK=true`，并提供 `E2B_WEBHOOK_API_KEY`，或通过 `E2B_WEBHOOK_API_CONFIG` 指向含 `teamApiKey` 的受保护 JSON 文件。默认读取 `/root/.e2b/config.json`；不把 key 写入命令行参数或仓库。

只对 `runtimeClassName: agentenv` 的普通 Pod 增加一条准入规则；带 BatchSandbox pod-index 标签的 Pod 继续使用已有入口，避免重复调用。其他 RuntimeClass 不匹配新增规则。Helm 启用此规则要求 Kubernetes >=1.30，使用稳定的 [matchConditions](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/#matching-requests-matchconditions)。旧集群保持开关关闭，按下面的专用 namespace 方式配置。

已有 Cube Provider 的共享 Webhook 必须使用同时包含 Cube 与 AgentENV 的组合源码/镜像；当前独立候选不能覆盖它，也不能直接把只列本分支入口的 Helm 清单应用到同事管理的共享资源。

Helm 中 AgentENV 入口使用 `failurePolicy: Ignore`：Webhook 不可达时由 CRI engine 校验模板要求；Provider 解析失败沿用现有 fail-open 行为，不注入 canonical/build 注解，由 CRI engine/Runtime 最终校验；不应把准入允许视为模板已验证。模板解析只读访问 `E2B_API_URL`，整个解析共用 `E2B_API_TIMEOUT` 超时预算。当前后端不支持从镜像名或 `TEMPLATE_NAME` 推断模板。

`deploy/deploy.sh deploy-webhook` 会构建/推送镜像、更新共享 Webhook Secret/准入配置并重启 Webhook Pod；须有部署维护窗口，本轮只做命令桩测试。caBundle 一次更新全部入口，包含已存在的 Cube 入口；关闭开关不代表卸载，显式卸载参见回退文档。不要对同一资源混用 Helm 和独立 `kubectl apply` 所有权。

容器使用 UID/GID 65532，禁止提权、只读根文件系统，不挂载 ServiceAccount token；TLS Secret 仍需可读。PDB `minAvailable: 1` 会阻止最后一个副本被自愿驱逐，维护时应保留可用副本。

### 独立服务方式


Webhook 独立服务运行已校验二进制；systemd EnvironmentFile 使用 root 0600 文件，配置：

```text
E2B_API_URL=http://127.0.0.1:8000
E2B_API_KEY=<从节点安全凭据配置，不提交>
E2B_API_TIMEOUT=5s
TLS_CERT_FILE=/path/to/tls.crt
TLS_KEY_FILE=/path/to/tls.key
LISTEN_ADDRESS=<API-server可达的本机IP>:8443
```

独立新节点 Webhook unit 示例（/etc/systemd/system/e2b-webhook-agentenv.service）：

```ini
[Unit]
Description=AgentENV admission webhook
After=network-online.target aenv.service
[Service]
Type=simple
EnvironmentFile=/etc/default/e2b-webhook-agentenv
ExecStart=/opt/e2b-infra/bin/e2b-webhook-agentenv
Restart=on-failure
RestartSec=3
[Install]
WantedBy=multi-user.target
```

安装二进制、填写环境文件与证书并校验权限后，执行 systemctl daemon-reload、systemctl enable --now e2b-webhook-agentenv。用 curl --cacert /path/to/ca.pem https://WEBHOOK_HOST:8443/healthz 校验，再创建准入注册；不要先注册不可达 Webhook。

变量名称 E2B_API_* 同样供 AgentENV template resolver 使用。TLS 证书 SAN 必须覆盖准入 URL 主机，API server 信任 caBundle；不要用 curl -k 作为 TLS 验收。共享节点绑定明确 IP，避免占用其他业务的 :8443。已存在的 Webhook 服务/证书/其他后端配置优先复用，不替换为仅适配本测试的配置。

独立新集群创建 RuntimeClass：

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: agentenv
handler: agentenv
```

创建专用测试 namespace agentenv-e2e；MutatingWebhookConfiguration 示例应限制到该 namespace，避免影响其他 Pod：

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: agentenv-native
webhooks:
  - name: agentenv-native.example.internal
    admissionReviewVersions: [v1]
    sideEffects: None
    failurePolicy: Fail
    timeoutSeconds: 10
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: agentenv-e2e
    clientConfig:
      url: https://WEBHOOK_HOST:8443/mutate
      caBundle: BASE64_OF_CA_PEM
    rules:
      - operations: [CREATE]
        apiGroups: [""]
        apiVersions: [v1]
        resources: [pods]
        scope: Namespaced
```

先替换 URL/CA，确认资源归属再 apply；已有同类 Webhook 不重复注册。生产 namespace 纳入准入需单独评估该 namespace 的其他 Pod：Webhook 对未显式指定 runtimeClass 的对象存在旧 E2B 默认选路行为。不要扩展到全 namespace。HTTP /healthz 可用只说明进程活着，还需 server-side dry-run 验证模板解析与注解注入。

## 7. 使用示例与验收

将下面保存为 example-pod.yaml，替换节点和模板，保持 template 来自当前 AgentENV 可用模板：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: agentenv-example
  namespace: agentenv-e2e
  annotations:
    sandbox.cri/template: REPLACE_TEMPLATE_ID_OR_ALIAS
spec:
  nodeName: REPLACE_NODE
  runtimeClassName: agentenv
  restartPolicy: Never
  containers:
    - name: app
      image: agentenv.dev/app:v1
      imagePullPolicy: IfNotPresent
      command: [/bin/sh, -c, "sleep 600"]
```

```bash
kubectl create --dry-run=server -f example-pod.yaml -o yaml
# 检查已注入 aenv.dev/template-id 与 sandbox.cri/build，再创建。
kubectl create -f example-pod.yaml
kubectl wait -n agentenv-e2e pod/agentenv-example --for=condition=Ready --timeout=180s
kubectl exec -n agentenv-e2e agentenv-example -- /bin/sh -c 'echo hello'
kubectl delete -n agentenv-e2e pod/agentenv-example --wait=true
```

image 是 CRI 逻辑引用，guest 程序来自 Template。不要设置非零 HostPort 或 auto-pause=true；需要访问 guest 时使用 AgentENV 节点代理或 PortForward。九个 /internal/* API 调用方应使用两仓原始 OpenAPI YAML，不能使用 target/codegen/server-openapi.json。

最终验收：Runtime Ready、maintenance blocked=false、pending=none、CRI conditions 全 True、Node Ready、测试 Sandbox/Pod 及资源清空；比较 Cube 配置哈希、其他业务 Pod 和 service PID。第 42 项安装态测试见 [测试指导](TESTING.md)。单独 crictl info 主要反映 containerd，不能替代 AgentENV runtime-health 和真实 Pod 测试。

# 测试执行指导

## 测试分层

| 层级 | 执行位置 | 内容 | 默认范围 |
|---|---|---|---|
| Go 单元测试 | 非 root 构建机 | CRI 生命周期、原生网络事实/所有权、幂等、HostPort 拒绝、Webhook | 可执行 |
| 九接口 HTTP 与契约 | 授权节点 + 契约环境 | 参数、鉴权、幂等、隔离、并发、两仓 YAML 一致性 | 可执行，会写入唯一合成元数据 |
| 维护专项 | 独立临时目录/私有 mount namespace | 维护 CLI、RPM 服务桩 | 显式选择无故障 suite |
| 安装态 42 | 已排空、已安装的 Linux 节点 | 真实 kubelet/Webhook/VM/网络/清理 | 可执行，不重启服务 |
| 历史故障、soak、容量 | 专用环境 | 34–39 及故障 suite | 本项目暂缓，不自动运行 |

当前节点存在最近复测记录的内核 list_add corruption 告警。开始新 VM 测试前先复核环境健康；不要用反复跑测替代异常处理。真实 OpenFuyao 调用方尚未就绪，九接口 HTTP/契约通过不等于调用方联调通过。

## 1. 构建机单元测试

```bash
cd "$KA_REPO/cri-multiplex"
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go build -o /path/to/portforward-probe ./e2b-verify/portforward-probe
cd "$KA_REPO/e2b-webhook"
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

新增离线交付检查（不需要 API key 或 kubeconfig）：

```bash
# 需要 Helm 3、Bash、Python 3 和 PyYAML；kubectl/openssl 使用测试桩。
python3 "$KA_REPO/deploy/tests/test_agentenv_webhook.py"
python3 "$KA_REPO/scripts/check-agentenv-contract.py" --agentenv "$AENV_REPO/src/api/openapi.yml"
cd "$KA_REPO/packages/api"
GOWORK=off go generate ./internal/api
GOWORK=off go test ./internal/api
# 生成后 git diff 不应出现未提交的 api.gen.go 变化。
```

Webhook 全模块测试包含只读模板解析、鉴权、重定向拒绝、错误响应、大小限制、超时取消及真实 Provider 的 AdmissionReview。`AGENTENV_TEMPLATE_API_E2E=1` 是真实 API 的 opt-in 开关，本轮保持未设置。Helm 测试是离线渲染，不等于 API server 已接受该配置。

需要 Linux 目标构建 helper；不能把 Windows .exe 传到节点。完整 Rust unit suite 内含独立子进程恢复测试，若本轮禁止任何故障测试，不选择 unit/rpm-isolated；维护契约可单独选择 maintenance-unit（非 root）：

```bash
cd "$AENV_REPO"
python3 scripts/tests/run-release-regression.py --suite maintenance-unit --output /path/to/new-maintenance-unit
```

## 2. 九接口与两仓契约

Python 3.11+；schema 环境另需 PyYAML、jsonschema。节点 API 管理凭据读取本地文件或安全环境变量，不写进脚本、命令历史或报告。默认 /root/.config/aenv/credentials 的 api_key；可通过 AENV_CREDENTIALS 指定文件。

```bash
cd "$AENV_REPO"
export AENV_API_URL=http://127.0.0.1:8000
export AENV_CREDENTIALS=/root/.config/aenv/credentials
python3 scripts/tests/run-release-regression.py --suite http --output /path/to/new-http
python3 scripts/tests/run-release-regression.py --suite contract \
  --http-results /path/to/new-http/http-result.json \
  --kasandbox-spec "$KA_REPO/spec/openapi.yml" \
  --output /path/to/new-contract
```

可加 --maintenance-results /path/to/new-maintenance-unit/maintenance-result.json，将维护前/中/后的真实响应纳入契约校验。HTTP suite 使用唯一合成数据并清理；失败时保留结果，按本轮 ID 核查，不批量删除 catalog。

覆盖 pause-snapshot、snapshot-templates、last-snapshot、builds/status、snapshot-state、删除 sandbox snapshot、删除 snapshot-template、snapshot resolve、删除 template。详细场景见 AgentENV scripts/tests/RELEASE-REGRESSION.md。两仓请求参数和必填要求必须一致；客户端从原始 src/api/openapi.yml 或 spec/openapi.yml 生成。

## 3. 无故障维护专项

仅 Linux root；RPM suite 需要 unshare/mount 和现有默认安装路径，但用私有 mount namespace 与服务桩，不停止宿主服务。

```bash
cd "$AENV_REPO"
python3 scripts/tests/run-release-regression.py \
  --suite maintenance-cli --suite rpm-maintenance \
  --maintenance-server /usr/local/bin/server \
  --output /path/to/new-maintenance
```

不要追加 rpm-isolated、soak；它们不属于本轮默认范围。以上服务桩验证不能代替真实 RPM 升降级、卸载验收。

## 4. 原生网络安装态

执行前：Runtime Ready、Sandbox 空、无 agentenv/aenv RuntimeClass Pod；已存在 agentenv-e2e namespace、agentenv RuntimeClass、模板及可用 Webhook，kubectl/crictl 可用。测试读取 root credentials 和本机 HTTP 8000。该脚本不是通用远端 API runner，非默认路径环境应先适配后审核。

```bash
cd "$KA_REPO/cri-multiplex"
python3 e2b-verify/42_agentenv_native_installed_e2e.py \
  --output /path/to/new-native-installed-result \
  --template REPLACE_TEMPLATE_ID_OR_ALIAS \
  --node REPLACE_NODE \
  --node-ip REPLACE_NODE_IP \
  --socket /run/cri-multiplex.sock \
  --portforward-helper /path/to/portforward-probe
```

输出目录必须不存在。测试只创建唯一标签 Pod，验证 server-side dry-run 注解、Pod/Container Ready、Exec 输出与退出码 37、Stats、PodIP HTTP、原生代理、真实 PortForward 数据及清理。finally 只删除本轮 Pod，保留 namespace；比较 boot ID、共享服务 PID、netns 和 Pod UID 集合。HostPort 拒绝由单元测试覆盖，第 42 项本身不含非零 HostPort 请求。

成功同时要求进程退出 0、result.json 为 PASS、cleanup=true；live-result.json 仅代表清理前功能通过，不是最终 PASS。进程强杀可能绕过 finally，人工清理必须核对 pod-input.json 中本轮名称和 native-installed-probe 标签；不得删除整个 namespace。保存前后快照，不仅保留终端 PASS 字样。

## 5. 结果、性能与暂缓项

统一回归生成 results.json、junit.xml 与各 suite 日志；JUnit 每项代表一个 suite，不等于业务子用例数。输出应归档版本/二进制 SHA256、环境、模板、命令、开始结束时间及失败清理记录，不包含凭据。

性能复测保持相同版本/模板/CPU/内存/磁盘/并发/冷却与计时边界，分别记录 HTTP、CRI，保留失败与慢样本。原生 v0.2.4 对照需版本切换和恢复验收；历史 test-results 脚本含固定路径，不列入默认业务回归。最新复测未完成，不能根据 7 个原生成功样本得出稳定加速结论。

不执行：节点重启/断电、Runtime SIGKILL/recovery、CNI 故障、共享 mux 活动业务故障测试、x86_64 实机、Cube 独立实机回归、容量/长期 soak、CoreDNS 排查。34/35 的 CNI 用例不适用于裁减版，40/41 旧入口不作为当前原生网络总入口；未来启用其他项目需单独确定范围。

本次 PR 不携带 34–41 历史 AgentENV 测试脚本；上文编号用于说明历史边界，当前安装态入口为 42，旧脚本仍保留在开发工作区。

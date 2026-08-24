# E2B 自托管环境自动化验收脚本

本项目用于验证自托管 E2B 的控制面与数据面链路。测试通过统一入口执行 60 个真实 E2E 用例，覆盖 Sandbox 创建、Template 构建、命令执行、文件上传下载及资源查询，可用于版本上线验收和升级回归。


## 1. 测试范围

| 业务 | 用例编号 | 数量 | 核心检查 |
| --- | --- | ---: | --- |
| 创建 Sandbox | `SB-001` ~ `SB-010` | 10 | 参数组合、生命周期、安全属性、错误边界 |
| 创建 Template | `TP-001` ~ `TP-008` | 8 | 镜像构建、Dockerfile、资源参数、失败归因 |
| 执行命令 | `CMD-001` ~ `CMD-014` | 14 | 退出码、输出、用户、目录、超时和异常目标 |
| 上传文件 | `UP-001` ~ `UP-008` | 8 | 文本、二进制、空文件、路径、覆盖和摘要 |
| 下载文件 | `DL-001` ~ `DL-008` | 8 | 内容一致性、目录创建、覆盖保护和异常路径 |
| 查询 Sandbox | `LSB-001` ~ `LSB-006` | 6 | 可见性、metadata 和分页边界 |
| 查询 Template | `LTP-001` ~ `LTP-006` | 6 | 可见性、构建状态、稳定性和分页边界 |

负向用例收到预期错误时记为 `PASS`。例如，不存在的 Template 被服务端拒绝，说明错误处理符合预期。

## 2. 目录结构

```text
e2b-scripts/
├── start.sh                       # Linux 统一入口
├── start.py                       # 环境初始化与命令转发
├── e2b_validator/                 # 验收实现
│   ├── build_prod.py              # 子命令注册
│   ├── create_sandbox.py          # 创建 Sandbox
│   ├── create_template.py         # 创建 Template
│   ├── run_command.py             # 执行命令
│   ├── upload_file.py             # 上传文件
│   ├── download_file.py           # 下载文件
│   ├── list_sandboxes.py          # 查询 Sandbox
│   ├── list_templates.py          # 查询 Template
│   └── e2e_*.py                   # 用例、校验和报告
├── e2b-self-hosted.env.example    # 配置模板
├── requirements.txt               # Python 依赖
├── .gitignore                     # 本地数据排除规则
└── README.md                      # 使用说明
```

`start.sh` 是 Linux 对外入口，`start.py` 是唯一的根目录 Python 入口。`e2b_validator/` 中的模块由入口统一加载，无需单独执行。

## 3. 环境准备

### 3.1 运行要求

| 项目 | 要求 |
| --- | --- |
| 操作系统 | Linux；建议在 E2B API 节点运行 |
| Python | 3.10 或更高版本 |
| E2B SDK | `e2b>=2.0.0`，首次启动自动安装 |
| 服务 | API、client-proxy、template-manager、Nomad、Consul、Harbor 可用 |
| 网络 | 可访问 API、client-proxy 和 Harbor |
| 权限 | Template 构建与基础镜像自动发现建议使用 `root` |

脚本可放在任意目录，不依赖 `/opt/e2b-infra` 作为代码路径。默认只在 API 节点读取 `/opt/e2b-infra/dep/.env` 和 `/root/.e2b/config.json`，用于自动发现当前部署参数。

### 3.2 获取代码

```bash
# 克隆仓库
git clone https://gitcode.com/fqy_Sandbox/KASandbox.git

# 进入验收脚本目录
cd KASandbox/e2b-scripts
```

### 3.3 获取 Team API Key

在 E2B API 节点执行：

```bash
# 读取当前部署的 Team API Key
python3 -c 'import json; print(json.load(open("/root/.e2b/config.json", encoding="utf-8"))["teamApiKey"])'
```

### 3.4 配置方式

脚本在 E2B API 节点运行时，会自动读取当前部署配置，通常无需创建 `.env`。先直接执行只读检查：

```bash
# 自动安装依赖、编译源码并检查资源查询链路
bash start.sh
```

脚本在其他节点运行，或需要覆盖自动发现结果时，创建本地配置：

```bash
# 复制配置模板
cp e2b-self-hosted.env.example .env

# 限制凭据文件权限
chmod 600 .env

# 编辑连接参数
vi .env
```

最小配置如下：

```dotenv
# 当前部署签发的 Team API Key
E2B_API_KEY=<team-api-key>

# 非 API 节点填写可访问的 API 地址
E2B_API_URL=http://<api-host>:3000
```

也可以使用项目目录外的配置文件：

```bash
# 使用受控目录中的配置
bash start.sh --env-file /root/secure/e2b.env list-sandboxes
```

### 3.5 配置项

| 变量 | 必填条件 | 说明 |
| --- | --- | --- |
| `E2B_API_KEY` | 无法自动读取客户端配置时 | Team API Key |
| `E2B_API_URL` | 非 API 节点 | API 地址；API 节点默认 `http://127.0.0.1:3000` |
| `E2B_E2E_BASE_IMAGE` | 否 | 覆盖自动发现的基础镜像 |
| `E2B_DOMAIN` | 否 | Sandbox 数据面域名或 IP |
| `E2B_HTTP_SSL` | 否 | 数据面是否使用 TLS |
| `E2B_PROXY_PORT` | 否 | client-proxy 端口，默认 `3002` |
| `E2B_SANDBOX_URL` | 否 | 固定 Sandbox 数据面入口 |


基础镜像按以下顺序解析：

1. 命令行 `--base-image`。
2. 环境变量 `E2B_E2E_BASE_IMAGE`。
3. 可见 Template 的构建元数据。
4. API 节点部署配置和本地 Docker 镜像。

正常场景只需提供 Team API Key。仅当自动发现无法获得符合当前自托管 Harbor 的镜像时，才指定 `E2B_E2E_BASE_IMAGE`。

## 4. 使用方法

### 4.1 执行全量验收

```bash
# 自动准备 fixture 并执行 60 个真实用例
bash start.sh test-e2e --all
```

默认构建本轮隔离的 `e2e-<run-id>-fixture`，测试会创建真实 Sandbox 和 Template，应预留 Nomad、Firecracker、Harbor 及构建节点资源。

指定已知可用的 Template：

```bash
# 使用指定 Template 执行业务验证
bash start.sh test-e2e --all --template <ready-template>
```

临时覆盖基础镜像：

```bash
# 仅对本次测试指定基础镜像
bash start.sh test-e2e --all \
  --base-image <registry>/<project>/ubuntu:22.04-custom
```

执行指定用例：

```bash
# 单独回归两个用例
bash start.sh test-e2e \
  --case SB-004 \
  --case UP-007
```

### 4.2 查询资源

```bash
# 查询全部 Sandbox
bash start.sh list-sandboxes

# 查询全部 Template
bash start.sh list-templates
```

### 4.3 创建 Sandbox

```bash
# 创建带生命周期、metadata 和环境变量的 Sandbox
bash start.sh create-sandbox \
  --template <ready-template> \
  --timeout 300 \
  --metadata '{"env":"test","owner":"automation"}' \
  --envs '{"APP_ENV":"test"}'
```

### 4.4 创建 Template

```bash
# 使用 Harbor 中的基础镜像构建 Template
bash start.sh create-template \
  --name template-$(date +%Y%m%d-%H%M%S) \
  --base-image <registry>/<project>/ubuntu:22.04-custom \
  --cpu-count 1 \
  --memory-mb 1024
```

### 4.5 执行命令

```bash
# 检查默认用户和工作目录
bash start.sh run-command \
  --sandbox-id <sandbox-id> \
  --command "whoami && pwd"
```

### 4.6 上传和下载文件

```bash
# 上传本地文件
bash start.sh upload-file \
  --sandbox-id <sandbox-id> \
  --local-path ./demo.txt \
  --remote-path /tmp/demo.txt

# 下载远端文件
bash start.sh download-file \
  --sandbox-id <sandbox-id> \
  --remote-path /tmp/demo.txt \
  --local-path ./downloads/demo.txt
```

## 5. E2E 用例清单

下表与 `e2b_validator/e2e_test_cases.py` 保持一致。`<fixture-template>`、`<base-image>` 和 `<run-id>` 在运行时动态生成或解析。

### 5.1 创建 Sandbox（10 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `SB-001` | 指定模板创建共享 Sandbox | fixture Template；`timeout=3600` | 创建成功，返回真实 ID，命令链路可用 |
| `SB-002` | metadata 与 envs 组合 | `timeout=120`；metadata 含 `env/owner/run_id`；注入 `APP_ENV/E2E_RUN_ID` | 创建成功，环境变量可在 Sandbox 内读取 |
| `SB-003` | secure 模式安全语义 | `secure=true`；`timeout=120` | 创建成功，可观察 secure 属性或安全访问凭据 |
| `SB-004` | 最短生命周期 10 秒 | `timeout=10`；宽限检查 35 秒 | 创建后短暂可用，随后不可连接 |
| `SB-005` | timeout 为 0 | `timeout=0` | 客户端拒绝非正整数，不创建资源 |
| `SB-006` | timeout 为负数 | `timeout=-1` | 客户端拒绝负数，不创建资源 |
| `SB-007` | metadata 非法 JSON | `metadata={bad` | 返回 JSON 解析错误，不到达服务端 |
| `SB-008` | envs 使用数组 | `envs=[]` | 拒绝非 JSON object 的 envs |
| `SB-009` | 不存在的 Template | `template=missing-<run-id>` | 服务端拒绝请求，不返回 Sandbox ID |
| `SB-010` | 空 Template 名称 | Template 仅含空格 | 客户端拒绝空资源名称 |

### 5.2 创建 Template（8 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `TP-001` | 真实 base image 构建 | 自动发现的 Harbor Ubuntu 镜像；`CPU=1`；`memory=1024 MB` | template-manager 完成构建；失败时可归因到镜像或基础设施 |
| `TP-002` | inline Dockerfile 构建 | `FROM <base-image>`；写入 marker；`memory=512 MB` | Dockerfile content 链路可用，构建名称唯一 |
| `TP-003` | skip-cache 组合 | `skip-cache=true`；`CPU=2`；`memory=1024 MB` | 参数被接收，产生可追踪构建结果 |
| `TP-004` | Dockerfile 不存在 | 指定不存在的本地文件 | 请求发出前返回文件不存在 |
| `TP-005` | CPU 为 0 | `cpu-count=0` | 客户端拒绝非法 CPU 值 |
| `TP-006` | 内存为负数 | `memory-mb=-1` | 客户端拒绝非法内存值 |
| `TP-007` | 无效镜像名称 | `invalid.invalid/<run-id>:missing` | 不产生成功 Template，错误可归因 |
| `TP-008` | 多个构建源冲突 | 同时指定 base image 和 inline Dockerfile | 客户端执行 mutually-exclusive 校验并拒绝请求 |

### 5.3 执行命令（14 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `CMD-001` | 正常退出 | 输出 `command-ok` | 退出码为 `0`，stdout 内容正确 |
| `CMD-002` | 非零退出码 | stderr 输出后 `exit 7` | 保留退出码 `7` 和 stderr |
| `CMD-003` | stdout 与 stderr | 分别写入两个输出流 | 两个输出流可区分，退出码为 `0` |
| `CMD-004` | 环境变量注入 | `CASE_VALUE=env-ok` | Sandbox 命令读取到注入值 |
| `CMD-005` | 工作目录 | `cwd=/tmp`；执行 `pwd` | 输出 `/tmp` |
| `CMD-006` | 默认 user 用户 | 执行 `whoami` | 输出默认用户 `user` |
| `CMD-007` | 显式 root 用户 | `user=root`；执行 `id -u && whoami` | UID 为 `0`，用户为 `root` |
| `CMD-008` | 特殊字符 | 空格、分号、中文和 `$HOME` 字面量 | 参数不被错误拆分或展开 |
| `CMD-009` | 多行输出 | 输出三行文本 | 完整返回至 `line3` |
| `CMD-010` | 长输出 | 生成 32768 字节并统计 | 返回长度 `32768`，无截断误判 |
| `CMD-011` | 命令超时 | `sleep 3`；`timeout=1` | 命令按预期超时；该负向用例通过时状态为 `PASS` |
| `CMD-012` | 命令为空 | 空字符串 | 客户端拒绝请求 |
| `CMD-013` | 工作目录不存在 | `cwd=/missing/<run-id>` | 服务端拒绝无效目录；负向用例状态为 `PASS` |
| `CMD-014` | Sandbox ID 不存在 | `sandbox_id=missing-<run-id>` | 服务端拒绝未知 Sandbox；负向用例状态为 `PASS` |

### 5.4 上传文件（8 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `UP-001` | 文本文件 | `hello-e2b` → `/tmp/e2e-text.txt` | 远端 SHA-256 与本地一致 |
| `UP-002` | 二进制文件 | `00 01 fe ff` → `/tmp/e2e-binary.bin` | 二进制内容逐字节一致 |
| `UP-003` | 空文件 | 0 字节 → `/tmp/e2e-empty` | 远端为空文件，摘要一致 |
| `UP-004` | 中文 UTF-8 | 中文内容 → `/tmp/e2e-cn.txt` | UTF-8 字节与摘要一致 |
| `UP-005` | 嵌套路径 | `/tmp/e2e/nested/value.txt` | 父目录处理正确，摘要一致 |
| `UP-006` | 较大文件 | 4096 字节 → `/tmp/e2e-large.bin` | 远端内容完整，摘要一致 |
| `UP-007` | 覆盖同名远端文件 | 先写 `old-value`；以 `root` 上传 `replacement` | 覆盖成功，所有者与权限不阻断 Files API，摘要一致 |
| `UP-008` | 本地文件不存在 | 不存在的源路径 | 返回文件不存在，不写入 Sandbox |

### 5.5 下载文件（8 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `DL-001` | 文本文件 | 下载 `download-text` | 本地内容与远端逐字节一致 |
| `DL-002` | 二进制文件 | 下载 `00 01 fe ff` | 本地二进制内容一致 |
| `DL-003` | 空文件 | 下载 0 字节文件 | 本地文件存在且长度为 0 |
| `DL-004` | 中文文件 | 下载 UTF-8 中文内容 | 本地编码和字节内容一致 |
| `DL-005` | 自动创建本地父目录 | 目标位于不存在的嵌套目录 | 自动创建父目录并正确写入 |
| `DL-006` | 覆盖本地文件 | `overwrite=true` | 已有文件被新内容替换 |
| `DL-007` | 本地覆盖保护 | 已有本地文件，未指定 overwrite | 拒绝覆盖，原文件保持不变 |
| `DL-008` | 远端文件不存在 | `/tmp/missing-<run-id>` | 下载失败，不产生有效本地文件 |

### 5.6 查询 Sandbox（6 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `LSB-001` | 基础查询 | 不限制页数 | 返回合法 JSON |
| `LSB-002` | 创建后可见 | 依赖 `SB-001` | 主测试 Sandbox 出现在查询结果中 |
| `LSB-003` | 单页查询 | `max-pages=1` | 分页限制生效，返回合法 JSON |
| `LSB-004` | 多个本轮 Sandbox 可见 | 查询本轮创建资源 | 本轮资源均可识别 |
| `LSB-005` | metadata 精确查询链路 | 依赖 `SB-002` 的 `run_id` metadata | 指定 Sandbox 的 metadata 可见且匹配 |
| `LSB-006` | 分页值为 0 | `max-pages=0` | 客户端拒绝分页下界非法值 |

### 5.7 查询 Template（6 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `LTP-001` | 基础查询 | 不限制页数 | 返回合法 JSON，状态可解释 |
| `LTP-002` | 本轮 fixture 可见 | 匹配 `<fixture-template>` | fixture 出现在查询结果中 |
| `LTP-003` | 单页查询 | `max-pages=1` | 分页限制生效，返回合法 JSON |
| `LTP-004` | 构建后状态查询 | 查询本轮构建的 Template | 构建资源和状态可见 |
| `LTP-005` | 重复查询稳定性 | 连续执行查询 | 返回结构稳定，无随机解析差异 |
| `LTP-006` | 分页值为负数 | `max-pages=-1` | 客户端拒绝负分页值 |

## 6. 结果与判定

每次运行生成独立结果目录：

```text
test-results/<run-id>/
├── report.md       # 用例步骤、判定和证据
├── result.json     # 结构化结果
├── resources.json  # 本轮资源和清理记录
└── commands.log    # 脱敏命令证据
```


| 状态 | 含义 |
| --- | --- |
| `PASS` | 实际行为符合预期；非法请求被正确拒绝也属于通过 |
| `FAIL` | 接口行为或独立校验结果与预期不一致 |
| `BLOCKED` | 服务、调度、镜像、网络或资源问题阻断有效结论 |
| `SKIPPED` | 前置用例未通过，继续执行无法产生有效结论 |

运行编号采用 `YYYYMMDD-HHMMSS-随机后缀`，例如 `20260725-022435-8cd57e`，用于隔离并关联同一轮资源、日志和报告。

## 7. 故障定位

### 7.1 API 鉴权失败

> `401 Invalid API key`

重新读取 `/root/.e2b/config.json` 中的 Team API Key。API 节点验收时，检查当前目录是否残留旧 `.env`，避免旧凭据覆盖自动发现结果。

```bash
# 检查当前配置是否包含旧 Key；
grep '^E2B_API_KEY=' .env 2>/dev/null
```

### 7.2 Sandbox 放置失败

> `500 Failed to place sandbox`

按 Nomad allocation、template-manager、Firecracker/cgroup、资源限制和 restart policy 的顺序排查。该错误通常与调度或运行资源有关，不由 Template 时间戳导致。

```bash
# 检查新版本关键监听端口
ss -tlnp | grep -E ':3000|:3002|:5008'

# 查看 Nomad Job 状态
nomad job status

# 查看失败 allocation 的事件和资源信息
nomad alloc status <alloc-id>
```

### 7.3 Template 构建失败

> `template builder not found`
>
> `failed to get builder client`

检查 template-manager、Template builder allocation、Consul 服务注册和 Harbor 镜像可达性。基础镜像自动发现成功，只说明镜像名称已解析，不代表构建链路一定可用。

```bash
# 检查 template-manager 服务注册
consul catalog services | grep template

# 检查 Harbor 基础镜像是否存在
docker manifest inspect <registry>/<project>/<image>:<tag>
```

### 7.4 命令与文件用例集中失败

如果 Sandbox 创建通过，但命令执行出现 `124/126`，同时上传下载摘要不一致，应优先检查该 Template 的默认用户、Shell、基础命令、目录权限和 client-proxy 数据面，不应仅按 Template 名称更换断言。

```bash
# 检查目标 Template 的最小运行能力
bash start.sh create-sandbox \
  --template <ready-template> \
  --timeout 300

# 使用返回的 ID 检查用户、Shell 和目录权限
bash start.sh run-command \
  --sandbox-id <sandbox-id> \
  --command 'id; command -v sh; stat -c "%U:%G %a %n" /tmp'
```

## 8. 退出码

| 退出码 | 含义 |
| ---: | --- |
| `0` | 操作成功，或全部 E2E 用例符合预期 |
| `1` | SDK、网络、服务端错误，或存在 `FAIL` / `BLOCKED` |
| `2` | 参数或配置错误 |
| `130` | 用户中断 |
| 其他 | `run-command` 返回的远端命令退出码 |

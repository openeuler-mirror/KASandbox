# template-migrate 使用手册

工具版本: `0.1.0-demo`

本手册讲解 `template-migrate` 的完整用法:构建、端点写法、命令参考、导入语义、
Mooncake 配置与常见错误。项目简介与快速开始见 [README.md](README.md)。

---

## 一、工具简介

`template-migrate` 把 E2B Template 的 ready Build 从一个环境迁移到另一个环境:
Template ID 和 Build UUID 保持不变,Team/Cluster 所有权重新绑定到目标 Team,
历史 Build Node 清空。

| | 源端 | 目标端 |
| --- | --- | --- |
| Catalog | 本地 JSON 文件、PostgreSQL 15+ | 本地 JSON 文件、PostgreSQL 15+ |
| 对象存储 | 本地目录、S3/MinIO | 本地目录、S3/MinIO、**Mooncake**(仅目标端) |

迁移范围:

- ✅ 普通 Template、Snapshot Template、ready Build 及其完整对象闭包、
  Build Assignment(Tag 历史)、Alias 及 Namespace 转换
- ❌ Runtime Snapshot、非 ready Build、缺失 Header 的 legacy Build
  (导出时报 `legacy_headerless_build`)、Mooncake 源端导出

Bundle 是一个**目录**,不是 tar/zip 或单个镜像文件。

## 二、二进制与构建

两个制品:

| 制品 | 能力 |
| --- | --- |
| `template-migrate` | File、PostgreSQL、S3/MinIO |
| `template-migrate-mooncake` | 以上全部 + `mooncake://` 导入目标(Linux/ARM64 CGO) |

普通制品收到 `mooncake://` 时会明确报错 `Mooncake support is not included in
this binary`,此时改用 Mooncake 制品。

普通版本构建:

```bash
./build.sh
BIN="$PWD/bin/template-migrate"   # 本手册后续示例统一用 $BIN 指向所选制品
```

`migrate-tool` 是独立 Go 模块,**不在仓库根 `go.work` 的 `use` 列表里**(与
`cri-multiplex` 相同)。仓库内脚本都已 `export GOWORK=off`;直接敲 `go` 命令时
必须自己带上,否则报 `directory prefix . does not contain modules listed in
go.work`:

```bash
GOWORK=off go build -buildvcs=false -trimpath -o bin/template-migrate ./cmd/template-migrate
GOWORK=off go test ./...
```

Mooncake 版本必须在已安装 Mooncake 头文件和动态库的 Linux/ARM64 主机上构建:

```bash
./scripts/build-mooncake.sh
# 输出: bin/template-migrate-mooncake(同时覆盖普通版本的全部能力)
```

## 三、端点写法

### Catalog 端点

```text
/path/to/catalog.json
file:///path/to/catalog.json
postgresql://USER@DB_HOST:5432/DB_NAME?sslmode=require
```

PostgreSQL 密码**不要写进 URI**(会进入 shell history):用 `~/.pgpass`
(`PGPASSFILE`)、`PGSERVICE` 或 `PGPASSWORD` 环境变量提供。`sslmode` 按目标库
实际配置选择(未启用 TLS 的测试库用 `sslmode=disable`)。

PostgreSQL 适配器连接后会做能力预检,**仅版本达到 15 并不足够**,还要求:
migration baseline `20260218120000`、既定表/列/约束/触发器全部存在
(含 `(alias, namespace)` NULLS NOT DISTINCT 唯一索引和 `status_group` 触发器)、
当前角色对 catalog 表旁路 RLS。任一不满足,命令在预检阶段即拒绝执行。

### 对象存储端点

```text
/path/to/objects
file:///path/to/objects
s3://BUCKET/PREFIX?region=REGION
mooncake://NAMESPACE          # 仅 import --store 可用
```

S3 URI 只接受 `region`、`endpoint`、`path_style` 三个查询参数,不接受凭证。
MinIO 等 S3 兼容服务需要 endpoint 和 path-style,两种写法等价:

```bash
# 写法一: endpoint 放 URI(需 URL 编码)
s3://BUCKET/PREFIX?endpoint=http%3A%2F%2FMINIO_HOST%3A9000&path_style=true&region=us-east-1

# 写法二: endpoint 走环境变量
export TM_S3_ENDPOINT='http://MINIO_HOST:9000'
s3://BUCKET/PREFIX?region=us-east-1
```

设置了 endpoint(任一来源)时 path-style 自动开启,可用 `path_style=false` 关闭。
Region 的优先级:URI `region` 参数 → `AWS_REGION` → `AWS_DEFAULT_REGION` →
`us-east-1`。AWS/MinIO 凭证走标准 AWS SDK 凭证链(`AWS_ACCESS_KEY_ID`、
`AWS_SECRET_ACCESS_KEY` 环境变量或 `~/.aws/credentials`)。

`mooncake://` 中只写 namespace,不写主机端口。连接参数来自与 E2B Job 一致的
`MOONCAKE_*` 环境变量(见第八节)。

### 环境变量默认值

| 环境变量 | 对应参数 |
| --- | --- |
| `TM_SOURCE_CATALOG` | `list`/`export` 的 `--catalog` 默认值 |
| `TM_SOURCE_STORE` | `export` 的 `--store` 默认值 |
| `TM_TARGET_CATALOG` | `import` 的 `--catalog` 默认值 |
| `TM_TARGET_STORE` | `import` 的 `--store` 默认值 |
| `TM_S3_ENDPOINT` | S3 兼容 endpoint(URI 未提供时) |

命令行 `--catalog`/`--store` 始终优先于环境变量。`USER`、`DB_HOST`、`BUCKET`
等大写内容都是占位符,执行前必须换成真实值。

## 四、标准迁移流程

```text
list → export → inspect → verify → import(dry-run) → import --apply
```

```bash
# 1. 查询源端有哪些可迁移 Template(list 只访问 Catalog,不访问对象存储)
"$BIN" list --catalog "$TM_SOURCE_CATALOG" --all --all-tags

# 2. 导出(--out 目录必须不存在,工具不覆盖已有 Bundle)
BUNDLE="$PWD/run/export-$(date +%Y%m%d-%H%M%S).bundle"
"$BIN" export \
  --catalog "$TM_SOURCE_CATALOG" \
  --store "$TM_SOURCE_STORE" \
  --template-id TEMPLATE_ID \
  --out "$BUNDLE"

# 3. 快速检查结构(不扫描大对象内容)
"$BIN" inspect "$BUNDLE"

# 4. 完整校验(对象大小、SHA-256、Header 依赖闭包);导入前必须通过
"$BIN" verify "$BUNDLE"

# 5. dry-run 预览计划(默认行为,不写任何数据)
#    Bundle 含 literal namespace 时,dry-run 和 apply 都必须追加
#    --literal-namespace SOURCE=TARGET(仓库 fixture 需要 legacy=archive)
"$BIN" import "$BUNDLE" \
  --catalog "$TM_TARGET_CATALOG" \
  --store "$TM_TARGET_STORE" \
  --target-team slug:TARGET_TEAM \
  --format json

# 6. 确认无 conflicts 后,同一条命令追加 --apply 重跑
"$BIN" import "$BUNDLE" \
  --catalog "$TM_TARGET_CATALOG" \
  --store "$TM_TARGET_STORE" \
  --target-team slug:TARGET_TEAM \
  --format json \
  --apply
```

dry-run 输出中的 `"applied": false` 表示预演正常完成,**不是失败**。

## 五、命令参考

### list — 查询源 Catalog

```text
list [--catalog ENDPOINT] [--format table|json] [选择参数]
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--catalog` | `$TM_SOURCE_CATALOG` | 源 Catalog 端点 |
| `--format` | `table` | `table` 或 `json` |

不传 Template 选择参数(或用 `--all`)时进入**目录发现模式**:缺 ready Build
的 Template 也会列出,Assignment 计数为 0,整条命令不失败。显式指定
`--template-id`/`--name`/`--name-glob` 时保持严格:选中的 Template 在所选
Tag 上没有 ready Build 即报错。`export` 始终严格。

### export — 导出 Bundle

```text
export [--catalog ENDPOINT] [--store ENDPOINT] --out DIR [选择参数]
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--catalog` | `$TM_SOURCE_CATALOG` | 源 Catalog 端点 |
| `--store` | `$TM_SOURCE_STORE` | 源对象存储端点 |
| `--out` | (必填) | 输出 Bundle 目录,必须不存在 |

与 `list` 不同,`export` **必须**显式给出至少一个 Template 选择参数
(`--template-id`/`--name`/`--name-glob`/`--all` 之一)。

### inspect — 快速检查 Bundle

```text
inspect BUNDLE_DIR [--format table|json]
```

校验 manifest、digest、记录文件摘要和关系完整性,不扫描大对象内容。

### verify — 完整校验 Bundle

```text
verify BUNDLE_DIR [--format table|json]
```

在 inspect 基础上流式读取全部对象,核对大小与 SHA-256,并验证 Header 依赖
闭包。完全离线,不需要源端或目标端连接。

### import — 导入 Bundle

```text
import BUNDLE_DIR [--catalog ENDPOINT] [--store ENDPOINT]
       --target-team (slug:SLUG|id:UUID)
       [--include-global-aliases]
       [--literal-namespace SOURCE=TARGET]...
       [--conflict-policy fail|skip-identical]
       [--apply] [--format table|json]
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--catalog` | `$TM_TARGET_CATALOG` | 目标 Catalog 端点 |
| `--store` | `$TM_TARGET_STORE` | 目标对象存储端点(File/S3/`mooncake://`) |
| `--target-team` | (必填) | 目标 Team,`slug:SLUG` 或 `id:UUID`,必须已存在 |
| `--include-global-aliases` | 关闭 | 发布全局 Alias(默认跳过) |
| `--literal-namespace` | 无 | literal namespace 映射,可重复,同一 SOURCE 只能出现一次 |
| `--conflict-policy` | `fail` | `fail` 或 `skip-identical` |
| `--apply` | 关闭 | 不加时只做 dry-run |
| `--format` | `table` | `table` 或 `json` |

导入总是先完整 `verify` Bundle,再生成计划;有任何冲突时不执行写入,退出码
`2`。apply 时先发布对象并逐个复核(File/S3 复核对象版本身份,Mooncake 只复核
完成 key 仍存在),最后才提交 Catalog 事务。

### version — 显示版本

```text
version
```

## 六、选择参数(list 与 export 共用)

| 参数 | 说明 |
| --- | --- |
| `--template-id ID` | 按 Template ID 选择,可重复 |
| `--name NAME` | 按完整 Alias 名选择,可重复 |
| `--name-glob GLOB` | 按 Alias glob 模式选择,可重复 |
| `--all` | 选择全部 Template |
| `--source-team slug:SLUG` 或 `id:UUID` | 只选择某个源 Team |
| `--tag TAG` | 选择指定 Tag,可重复;默认 `default` |
| `--all-tags` | 选择全部 Tag |
| `--build-id UUID` | 精确选择 ready Build,可重复 |
| `--build-scope latest\|all` | 每个 Tag 取最新 ready Build(默认)或全部历史 |

Alias 名的写法:Team/literal Alias 用 `NAMESPACE/ALIAS`,全局 Alias 直接用
`ALIAS`。glob 同时匹配完整名和不带 Namespace 的短名。

互斥与校验规则:

- `--all` 与 `--template-id`/`--name`/`--name-glob` 互斥;
- `--all-tags` 与 `--tag` 互斥;
- `--build-id` 与 `--tag`/`--all-tags`/`--build-scope all` 互斥;
- 不传 `--tag`/`--all-tags`/`--build-id` 时默认选择 Tag `default`——环境若使用
  其他 Tag 必须显式传 `--tag`;
- **每个显式选择器都必须命中至少一个可迁移 Template**,否则整个命令报错;
  拼写错误不会被静默降级成部分成功;
- 同一 Template+Tag 有两个创建时间完全相同的 ready Assignment 时,`latest`
  无法判定先后,会要求改用 `--build-id` 精确指定。

## 七、导入语义

### 记录转换

- Template ID、Build UUID、时间戳等跨环境属性保持不变;
- Template/Build 的 Team 与 Cluster 绑定到 `--target-team` 指定的 Team;
- 历史 `cluster_node_id`(Build Node)清空;源端 `created_by` 不复制;
- Alias 和 Assignment 在目标端生成新 UUID。

### Alias Namespace 处理

| Bundle 中的类型 | 导入行为 |
| --- | --- |
| Team-scoped | 自动改写为目标 Team slug,无需参数 |
| Global(无 Namespace) | 默认跳过;`--include-global-aliases` 显式包含 |
| Literal | 必须 `--literal-namespace SOURCE=TARGET` 显式映射,否则报错 |

### 冲突策略

- `fail`(默认):目标端存在同 ID Template/Build、同名 Alias、重复 Assignment
  或同 key 对象时全部记为冲突,退出码 `2`,不写入任何数据;
- `skip-identical`:仅当目标端已有记录/对象**在 Target Team 与 Namespace 转换
  之后逐字段(对象为逐字节 SHA-256)完全一致**时才复用;任何差异仍是冲突。

注意:向**源环境**回导时,即使数据未变,Build 也会因为导入清空了
`cluster_node_id` 而与源记录不同,`skip-identical` 仍报告冲突——这是记录的
迁移语义,不是错误。Mooncake 的已有完成 key 一律按冲突处理(见下节)。

## 八、导入到 Mooncake

前提:使用 `template-migrate-mooncake` 制品。**连接类**变量(master、metadata、
hostname、protocol、device)与目标 E2B Job 一致(`env | sort | grep '^MOONCAKE_'`
检查);**容量类**变量按迁移器自身角色设置——迁移器是纯客户端,不贡献内存段,
`MOONCAKE_GLOBAL_SEGMENT_SIZE` 应设为 `0`,`MOONCAKE_MOUNT_SEGMENT_SIZE` 保持
未设置。工具在进入 native 调用前会对 master 和 metadata 两个端点各做一次 3 秒
TCP 预检,地址错误会快速失败。

| 环境变量 | 未设置时的默认值 | 说明 |
| --- | --- | --- |
| `MOONCAKE_MASTER_ADDR` | `localhost:50051` | master RPC 地址(HOST:PORT) |
| `MOONCAKE_METADATA_SERVER` | `http://localhost:8080/metadata` | metadata 服务 URL |
| `MOONCAKE_LOCAL_HOSTNAME` | `localhost` | 本机对外公布地址 |
| `MOONCAKE_PROTOCOL` | `tcp` | 传输协议 |
| `MOONCAKE_DEVICE_NAME` | 空 | RDMA 设备名(TCP 留空) |
| `MOONCAKE_GLOBAL_SEGMENT_SIZE` | `1073741824`(1 GiB) | 迁移器应显式设为 `0` |
| `MOONCAKE_LOCAL_BUFFER_SIZE` | `134217728`(128 MiB) | 本地缓冲区 |
| `MOONCAKE_MOUNT_SEGMENT_SIZE` | 未设置 | 保持未设置 |

数值变量只接受**十进制字节数**(如 `1073741824`);`1 GiB`、`128 MiB` 这类写法
无法解析,命令会直接报错。

对象布局与 E2B 的 Mooncake 读取端完全一致:header/snapfile/metadata 等 Blob
写单 key;memfile/rootfs 按 4 MiB 分块写 `KEY#c#OFFSET`,最后写逻辑 metadata
key 作为完整性提交点。E2B 侧把 `TEMPLATE_BUCKET_NAME` 指到导入所用的
namespace 即可读取迁移结果。

约束:

- Mooncake **只能作导入目标**,不支持导出;
- namespace 中已有逻辑完成 key 时一律报冲突,`skip-identical` 不适用——重跑
  测试要换新 namespace;
- dry-run 与 apply 结束后 native 客户端都会显式关闭,可对同一 master 连续执行。

## 九、退出码

| 退出码 | 含义 |
| --- | --- |
| `0` | 成功(包括无冲突的 dry-run) |
| `1` | 参数、连接、读取、校验或写入错误 |
| `2` | dry-run 或导入发现冲突 |

## 十、常见错误与处理

| 错误信息 | 原因与处理 |
| --- | --- |
| `endpoint is required` | 缺 `--catalog`/`--store`,补参数或设置对应 `TM_*` 环境变量 |
| `lookup DB_HOST ... no such host` | 连接串仍是占位符,换成真实值 |
| `output ... already exists` | `export --out` 目录已存在,换新目录名 |
| `a template selector is required` | `export` 必须显式给出选择参数(含 `--all`) |
| `did not match an eligible template` | 选择器拼写错误,或该 Template 被 `--source-team` 过滤掉 |
| `template ... has no ready build for tag "default"` | 环境使用其他 Tag,显式传 `--tag`;或该 Template 无 ready Build |
| `legacy_headerless_build` | 源桶中缺该 Build 的 Header 对象;当前版本不支持迁移此类 Build,确认桶/前缀是否正确或改用包含完整对象的存储 |
| `target team ... was not found` | 目标 Catalog 中不存在该 Team,先核对 slug/UUID |
| `literal namespace ... requires an explicit SOURCE=TARGET mapping` | 补 `--literal-namespace` 映射 |
| `Mooncake support is not included in this binary` | 用了普通制品,改用 `template-migrate-mooncake` |
| `connect to MOONCAKE_MASTER_ADDR ...` / metadata 超时 | 确认 `MOONCAKE_*` 变量指向正确的集群且网络可达 |
| 导入返回 `conflicts` | 查看 dry-run 输出的 `conflicts` 明细;Mooncake 测试换新 namespace |
| `target changed after dry-run` | 同一次 apply 从读取目标快照到提交事务的窗口内,目标 PostgreSQL Catalog 出现了并发写入;重试导入。File Catalog 无此检测 |

## 十一、单人串行使用(已知限制)

本工具按单操作者串行使用设计,**不支持并行导入同一目标**:

- File Catalog 的提交是整文件原子替换,两个并行 import 会互相覆盖(后提交者
  丢掉先提交者的记录);PostgreSQL 目标有事务级防护,File 没有。
- Mooncake 写入没有"仅创建"保护,并行导入同一 namespace 可能交错覆盖分片。

同一时间对同一目标只跑一个 import 即可完全规避,无需其他操作。

## 十二、默认行为速查

- 默认 Tag 是 `default`;
- 默认 `--build-scope latest`(每 Tag 只取最新 ready Build);
- 全局 Alias 默认跳过;
- 导入默认 dry-run,必须 `--apply` 才写入;
- 默认冲突策略 `fail`;
- 只迁移 ready Build。

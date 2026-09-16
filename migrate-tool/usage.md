# template-migrate 使用手册

工具版本: `0.3.1`

本手册讲解 `template-migrate` 的完整用法:构建、端点写法、命令参考、导入语义、
Mooncake 配置与常见错误。项目简介与快速开始见 [README.md](README.md)，
机制与数据流见 [设计文档](design.md)，本地 JSON 字段见 [Catalog 格式](docs/catalog.md)。

## 一、工具简介

`template-migrate` 把 E2B Template 的 ready Build 从一个环境迁移到另一个环境:
Template ID 和 Build UUID 保持不变,Team/Cluster 所有权重新绑定到目标 Team,
历史 Build Node 清空。工具**仅支持 Linux**；可迁移 Linux 和 Android 模板。

支持的端点形态(写法见第三节,源端/目标端可任意组合):

| | Catalog | 对象存储 |
| --- | --- | --- |
| 源端 | PostgreSQL 15+ / 本地 JSON 文件 | S3/MinIO / 本地目录 |
| 目标端 | PostgreSQL 15+ / 本地 JSON 文件 | Mooncake / S3/MinIO / 本地目录 |

KASandbox 生产环境的典型路径是 PostgreSQL + S3/MinIO(源)→ PostgreSQL +
Mooncake(目标);本地 JSON Catalog 与本地目录对象存储是同等支持的正式端点,
适用于无 PostgreSQL/对象存储服务的环境、离线交接与本地验证。Mooncake 只能作
导入目标,不支持导出。

迁移范围:

- ✅ 普通 Template、Snapshot Template、ready Build 及其完整对象闭包、
  Build Assignment(Tag 历史)、Alias 及 Namespace 转换
- ❌ Runtime Snapshot、非 ready Build、缺失 Header 的 legacy Build
  (导出时报 `legacy_headerless_build`)、Mooncake 源端导出

Bundle 是一个**目录**,不是 tar/zip 或单个镜像文件。跨机器交接时可打包传输，
到目标机解包后，对 Bundle 目录执行 `verify`，再导入。

### Android 三盘模板

命令参数与 Linux 模板相同，无需添加 Android 开关。工具读取每个 Build 的
`metadata.json` 中 `template.os_type`：缺省或 `linux` 使用单 rootfs；`android`
使用 rootfs.ext4、persistent.img、sdcard.img 三块盘。未知系统明确拒绝。
三块盘的 `.header` 必须齐全，数据文件按 Header 的映射从当前或历史 Build
目录读取；全零区间无需数据对象，完全继承历史层的盘也无需当前 Build 数据文件。
构建机上某份基础 `persistent.img` 的路径和哈希相同，不能替代这些模板对象。

源对象目录示例（各数据文件是否存在由 Header 引用决定）：

```text
<BUILD_ID>/metadata.json
<BUILD_ID>/snapfile
<BUILD_ID>/memfile.header
<BUILD_ID>/rootfs.ext4.header
<BUILD_ID>/persistent.img.header
<BUILD_ID>/sdcard.img.header
<REFERENCED_BUILD_ID>/memfile
<REFERENCED_BUILD_ID>/rootfs.ext4
<REFERENCED_BUILD_ID>/persistent.img
<REFERENCED_BUILD_ID>/sdcard.img
```

纯 Linux 导出仍是 Bundle v1；包含 Android（也可混合 Linux）的导出为 v2。
v2 manifest 中 `build_layouts` 为每个选中 Build 声明 `build_id`、`os_type` 和
有序 `disks`；`inspect` 检查声明，`verify` 再与原始元数据及实际对象依赖核对。
对象字节（包括 vmm_type、android_version 等元数据）保持不变。

**Android 源端至少升级到 v0.2.0，Mooncake 目标端使用 v0.3.1。** 新工具会拒绝
v0.1.x 导出的不完整 Android 包；旧工具不支持 v2。新工具仍支持完整的旧 Linux 包。
目标运行时还需支持源模板的 Android/VMM、CPU 和内核组合；迁移工具不转换镜像或快照。

已有完整目标可用 `--conflict-policy skip-identical`，Mooncake 会读取所有分片并核对
SHA-256。目标已有损坏对象或冲突记录时会报错，需要由环境维护者核对处理；
工具不自动清理、覆盖或重导已有模板。

## 二、从源码构建

模块要求 Go 1.25.4+。以下命令在源码的 `migrate-tool/` 目录运行。
`build.sh` 一次输出一个 `bin/template-migrate`，以 `-tags mooncake` 构建，
同时支持全部正式端点。Mooncake 客户端是 CGO 绑定，必须在具备匹配头文件与
动态库的 Linux/ARM64 主机上原生构建：

```bash
./build.sh
BIN="$PWD/bin/template-migrate"   # 本手册后续示例用 $BIN 指向构建结果
"$BIN" version
```

`build.sh` 只显式链接 libmooncake_store.so 与 libmooncake_common.so(默认
目录 /usr/lib64,可用 `MOONCAKE_LIB_DIR` 覆盖),其余传递依赖由动态链接器
自行解析;头文件只需要 store_c.h(默认目录 /usr/include/mooncake)。如果安装的
软件包未提供该头文件，可指向与动态库版本匹配的 Mooncake 源码树：

```bash
MOONCAKE_INCLUDE_DIR=/path/to/Mooncake/mooncake-store/include ./build.sh
```

Mooncake 构建使用动态链接，运行机器也需具备上述库及其传递依赖；可用
`ldd "$BIN"` 检查是否有缺失库。

`migrate-tool` 是独立 Go 模块,**不在仓库根 `go.work` 的 `use` 列表里**(与
`cri-multiplex` 相同)。仓库内脚本都已 `export GOWORK=off`;直接敲 `go` 命令时
必须自己带上,否则报 `directory prefix . does not contain modules listed in
go.work`。

只使用 PostgreSQL、S3/MinIO 与本地文件端点时，可在 Linux(含 WSL)上不带
`mooncake` tag 构建，直接用于正式迁移。单元测试不依赖外部服务：

```bash
GOWORK=off go build -buildvcs=false -trimpath -o bin/template-migrate ./cmd/template-migrate
BIN="$PWD/bin/template-migrate"
./run-ut.sh
```

不带 tag 的构建收到 `mooncake://` 时会明确报错 `Mooncake support is not
included in this binary`；导入目标为 Mooncake 时，使用 `./build.sh` 构建。

## 三、端点写法

### Catalog 端点

```text
postgresql://USER@DB_HOST:5432/DB_NAME?sslmode=require
/path/to/catalog.json                # 本地 JSON Catalog
file:///path/to/catalog.json         # 同上,URI 写法
```

PostgreSQL 端点对接 KASandbox 部署库;本地 JSON 文件是单文件 Catalog,与
PostgreSQL 遵循同一套数据模型和校验,既可作导出源也可作导入目标(提交为
整文件原子替换)。作导入目标时文件必须已存在且包含目标 Team,最小可用
结构如下(`schema_version` 记录来源；File 适配器不据此判断运行时兼容性):

```json
{
  "schema_version": "20260218120000",
  "teams": [
    { "id": "11111111-1111-1111-1111-111111111111",
      "slug": "target-team", "name": "Target Team" }
  ],
  "templates": [],
  "aliases": [],
  "builds": [],
  "assignments": []
}
```

完整字段、校验规则及源端示例见 [本地 Catalog 格式](docs/catalog.md)。

PostgreSQL 密码**不要写进 URI**(会进入 shell history):用 `~/.pgpass`
(`PGPASSFILE`)、`PGSERVICE` 或 `PGPASSWORD` 环境变量提供。`sslmode` 按目标库
实际配置选择(未启用 TLS 的测试库用 `sslmode=disable`)。

PostgreSQL 适配器连接后会做能力预检,**仅版本达到 15 并不足够**,还要求:
migration baseline `20260218120000`、既定表/列/约束/触发器全部存在
(含 `(alias, namespace)` NULLS NOT DISTINCT 唯一索引和 `status_group` 触发器)、
当前角色对 catalog 表旁路 RLS。任一不满足,命令在预检阶段即拒绝执行。

### 对象存储端点

```text
s3://BUCKET/PREFIX?region=REGION     # 源端/目标端
mooncake://NAMESPACE                 # 目标端,仅 import --store 可用
/path/to/objects                     # 本地目录,源端/目标端
file:///path/to/objects              # 同上,URI 写法
```

本地目录存储按对象 key 组织子目录,作导入目标时目录与子目录会按需自动创建。

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

`mooncake://` 中的 NAMESPACE 是对象在 Mooncake 里的 **key 前缀**:工具把每个
对象写成 `NAMESPACE/<BUILD_ID>/memfile` 这样的 key。它必须与目标 KASandbox
运行时服务(orchestrator 等)配置的 `TEMPLATE_BUCKET_NAME` **取值相同**——运行时
使用 Mooncake 存储时并没有真正的桶,这个变量的值就是它读模板对象时用的 key
前缀,两边不一致运行时就找不到导入的模板。查法:在目标环境的服务进程环境里
`env | grep TEMPLATE_BUCKET_NAME`,或查看服务的 Job / systemd / .env 配置。

namespace 只写名字,不写主机端口;连接参数来自与 E2B Job 一致的 `MOONCAKE_*`
环境变量(见第八节)。

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
#    --literal-namespace SOURCE=TARGET
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

### 本地文件到本地文件

源端已有 `src/catalog.json` 与完整的 `src/objects/`；目标 `dst/catalog.json`
须先包含目标 Team，最小格式见第三节。对象目录由导入按需创建。

```bash
"$BIN" export --catalog ./src/catalog.json --store ./src/objects \
  --all --all-tags --out ./transfer.bundle
"$BIN" verify ./transfer.bundle
"$BIN" import ./transfer.bundle --catalog ./dst/catalog.json \
  --store ./dst/objects --target-team slug:target-team
# 确认计划无冲突后执行
"$BIN" import ./transfer.bundle --catalog ./dst/catalog.json \
  --store ./dst/objects --target-team slug:target-team --apply
```

导入后，目标 JSON Catalog 原子替换为包含新记录的完整文件；对象保持相同逻辑
路径写入 `dst/objects/`。增量 Build 引用的历史数据也必须在源目录中，不能只复制
当前 Build 的一个目录；目录布局见 [Catalog 格式](docs/catalog.md)。

### 本地存储的 KASandbox 到 Mooncake

源端使用 PostgreSQL 和本地模板目录时，导出的 `--store` 填源服务器上直接包含
`<BUILD_ID>/memfile.header` 等路径的对象根目录。导入的 `--store` 再指定
Mooncake namespace。PostgreSQL 凭证通过 `~/.pgpass` 等方式提供，不写进连接串。

```bash
# 源服务器：$BIN 可使用普通构建
"$BIN" export \
  --catalog 'postgresql://USER@SRC_HOST:5432/DB_NAME' \
  --store /path/to/source/templates \
  --template-id TEMPLATE_ID --out ./transfer.bundle
"$BIN" verify ./transfer.bundle

# 将整个 Bundle 目录传到目标服务器；目标 $BIN 必须含 Mooncake 支持
# 先按第八节配置 MOONCAKE_*，namespace 填目标运行时 TEMPLATE_BUCKET_NAME 的值
"$BIN" verify ./transfer.bundle
"$BIN" import ./transfer.bundle \
  --catalog 'postgresql://USER@DST_HOST:5432/DB_NAME' \
  --store 'mooncake://TARGET_NAMESPACE' --target-team slug:TARGET_TEAM
# 确认计划无冲突后，重跑同一条 import 命令并追加 --apply
```

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

**一次导出可以携带多个 Template**:选择参数均可重复,也可用 glob 或 `--all`
批量选择;所有选中的 Template 及其 ready Build 打进同一个 Bundle:

```bash
# 按 ID 逐个点名
"$BIN" export --template-id tpl-python --template-id tpl-node --out "$BUNDLE"

# 按 Alias 名/通配批量选择
"$BIN" export --name builder/python --name-glob 'builder/data-*' --out "$BUNDLE"

# 全量导出
"$BIN" export --all --all-tags --out "$BUNDLE"
```

`import` 一次导入整个 Bundle,Bundle 里有多少 Template 就迁移多少,无需逐个
执行。

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

导入总是先完整 `verify` Bundle,再生成计划;计划阶段发现冲突时不执行写入,退出码
`2`。apply 时先发布对象，再复核：File/S3 复核对象版本身份；Mooncake 先关闭
写入客户端，再新建读取客户端，全量读取 Blob 和每个分片并核对 SHA-256。
全部通过后才提交 Catalog。缺失分片、短读、摘要不一致都不会报告导入成功。
apply 中途出错返回退出码 `1`；已写入的对象不会自动回滚，可能留下未被 Catalog
引用的对象。处理原因后重跑，完整一致的对象可用 `skip-identical` 复用。

### version — 显示版本

```text
version
```

## 六、选择参数(list 与 export 共用)

选择器可以重复与组合,结果取并集:一次命令即可选中多个 Template(多选导出
示例见第五节 `export`)。

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

- Template ID、Build UUID、资源规格及版本信息保留源值；记录时间保留，
  PostgreSQL 以微秒精度存储；
- Template/Build 的 Team 绑定到 `--target-team` 指定的 Team，Template 的
  Cluster 取目标 Team 的 Cluster；
- 历史 `cluster_node_id`(Build Node)清空;源端 `created_by` 不复制;
- 新建 Build 的 `legacy_template_id` 取本包关联该 Build 的最小 Template ID，
  真实关系以 Assignment 为准；
- 新建 Assignment 的 `source` 固定为 `app`；
- 新建 Alias 和 Assignment 时生成新 UUID，复用时保留目标已有 ID。

### Alias Namespace 处理

| Bundle 中的类型 | 导入行为 |
| --- | --- |
| Team-scoped | 自动改写为目标 Team slug,无需参数 |
| Global(无 Namespace) | 默认跳过;`--include-global-aliases` 显式包含 |
| Literal | 必须 `--literal-namespace SOURCE=TARGET` 显式映射,否则报错 |

### 冲突策略

- `fail`(默认):目标端存在同 ID Template/Build、同名 Alias、重复 Assignment
  或同 key 对象时在计划阶段记为冲突，退出码 `2`，不进入写入阶段；
- `skip-identical`：按上述字段与 Namespace 转换后的结果比较记录，对象核对
  完整内容的大小与 SHA-256。记录比较会折叠时间等后端表示差异，Build 的兼容
  `legacy_template_id` 不参与比较；详细规则见 [设计文档](design.md) 第 7.1 节。
  复用已有内容不会新增记录或对象，但 File Catalog 仍会整文件替换。

向**源环境**回导时，即使对象未变，记录也可能因上述转换而不同。例如源 Build
的 `cluster_node_id` 非空时，清空后的记录就不能复用。Mooncake 的缺失或损坏
分片仍是冲突，不能用 `skip-identical` 跳过。

## 八、导入到 Mooncake

前提:使用含 Mooncake 支持的二进制。**连接类**变量(master、metadata、
hostname、protocol、device)与目标 E2B Job 一致(`env | sort | grep '^MOONCAKE_'`
检查)。迁移器固定为纯客户端：SDK Setup 的存储容量始终为 `0`，不调用
`InitAll` 挂载存储段；忽略 `MOONCAKE_GLOBAL_SEGMENT_SIZE` 和
`MOONCAKE_MOUNT_SEGMENT_SIZE`，即使继承旧服务环境也不会贡献存储内存。
无需再手工 export/unset 这两个变量。工具在进入 native 调用前分别预检 master
和 metadata：每个地址的 TCP 连接超时为 3 秒；列表按顺序尝试，各列表至少一个
地址可达才通过，总耗时可能超过 3 秒。

| 环境变量 | 未设置时的默认值 | 说明 |
| --- | --- | --- |
| `MOONCAKE_MASTER_ADDR` | `localhost:50051` | master 地址:单机 `HOST:PORT`;高可用部署写 etcd 集群 `etcd://HOST:PORT;HOST:PORT`,由客户端经 etcd 发现 master |
| `MOONCAKE_METADATA_SERVER` | `http://localhost:8080/metadata` | metadata 服务:HTTP URL,或 etcd 列表 `etcd://HOST:PORT;HOST:PORT` |
| `MOONCAKE_LOCAL_HOSTNAME` | `localhost` | 本机对外公布地址 |
| `MOONCAKE_PROTOCOL` | `tcp` | 传输协议 |
| `MOONCAKE_DEVICE_NAME` | 空 | RDMA 设备名(TCP 留空) |
| `MOONCAKE_GLOBAL_SEGMENT_SIZE` | 固定 `0` | 环境变量被忽略 |
| `MOONCAKE_LOCAL_BUFFER_SIZE` | `134217728`(128 MiB) | 本地缓冲区 |
| `MOONCAKE_MOUNT_SEGMENT_SIZE` | 不挂载 | 环境变量被忽略，不调用 `InitAll` |

`MOONCAKE_LOCAL_BUFFER_SIZE` 只接受正数的**十进制字节数**(如 `134217728`);`1 GiB`、`128 MiB` 这类写法
无法解析,命令会直接报错。

高可用部署的 etcd 地址支持分号或逗号分隔，例如：

```bash
export MOONCAKE_MASTER_ADDR='etcd://ETCD_A:2379;ETCD_B:2379;ETCD_C:2379'
export MOONCAKE_METADATA_SERVER='etcd://ETCD_A:2379;ETCD_B:2379;ETCD_C:2379'
```

两项分别使用目标环境的实际配置，不要求它们是同一个集群。整串地址原样交给
Mooncake SDK，服务发现由 SDK 处理。TCP 预检通过只表示至少一个地址可连接，
不代表 etcd 认证、Master 发现、故障切换或数据传输已经成功。

对象布局与 E2B 的 Mooncake 读取端完全一致:header/snapfile/metadata 等 Blob
写单 key;memfile 和 rootfs/persistent/sdcard 数据按 4 MiB 分块写 `KEY#c#OFFSET`,最后写逻辑 metadata
key 作为完整性提交点。所有 key 都带 `NAMESPACE/` 前缀,运行时按
`TEMPLATE_BUCKET_NAME/<BUILD_ID>/...` 读取,因此导入用的 namespace 必须与目标
运行时的 `TEMPLATE_BUCKET_NAME` 一致(见第三节)。

约束:

- Mooncake **只能作导入目标**,不支持导出;
- 已有对象只有全量读取并核对 SHA-256 一致后才能被 `skip-identical` 复用；
- apply 关闭写入连接后用新客户端再次全量校验，再提交 Catalog。新读取客户端与
  写入客户端在同一进程中，二者都不贡献存储容量；这不等于真实 Sandbox 启动验收；
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
| `mooncake:// is only supported as the import target` | 把 Mooncake 填成了 `export --store`;导出的 `--store` 是**源端**存储(S3/MinIO 或源服务器上的模板目录),Mooncake 只在导入时作 `--store` |
| `lookup DB_HOST ... no such host` | 连接串仍是占位符,换成真实值 |
| `output ... already exists` | `export --out` 目录已存在,换新目录名 |
| `a template selector is required` | `export` 必须显式给出选择参数(含 `--all`) |
| `did not match an eligible template` | 选择器拼写错误,或该 Template 被 `--source-team` 过滤掉 |
| `template ... has no ready build for tag "default"` | 环境使用其他 Tag,显式传 `--tag`;或该 Template 无 ready Build |
| `legacy_headerless_build` | 源桶中缺该 Build 的 Header 对象;当前版本不支持迁移此类 Build,确认桶/前缀是否正确或改用包含完整对象的存储 |
| `target team ... was not found` | 目标 Catalog 中不存在该 Team,先核对 slug/UUID |
| `literal namespace ... requires an explicit SOURCE=TARGET mapping` | 补 `--literal-namespace` 映射 |
| `Mooncake support is not included in this binary` | 当前构建不含 Mooncake；使用 `./build.sh` 构建 |
| `connect to MOONCAKE_MASTER_ADDR ...` / metadata 超时 | 确认 `MOONCAKE_*` 变量指向正确的集群且网络可达 |
| 导入返回 `conflicts` | 查看计划明细；只有内容完整一致时才使用 `skip-identical`。损坏或不同内容由环境维护者核对处理，生产 namespace 仍须与运行时配置一致 |
| `target changed after dry-run` | 同一次 apply 从读取目标快照到提交事务的窗口内,目标 PostgreSQL Catalog 出现了并发写入;重试导入。File Catalog 无此检测 |

## 十一、单人串行使用(已知限制)

本工具按单操作者串行使用设计,**不支持并行导入同一目标**:

- File Catalog 的提交是整文件原子替换,两个并行 import 会互相覆盖(后提交者
  丢掉先提交者的记录);PostgreSQL 目标有事务级防护,File 没有。
- Mooncake 写入没有"仅创建"保护,并行导入同一 namespace 可能交错覆盖分片。

同一时间对同一目标只运行一个 import，并避免其他进程同时修改本次迁移涉及的
对象或 Catalog 记录。PostgreSQL 的提交事务不覆盖整个导入过程，也不是完整的
并发变更检测；仅把 import 串行执行不能防止外部写入者造成的干扰。

## 十二、默认行为速查

- 默认 Tag 是 `default`;
- 默认 `--build-scope latest`(每 Tag 只取最新 ready Build);
- 全局 Alias 默认跳过;
- 导入默认 dry-run,必须 `--apply` 才写入;
- 默认冲突策略 `fail`;
- 只迁移 ready Build。

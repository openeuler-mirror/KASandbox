# E2B 模板迁移工具(template-migrate)

`template-migrate` 是一个独立的 Go CLI,用于在不同 E2B 部署之间迁移模板
(Template)的 ready Build:导出为离线 Bundle,校验后导入目标环境。迁移中
Template ID 与 Build UUID 保持不变,Team/Cluster 所有权重新绑定到目标 Team。
工具版本 **0.3.1**，运行平台仅支持 Linux(含 WSL)，可迁移 Linux 和 Android 模板。

支持的端点(源端/目标端可任意组合,写法见 [使用手册](usage.md) 第三节):

| | Catalog(元数据) | 对象存储(制品) |
| --- | --- | --- |
| 源端 | PostgreSQL 15+ / 本地 JSON 文件 | S3/MinIO / 本地目录 |
| 目标端 | PostgreSQL 15+ / 本地 JSON 文件 | **Mooncake** / S3/MinIO / 本地目录 |

KASandbox 生产环境的典型路径是 PostgreSQL + S3/MinIO(源)→ PostgreSQL +
Mooncake(目标);本地 JSON Catalog 与本地目录对象存储是同等支持的正式端点,
适用于无 PostgreSQL/对象存储服务的环境、离线交接与本地验证。

迁移范围:

- ✅ 普通 Template、Snapshot Template、ready Build 及其完整对象闭包
  (内存、各盘数据及 Header、snapfile、metadata)、Tag 历史、Alias 及 Namespace 转换
- ✅ Android 的 rootfs.ext4、persistent.img、sdcard.img 三盘，按各自 Header 收集历史 Build 数据
- ❌ Runtime Snapshot、非 ready Build、缺失 Header 的 legacy Build、Mooncake 源端导出

## 构建

模块要求 Go 1.25.4+。`build.sh` 输出一个含 Mooncake 的二进制，以 `-tags mooncake` 构建；Mooncake
客户端是 CGO 绑定,必须在已安装 Mooncake 头文件与动态库的 Linux/ARM64 主机上
原生构建:

```bash
./build.sh
# 输出: bin/template-migrate
```

`build.sh` 只显式链接 libmooncake_store.so 与 libmooncake_common.so(默认在
/usr/lib64),头文件只需要 store_c.h(默认在 /usr/include/mooncake)。如果安装的
软件包未提供该头文件，可用 `MOONCAKE_INCLUDE_DIR` 指向与动态库版本匹配的
Mooncake 源码树的 mooncake-store/include；库目录可用 `MOONCAKE_LIB_DIR` 覆盖。

> `migrate-tool` 是独立 Go 模块,**不在仓库根 `go.work` 的 `use` 列表里**
> (与 `cri-multiplex` 相同)。仓库内的脚本都已 `export GOWORK=off`;手工执行
> `go` 命令时也要带上,否则会报
> `directory prefix . does not contain modules listed in go.work`:
>
> ```bash
> GOWORK=off go build ./cmd/template-migrate
> ```

在 Linux 上可不带 `mooncake` tag 构建(Mooncake 支持编译为 stub,收到
`mooncake://` 端点时显式报错),单元测试不依赖 Mooncake 库。该构建除
Mooncake 外功能完整:只使用 PostgreSQL、S3/MinIO 与本地文件端点时,可直接
用于正式迁移。

## 快速开始

标准迁移流程五步:

```bash
template-migrate list   --catalog SOURCE_CATALOG --all --all-tags
template-migrate export --catalog SOURCE_CATALOG --store SOURCE_STORE \
    --template-id TEMPLATE_ID --out ./run/my-export.bundle
template-migrate verify ./run/my-export.bundle
template-migrate import ./run/my-export.bundle \
    --catalog TARGET_CATALOG --store TARGET_STORE --target-team slug:TEAM
template-migrate import ./run/my-export.bundle \
    --catalog TARGET_CATALOG --store TARGET_STORE --target-team slug:TEAM --apply
```

导入默认是 dry-run(只出计划不写数据),确认无冲突后追加 `--apply` 执行。
选择参数可重复,一个 Bundle 可携带多个 Template,`import` 一次导入整个 Bundle。

根据每个 Build 的 `metadata.json` 中 `template.os_type` 自动选择磁盘；缺省或
`linux` 为 Linux，`android` 要求三盘 Header 齐全，其他系统明确报不支持。
纯 Linux 导出保持 Bundle v1；包含 Android 的导出使用 Bundle v2，新工具读取两者。
v0.1.x 导出的 Android 包缺盘，必须用新版从源端重新导出；只升级导入端不能补齐。
Android 源端至少使用 v0.2.0，Mooncake 目标端使用 v0.3.1。
Mooncake 的 `skip-identical` 会完整读取并核对 SHA-256；缺失分片或内容不一致仍报冲突。
迁移客户端固定 0 存储容量、不挂载存储段，无需设置两个 segment 环境变量。
Mooncake 导入在关闭写入客户端后完整读回校验，通过后才提交 Catalog。
工具不提供清理重导命令；目标已有损坏对象或冲突记录时，由环境维护者核对处理。
迁移保留镜像、快照及版本元数据；目标运行时仍须支持源模板的 CPU、VMM、
内核及 Android 版本组合，Bundle v1/v2 兼容不代表任意平台版本互迁。

## 安全设计

- 连接凭证请通过驱动提供的渠道配置：PostgreSQL 使用 `PGPASSWORD`/`~/.pgpass`，
  S3/MinIO 使用标准 AWS SDK 凭证链；不要把密码写进 URI 或保存到迁移文件中。
- 导入前自动检查迁移包的内容完整性与依赖齐全性，包括对象大小、SHA-256 和
  Header 引用的数据；
- 计划阶段发现冲突时停止，不写目标数据(退出码 `2`)。apply 中途失败时可能留下
  未被 Catalog 引用的对象；跨存储提交顺序见 [design.md](design.md) 第 7.2 节。

## 已知限制

按单操作者串行使用设计:不要并行导入同一目标(File Catalog 为整文件原子替换,
Mooncake 写入无仅创建保护)。详见 [usage.md](usage.md) 第十一节。

## 文档

- **[usage.md](usage.md)**:完整的命令参考、参数说明、端点写法、Mooncake
  配置与故障排查;
- **[design.md](design.md)**:代码架构、功能清单、导入导出时序图与关键
  设计决策(对象闭包、幂等语义、与运行时对齐的常量)。
- **[本地 Catalog 格式](docs/catalog.md)**:JSON 字段、目标 Team 最小示例、
  源端关系数据及配套对象目录。

## 测试

```bash
./run-ut.sh              # 全部单元测试
./run-ut.sh -p importer  # 只跑 internal/importer
./run-ut.sh --no-race    # 无 C 工具链的环境
./run-ut.sh --cover      # 附带覆盖率
```

单元测试在 Linux(含 WSL)上运行,不依赖任何外部服务;测试数据(Catalog、
对象、golden Header)全部由 `internal/testfixture` 在运行时生成,仓库不提交
二进制 fixture。PostgreSQL Catalog 的集成测试在设置了 `TM_TEST_POSTGRES_DSN`
(指向一次性测试库)时才运行,否则自动跳过。所有 `.sh` 脚本必须保持 LF 行尾
(测试会强制检查)。

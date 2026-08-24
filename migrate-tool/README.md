# E2B 模板迁移工具(template-migrate)

`template-migrate` 是一个独立的 Go CLI,用于在不同 E2B 部署之间迁移模板
(Template)的 ready Build:导出为离线 Bundle,校验后导入目标环境。迁移中
Template ID 与 Build UUID 保持不变,Team/Cluster 所有权重新绑定到目标 Team。
工具仅支持 Linux(与运行时一致)。

生产迁移路径:

| | 源端 | 目标端 |
| --- | --- | --- |
| Catalog(元数据) | PostgreSQL 15+ | PostgreSQL 15+ |
| 对象存储(制品) | S3/MinIO | **Mooncake** |

(本地 JSON Catalog 与本地目录对象存储作为测试/调试设施保留,见 usage.md。)

迁移范围:

- ✅ 普通 Template、Snapshot Template、ready Build 及其完整对象闭包
  (memfile/rootfs/header/snapfile/metadata)、Tag 历史、Alias 及 Namespace 转换
- ❌ Runtime Snapshot、非 ready Build、缺失 Header 的 legacy Build、Mooncake 源端导出

## 构建

模块要求 Go 1.25.4+。发布制品只有一个,以 `-tags mooncake` 构建;Mooncake
客户端是 CGO 绑定,必须在已安装 Mooncake 头文件与动态库的 Linux/ARM64 主机上
原生构建:

```bash
./build.sh
# 输出: bin/template-migrate
```

> `migrate-tool` 是独立 Go 模块,**不在仓库根 `go.work` 的 `use` 列表里**
> (与 `cri-multiplex` 相同)。仓库内的脚本都已 `export GOWORK=off`;手工执行
> `go` 命令时也要带上,否则会报
> `directory prefix . does not contain modules listed in go.work`:
>
> ```bash
> GOWORK=off go build ./cmd/template-migrate
> ```

开发自测可在任意 Linux 上不带 `mooncake` tag 构建(Mooncake 支持编译为
stub,收到 `mooncake://` 端点时显式报错),单元测试不依赖 Mooncake 库。

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

## 安全设计

- 凭证不进入 URI、Bundle、报告与命令历史:PostgreSQL 密码走
  `PGPASSWORD`/`~/.pgpass`,S3/MinIO 走标准 AWS SDK 凭证链;
- 导入前强制完整校验(对象大小、SHA-256、Header 依赖闭包);
- 有任何冲突时不写入任何数据(退出码 `2`)。

## 已知限制

按单操作者串行使用设计:不要并行导入同一目标(File Catalog 为整文件原子替换,
Mooncake 写入无仅创建保护)。详见 [usage.md](usage.md) 第十一节。

## 文档

完整的命令参考、参数说明、端点写法、Mooncake 配置与故障排查见
**[usage.md](usage.md)**。

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

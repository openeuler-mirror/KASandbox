# 本地 JSON Catalog 编写说明

> 面向 template-migrate 使用方。说明 `--catalog /path/to/catalog.json`
> 这种本地文件 Catalog 的格式、校验规则、与之配套的对象目录布局,并给出
> 可直接套用的完整示例。命令用法见 [使用手册](../usage.md)，项目入口见
> [README](../README.md)。本文与工具 v0.3.1 对齐。

## 一、先分清两种用途

| 用途 | 需要写什么 | 示例 |
| --- | --- | --- |
| **作导入目标**(`import --catalog dst/catalog.json`) | 只需目标 Team;其余数组留空,导入时由工具追加 | 第二节 |
| **作导出源**(`export --catalog src/catalog.json --store src/objects`) | 完整描述 Team、Template、Alias、Build、Assignment,并配套对象目录 | 第七节 |

写完先用 `list` 检查 JSON 类型、引用和唯一性——它只读 Catalog、不碰对象存储。
能被加载不等于资源规格、版本信息和对象内容已经验证，完整检查范围见第五节：

```bash
template-migrate list --catalog ./catalog.json --all --all-tags
```

## 二、作导入目标:最小结构

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

- 文件必须已存在;`--target-team slug:target-team` 或 `id:<UUID>` 必须能在
  teams 里找到;
- `schema_version` 记录来源，本地 Catalog 不按这个值进行数据库能力预检，
  也不据此确认目标运行时兼容；这不表示支持任意平台版本互迁；
- 导入成功后文件被整体原子替换,templates/builds 等数组由工具填入。

## 三、总体格式约定

- 单个 JSON 文档，支持的顶层键为 `schema_version`、`teams`、`templates`、
  `aliases`、`builds`、`assignments`、`snapshot_templates`；无记录的数组可为空；
- **不允许出现未知字段**(多写一个字段就会 `unknown field` 报错),字段名
  全部小写下划线;
- 时间一律 RFC 3339 字符串,如 `"2026-09-01T10:00:00Z"`;
- ID 是任意非空字符串,建议与源环境保持一致:Team/Build/Alias/Assignment
  用 UUID,Template ID 用源环境的模板 ID(E2B 里是短字母数字串);
  **Template ID 与 Build ID 迁移后保持不变**,是重复导入判定"相同/冲突"
  的依据；Catalog 加载允许非空字符串，但实际 Build ID 必须与二进制 Header
  中的 UUID 一致，接入 PostgreSQL 时也需满足对应列类型；
- 表中的“必填”表示为实际迁移准备数据时应提供的字段，不等于加载器逐项检查
  JSON 键是否存在。缺省字符串、数值、布尔和时间会解码为 Go 零值，只有违反
  第五节规则的值才会被拒绝；请按源端真实数据填写，不能把加载成功作为字段齐全的证明。

## 四、各实体字段

### teams

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| id | 是 | 唯一 |
| slug | 是 | 唯一;Team 级 Alias 的 namespace 通常就是它 |
| name | 是 | 显示名 |
| cluster_id | 否 | 目标 Team 所在 Cluster;导入时模板绑定到目标 Team 的这个值 |

### templates

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| id | 是 | 唯一 |
| created_at / updated_at | 是 | 时间 |
| public | 是 | true/false |
| team_id | 是 | 必须是 teams 里存在的 id |
| source | 是 | `"template"` 或 `"snapshot_template"`;后者必须在 snapshot_templates 里有且只有一条对应记录 |
| created_by | 否 | 源端用户 ID,导入时不复制 |
| cluster_id | 否 | 导入时改绑目标 Team 的 Cluster |
| build_count / spawn_count / last_spawned_at | 否 | 统计信息 |

### aliases

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| id | 是 | 唯一 |
| template_id | 是 | 必须存在 |
| namespace | 否 | 不写 = 全局 Alias(导入默认跳过);写 Team slug = Team 级 Alias(导入自动改成目标 Team slug);写其他值 = literal namespace,导入时必须 `--literal-namespace SRC=DST` 显式映射 |
| alias | 是 | 名称;`(namespace, alias)` 组合必须唯一 |
| is_renamable | 是 | true/false |

`list`/`export` 里按名称选择时,Team 级/literal Alias 写 `NAMESPACE/ALIAS`,
全局 Alias 直接写 `ALIAS`。

### builds

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| id | 是 | 唯一;同时是对象目录里的子目录名 |
| created_at / updated_at | 是 | 时间 |
| finished_at | 否 | 时间 |
| status | 是 | 原始状态字符串 |
| status_group | 是 | 必须等于 status 按下表映射出的分组;**只有 `ready` 分组的 Build 会被迁移** |
| vcpu / ram_mb / free_disk_size_mb | 是 | 整数 |
| total_disk_size_mb | 否 | 整数 |
| kernel_version / firecracker_version | 是 | 填源 Build 的真实版本，需与对象及目标运行时匹配；工具不对这些版本做跨来源核对 |
| team_id | 是 | 可填 `""`(历史数据容忍);非空时必须存在。导入时统一重绑,不进 Bundle |
| dockerfile / start_command / ready_command | 否 | 字符串 |
| legacy_template_id / envd_version / version | 否 | 字符串；导入新建 Build 时 legacy_template_id 根据 Assignment 重设，其余保留 |
| cluster_node_id | 否 | 源端构建节点,导入时清空 |
| reason | 否 | 任意 JSON 对象 |
| cpu_architecture / cpu_family / cpu_model / cpu_model_name | 否 | 字符串 |
| cpu_flags | 否 | 字符串数组 |

status → status_group 映射(与数据库触发器一致):

| status | status_group |
| --- | --- |
| pending, waiting | pending |
| in_progress, building, snapshotting | in_progress |
| ready, uploaded, success | **ready** |
| 其他任何值 | failed |

### assignments

Assignment 表示"Template X 的 Tag T 指向 Build Y",是 Template 与 Build 的
真正关联;一个 Build 没有 Assignment 就不会被任何选择命中。

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| id | 是 | 唯一 |
| template_id | 是 | 必须存在 |
| build_id | 是 | 必须存在 |
| tag | 是 | 非空;工具默认只选 `default`,其他 Tag 要显式 `--tag` |
| source | 是 | 非空,填源端来源，如 `"build"`；导入新建时统一写为 `"app"` |
| created_at | 是 | 同一 Template+Tag 有多条时,`--build-scope latest` 取 created_at 最新的一条;两条时间完全相同会拒绝判定 |

### snapshot_templates(可选)

只有 `source = "snapshot_template"` 的 Template 才能、也必须有一条:

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| template_id | 是 | 指向 source 为 snapshot_template 的 Template,唯一 |
| sandbox_id | 是 | 非空 |
| created_at | 是 | 时间 |

## 五、加载时的校验规则

以下任一不满足,`list`/`export`/`import` 都会在读入口直接失败并指出条目:

- Team:id、slug 非空,各自唯一;
- Template:id 唯一,source 只能是两个合法值,team_id 必须存在;
- Alias:id 唯一,alias 非空,namespace 若写则非空,template_id 必须存在,
  `(namespace, alias)` 唯一;
- Build:id 唯一,team_id 非空时必须存在,status_group 与 status 映射一致;
- Assignment:id 唯一,template_id/build_id 必须存在,tag、source 非空;
- SnapshotTemplate:只能挂在 snapshot_template 类型的 Template 上,一对一,
  sandbox_id 非空;snapshot_template 类型的 Template 缺这条记录也报错。

加载器不检查显示名是否填写、时间是否为零、资源规格是否合理，也不核对内核/VMM
版本与对象内容。`list` 还会按选择器检查可迁移记录；`export` 才会读取对象，
检查 metadata 的 Build ID/OS、Header 和数据闭包。目标运行时能否启动模板仍需另行验证。

## 六、配套的对象目录(`--store /path/to/objects`)

对象按 Build ID 分子目录,文件名固定,**与 KASandbox/E2B 模板存储桶里的
路径一致**。除了选中 Build 的控制对象，还必须提供 Header 引用的全部历史
数据；不能只复制当前 Build 的一个目录：

```text
objects/
└── <BUILD_ID>/
    ├── memfile.header        必需;缺失时报 legacy_headerless_build
    ├── rootfs.ext4.header    必需;同上
    ├── snapfile              必需
    ├── metadata.json         必需;其中 template.build_id 必须等于 <BUILD_ID>
    ├── memfile               按 header 引用复制
    └── rootfs.ext4           按 header 引用复制
```

Android Build 的 `metadata.json` 中 `template.os_type` 为 `android`，除上表外还
必须有 `persistent.img.header` 和 `sdcard.img.header`；各自引用的 `persistent.img`
和 `sdcard.img` 数据层也需完整提供（可能位于历史 Build）。工具会自动识别，
无需在 JSON Catalog 中新增操作系统字段。

要点:

- 这些文件全部是构建流水线的产物,**不能手写**;header 是二进制 v3 格式,
  工具会解析并核对里面的 build id;
- 增量构建的 header 会把部分数据区间指向**其他 Build**(base build)的
  memfile / rootfs.ext4；Android 还包括 persistent.img / sdcard.img，工具解析各自
  header 后会自动去拿这些目录下的数据文件。
  所以把某个 Build 的目录复制过来时,它依赖的历史 Build 目录也要一起复制;
  漏了会在 export 时报错并指出缺少哪个 key(形如 `<BUILD_ID>/memfile`);
- 选中的 ready Build 决定需要读取哪些 Header、metadata 和 snapfile；数据
  对象范围则由 Header 决定。被引用的历史数据 Build 无需另行成为本次选择的
  Template/Assignment 记录；未被选择且未被 Header 引用的目录不会导出。

## 七、作导出源:完整示例

一个 Team、一个普通 Template、一个 Team 级 Alias、一个 ready Build、一条
`default` Tag 的 Assignment。除 ID、时间和资源规格按实际替换外,可直接套用:

```json
{
  "schema_version": "20260218120000",
  "teams": [
    { "id": "aaaaaaaa-0000-0000-0000-000000000001",
      "slug": "platform", "name": "Platform Team" }
  ],
  "templates": [
    { "id": "py3base01",
      "created_at": "2026-08-01T08:00:00Z",
      "updated_at": "2026-08-20T09:30:00Z",
      "public": false,
      "team_id": "aaaaaaaa-0000-0000-0000-000000000001",
      "source": "template" }
  ],
  "aliases": [
    { "id": "bbbbbbbb-0000-0000-0000-000000000001",
      "template_id": "py3base01",
      "namespace": "platform",
      "alias": "python-base",
      "is_renamable": true }
  ],
  "builds": [
    { "id": "cccccccc-0000-0000-0000-000000000001",
      "created_at": "2026-08-20T09:00:00Z",
      "updated_at": "2026-08-20T09:30:00Z",
      "finished_at": "2026-08-20T09:30:00Z",
      "status": "ready",
      "status_group": "ready",
      "vcpu": 2,
      "ram_mb": 1024,
      "free_disk_size_mb": 512,
      "total_disk_size_mb": 2048,
      "kernel_version": "vmlinux-6.1.102",
      "firecracker_version": "v1.10.1",
      "envd_version": "0.2.5",
      "team_id": "aaaaaaaa-0000-0000-0000-000000000001" }
  ],
  "assignments": [
    { "id": "dddddddd-0000-0000-0000-000000000001",
      "template_id": "py3base01",
      "build_id": "cccccccc-0000-0000-0000-000000000001",
      "tag": "default",
      "source": "build",
      "created_at": "2026-08-20T09:30:00Z" }
  ]
}
```

假设这个 Linux Build 的内存和 rootfs 都只引用自身数据，对象目录如下；
若 Header 引用历史 Build，还需追加相应数据对象，全零区间无需数据文件：

```text
objects/cccccccc-0000-0000-0000-000000000001/
    memfile.header  rootfs.ext4.header  snapfile  metadata.json  memfile  rootfs.ext4
```

验证与导出:

```bash
template-migrate list   --catalog ./catalog.json --all --all-tags
template-migrate export --catalog ./catalog.json --store ./objects \
    --name platform/python-base --out ./python-base.bundle
template-migrate verify ./python-base.bundle
```

多个 Template / Build 就往各数组里追加条目;一个 Template 多个 Tag 就多写几条
Assignment。快照类模板把 template.source 写成 `"snapshot_template"`,并在
`snapshot_templates` 里补一条 `{ "template_id", "sandbox_id", "created_at" }`。

## 八、从真实环境取值

源环境如果是带 PostgreSQL 的 KASandbox 部署,**不需要手写 Catalog**,直接
`--catalog postgresql://USER@HOST:5432/DB` 让工具自己读。手写只适用于拿不到
数据库连接、只有一份数据导出的情况,字段与数据库表的对应关系:

| Catalog 数组 | 数据库表 |
| --- | --- |
| teams | teams |
| templates | envs |
| aliases | env_aliases |
| builds | env_builds |
| assignments | env_build_assignments |
| snapshot_templates | snapshot_templates |

## 九、Catalog 相关报错对照

| 报错片段 | 原因 |
| --- | --- |
| `decode catalog ... unknown field "xxx"` | 字段名拼错或多写了不支持的字段 |
| `decode catalog ... parsing time` | 时间不是 RFC 3339 格式 |
| `validate catalog ... references missing team/template/build` | 引用的 id 在对应数组里不存在 |
| `duplicate alias "ns/name" on templates ...` | 同一 namespace 下重名 |
| `status "x" maps to status_group "y", got "z"` | status_group 没按映射表填 |
| `snapshot template "x" has no snapshot row` | source 写了 snapshot_template 却没补 snapshot_templates 记录 |
| `did not match an eligible template` | 选择器写错,或该 Template 的 source 不合法 |
| `has no ready build for tag "default"` | 没有 Assignment 指向 ready Build,或 Tag 不是 default(加 `--tag`) |
| `legacy_headerless_build <id>: missing <id>/memfile.header` | 对象目录缺 header 文件 |
| `metadata build id is "a", expected "b"` | metadata.json 与目录名对应的 Build 不一致 |
| `object "<id>/memfile" ... header mappings require at least N` | 数据文件被截断或复制不完整 |

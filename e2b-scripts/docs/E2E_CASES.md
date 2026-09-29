# E2E 用例清单（116 个）

> 从 README 拆出，与 `e2b_validator/e2e_test_cases.py`、`e2e_extended_cases.py` 保持一致。执行方式见 [README](../README.md#3-功能验收test-e2e)。

下表与 `e2b_validator/e2e_test_cases.py` 保持一致。`<fixture-template>`、`<base-image>` 和 `<run-id>` 在运行时动态生成或解析。

## 创建 Sandbox（10 个）

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

## 创建 Template（8 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `TP-001` | 真实 base image 构建 | 自动发现的 Harbor Ubuntu 镜像；`CPU=1`；`memory=1024 MB` | template-manager 完成构建；失败时可归因到镜像或基础设施 |
| `TP-002` | inline Dockerfile 构建 | `FROM <base-image>`；写入 marker；`memory=512 MB` | Dockerfile content 链路可用，构建名称唯一 |
| `TP-003` | skip-cache 组合 | `skip-cache=true`；`CPU=2`；`memory=1024 MB` | 参数被接收，产生可追踪构建结果 |
| `TP-004` | Dockerfile 不存在 | 指定不存在的本地文件 | 请求发出前返回文件不存在 |
| `TP-005` | CPU 为 0 | `cpu-count=0` | 客户端拒绝非法 CPU 值 |
| `TP-006` | 内存为负数 | `memory-mb=-1` | 客户端拒绝非法内存值 |
| `TP-007` | 无效镜像名称 | `invalid.invalid/<run-id>:missing` | 构建错误可归因；服务端生成的 error Template 按 ID 登记并清理 |
| `TP-008` | 多个构建源冲突 | 同时指定 base image 和 inline Dockerfile | 客户端执行 mutually-exclusive 校验并拒绝请求 |

## 执行命令（14 个）

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

## 上传文件（8 个）

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

## 下载文件（8 个）

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

## 查询 Sandbox（6 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `LSB-001` | 基础查询 | 不限制页数 | 返回合法 JSON |
| `LSB-002` | 创建后可见 | 依赖 `SB-001` | 主测试 Sandbox 出现在查询结果中 |
| `LSB-003` | 单页查询 | `max-pages=1` | 分页限制生效，返回合法 JSON |
| `LSB-004` | 多个本轮 Sandbox 可见 | 查询本轮创建资源 | 本轮资源均可识别 |
| `LSB-005` | metadata 精确查询链路 | 依赖 `SB-002` 的 `run_id` metadata | 指定 Sandbox 的 metadata 可见且匹配 |
| `LSB-006` | 分页值为 0 | `max-pages=0` | 客户端拒绝分页下界非法值 |

## 查询 Template（6 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `LTP-001` | 基础查询 | 不限制页数 | 返回合法 JSON，状态可解释 |
| `LTP-002` | 本轮 fixture 可见 | 匹配 `<fixture-template>` | fixture 出现在查询结果中 |
| `LTP-003` | 单页查询 | `max-pages=1` | 分页限制生效，返回合法 JSON |
| `LTP-004` | 构建后状态查询 | 查询本轮构建的 Template | 构建资源和状态可见 |
| `LTP-005` | 重复查询稳定性 | 连续执行查询 | 返回结构稳定，无随机解析差异 |
| `LTP-006` | 分页值为负数 | `max-pages=-1` | 客户端拒绝负分页值 |

## 扩展 SDK/API 用例（57 个）

扩展用例使用 E2B Python SDK 直接验证 API 与 data plane。所有临时对象名称、文件路径和 tag 都带有本轮 `run_id`；清理只处理账本中登记的本轮资源。

### Background Commands 与 Streaming（8 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `BG-001` | 后台命令等待 | `commands.run(..., background=True)` | 返回 PID；`wait()` 返回完整结果 |
| `BG-002` | 后台进程查询 | 查询已启动 PID | PID 出现在 `commands.list()` 结果中 |
| `BG-003` | 后台命令标准输入 | `send_stdin()` 写入文本 | 后台命令可读到完整输入 |
| `BG-004` | 断开并重新连接 | `disconnect()` 后重新 `connect()` | 进程不中断，重新连接后可等待结果 |
| `BG-005` | CommandHandle 终止 | 对本轮后台 PID 执行 `kill()` | 进程停止，账本记录清理状态 |
| `BG-006` | 不存在的 PID | 终止未创建 PID | 返回 `false` 或明确 not found 响应 |
| `STR-001` | stdout/stderr 流式回调 | 同时写入两个输出流 | 两个 callback 分别收到对应内容 |
| `STR-002` | 流式非零退出 | 输出 stderr 并 `exit 9` | 保留 stderr 与非零退出码 |

### Filesystem、Watcher 与 Signed URL（11 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `FS-001` | 三种读取格式 | `text`、`bytes`、`stream` | 三种读取结果字节一致 |
| `FS-002` | 写入并覆盖 | 连续写入同一路径 | 第二次内容完整覆盖第一次内容 |
| `FS-003` | 批量写入 | `write_files()` 写入两个文件 | 两个目标文件均正确落盘 |
| `FS-004` | 空批量写入 | `write_files([])` | 返回空结果且不产生副作用 |
| `FS-005` | 目录与深度查询 | 创建三层路径；`list(depth=2)` | 返回第二层目录，不越界返回第三层文件 |
| `FS-006` | 文件存在性与信息 | `exists()`、`get_info()` | 状态、路径与类型一致 |
| `FS-007` | 重命名与删除 | `rename()` 后 `remove()` | 旧路径和最终路径均按预期消失 |
| `FS-008` | 深度参数下界 | `list(depth=0)` | 客户端明确拒绝非法深度 |
| `WAT-001` | 目录事件监听 | 创建本轮文件 | watcher 收到创建事件并可停止 |
| `WAT-002` | 递归目录监听 | 在子目录创建文件 | 捕获子目录事件；旧 envd 不支持时为 `BLOCKED` |
| `URL-001` | 文件直连 URL | 获取 upload/download URL | query 解码后包含目标路径、用户、签名和过期时间 |

### Sandbox 与 Lifecycle（4 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `SI-001` | 运行状态与详情 | `is_running()`、`get_info()` | Sandbox 正在运行，详情包含当前 ID |
| `SI-002` | 动态延长 timeout | `set_timeout(900)` | 调用成功且 Sandbox 持续可用 |
| `SI-003` | 按 ID 重新连接 | 新 SDK 对象连接共享 Sandbox | 可执行独立命令 |
| `LC-001` | 手动暂停与恢复 | 对本轮独立 Sandbox 执行 `pause()` 后 `connect()` | 同一 Sandbox 可恢复使用，不影响共享基线 |

### Network（1 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `NET-001` | 端口 Host 路由 | `get_host(8080)` | 路由包含 Sandbox 标识与端口信息 |

### Snapshot（4 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `SNP-001` | 创建并查询 Snapshot | 为本轮独立 Sandbox 创建快照 | Snapshot ID 出现在查询结果中，不暂停共享基线 |
| `SNP-002` | 从 Snapshot 恢复 | 恢复为新 Sandbox | 快照前写入的文件内容仍存在 |
| `SNP-003` | 删除 Snapshot | 先终止本轮恢复 Sandbox，再删除本轮 Snapshot | 删除调用成功，账本记录清理状态 |
| `SNP-004` | 重复删除 Snapshot | 删除已删除 Snapshot | 返回 `false` 或明确不存在响应 |

### Checkpoint / Restore（8 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `CPR-001` | 创建并查询 Checkpoint | 写入普通路径、`/home/user-sandbox`、`/etc` 后创建 | Checkpoint ID 可按 source Sandbox 查询 |
| `CPR-002` | 从 Checkpoint 恢复 | `Sandbox.create(snapshot_id)` | 恢复实例保留基线文件，且 Sandbox ID 与 source 不同 |
| `CPR-003` | 两阶段精确回滚 | 依次保存 `state-1`、`state-2` | 两个恢复实例分别得到对应阶段内容 |
| `CPR-004` | source 与 restored 隔离 | 分别修改同一路径 | 两个实例的文件状态互不影响 |
| `CPR-005` | source 继续可用 | Checkpoint 后重新连接 source | source 可继续执行命令 |
| `CPR-006` | full-memory 进程恢复 | 创建心跳目录并在进程运行时创建 Checkpoint | 恢复后心跳文件继续增长 |
| `CPR-007` | 系统路径恢复 | 读取 `/home/user-sandbox` 和 `/etc` 标记 | 两个路径的文件内容均被恢复 |
| `CPR-008` | 无效 Checkpoint | 使用已删除 ID 和不存在 ID 恢复 | 两次请求均被拒绝；意外创建的 Sandbox 会立即登记为本轮资源 |

### Pause / Resume / Connect（13 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `PRC-001` | full-memory pause | `memory=true` | pause 返回 `204`，状态变为 `paused` |
| `PRC-002` | 显式 resume | 对 `PRC-001` 调用 `/resume` | 返回 `201`，恢复 `running`，Sandbox ID 不变 |
| `PRC-003` | connect 恢复 | full-memory pause 后调用 `/connect` | 返回 `201`，恢复同一 Sandbox |
| `PRC-004` | full-memory 进程状态 | 后台进程运行时 pause/resume | 恢复后原 PID 仍可查询 |
| `PRC-005` | full-memory 文件状态 | 写入文件后 pause/resume | 文件内容保持不变 |
| `PRC-006` | timeout 更新 | `resume=420`、`connect=480` | `endAt` 剩余时间落在允许误差范围 |
| `PRC-007` | 重复 pause | 状态稳定后再次 pause | 第一次 `204`，第二次 `409` |
| `PRC-008` | running connect | running 状态调用 `/connect` | 返回 `200`，Sandbox ID 不变 |
| `PRC-009` | 不存在 Sandbox | 使用格式合法但不存在的 ID 分别调用 pause、resume、connect | 三个接口均返回 `404` |
| `PRC-010` | `memory=false` resume 兼容性 | `memory=false` 后调用 `/resume` | 返回 `201`，恢复同一 Sandbox；不判定 filesystem-only 语义 |
| `PRC-011` | `memory=false` connect 兼容性 | `memory=false` 后调用 `/connect` | 返回 `201`，文件仍存在；不判定冷启动语义 |
| `PRC-014` | autoPause | `timeout=10`、`autoPause=true` | 宽限期内自动转为 `paused` |
| `PRC-015` | autoResume | 自动协商 `policy` / `enabled` payload，暂停后使用原 data-plane 对象执行命令 | 请求触发自动恢复，命令成功；部署未暴露能力时为 `BLOCKED` |

当前部署未暴露 filesystem-only pause 和 `autoPauseMemory`，因此不执行冷启动语义、进程丢失和配置冲突断言。`PRC-010`、`PRC-011` 仅验证服务端接受 `memory=false` 时的 `/resume`、`/connect` 兼容行为。`/resume` 在当前 API 中属于 deprecated compatibility path，仍保留用于升级兼容验证。`PRC-015` 会适配 `autoResume.policy` 与 `autoResume.enabled` 两类 API，并使用 pause 前保存的 data-plane 对象触发恢复。

### PTY 与 Template SDK（7 个）

| 编号 | 测试场景 | 参数或条件 | 关键断言 |
| --- | --- | --- | --- |
| `PTY-001` | 创建 PTY 并输入 | 创建终端并发送命令；通过 `on_pty` 收集输出 | 终端输出包含预期环境变量 |
| `PTY-002` | 调整终端尺寸 | `resize(rows, cols)` 后发送完整换行命令 | PTY 保持可用，尺寸更新成功 |
| `PTY-003` | 重新连接并终止 PTY | `connect()` 后 `kill()` | 可重新附着并终止本轮终端 |
| `TSDK-001` | Template 序列化 | `to_json()`、`to_dockerfile()` | 保留基础镜像、命令、目录和用户 |
| `TSDK-002` | 后台构建与状态查询 | `build_in_background()` | 返回 Template/Build ID，状态可读取 |
| `TSDK-003` | Template 存在性 | 查询本轮 SDK Template | `exists()` 返回 true |
| `TSDK-004` | Template tag 生命周期 | 添加、查询、移除本轮 tag | tag 可见后被移除；异常时账本兜底清理 |

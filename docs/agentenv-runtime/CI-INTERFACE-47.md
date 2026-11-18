# 接口门禁第 47 次运行分析与修复

日志：https://log-ci.openeuler.openatom.cn/job/multiarch/openeuler/check_interface/KASandbox/47/

PR 270，检查提交 `cea9a9e`。失败不是业务测试或编译失败，包含两类问题：

1. `spec/openapi.yml` 三个 POST 增加维护期 503 响应。oasdiff 报 `response-non-success-status-added`，仓库规则将所有接口增删改视为需要人工确认。这是预期的契约扩展，没有新增请求必填字段；保留 503 契约与原门禁规则，由维护者审核确认。
2. 集成测试客户端 `tests/integration/internal/api/generated.go` 未同步。上轮只重新生成和验证了 `packages/api/internal/api/api.gen.go`，漏掉了第二个生成入口。本轮以 oapi-codegen v2.5.1 执行该模块原有 `go generate ./internal/api`，补交生成物，不手改生成代码。

集成测试客户端已有较多历史 schema 漂移，所以重新生成的 diff 也包含九接口、transform 等现有定义；本轮没有为消除 diff 额外修改业务接口。新增 `maintenance_test.go` 覆盖 pause、checkpoint、build-status 三个客户端的 503 错误体与 Retry-After 保留，且只运行客户端包，不启动完整集成环境。

验证：Linux aarch64、Go 1.25.4、非 root、nice 15、ionice idle、并发 2、离线缓存；生成与重复生成摘要一致，客户端包 test/vet 通过。生成文件 SHA256：`1995ced9f83b39dfd92f204175704d669a8a118f42d0672c91726b9312ca4d0e`。

另一个 CI 配置注意点：本次日志比较的是 `HEAD^ (15c906a) -> HEAD (cea9a9e)`，不是 PR 对目标分支的完整差异。后续审核应显式传入 PR merge-base 与 PR head，避免补交生成文件被误认作“未改来源”，或仅因最后一个提交没有改 spec 而误判整个 PR 无接口变化。不能用最后一次提交的绿灯代替对三处 503 契约变更的人工审核。本轮不修改流水线或绕过检查。

保留 Cube 配置；未切换版本、重启服务、运行 VM 或执行任何已暂缓项目。

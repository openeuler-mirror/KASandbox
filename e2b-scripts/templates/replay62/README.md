# replay62 模板修复

`catalogs/swerebench-arm64-62.json` 中的 62 个 `swr60-*` 模板默认由 `bench replay-provision` 直接从 swerebench 镜像构建。以下 4 个镜像在内网环境下会被外网下载卡住，需要用本目录的 Dockerfile 重建，否则回放数据会被"等超时"而非真实负载主导。

| 模板 | 问题 | 修复 | 效果（124 档） |
| --- | --- | --- | --- |
| `swr60-appvia-kev-617` | `go doc k8s.io/api/...` 需下载 Go 模块，proxy.golang.org 被静默丢包 | 经 goproxy.cn 预置所需模块缓存 | 任务 ≈4100s → ≈930s |
| `swr60-kestra-io-kestra-2445` | `./gradlew` 每次下载 Gradle 发行包，约 120s 后失败（`\| tail` 使 rc=0 假成功） | 腾讯镜像预置发行包 + 预热构建脚本依赖 | 任务耗时约减半，gradle 命令真实执行 |
| `swr60-kestra-io-kestra-3429` | 同上 | 同上 | 同上 |
| `swr60-kestra-io-kestra-5478` | 同上 | 同上 | 同上 |

每个 Dockerfile 头部注释写明了根因、修复点和注意事项。

## 重建

在 `e2b-scripts/` 目录执行，模板名须与 catalog 派生的名称一致（`swr60-<name>`），规格固定 2U4G：

```bash
for t in swr60-appvia-kev-617 swr60-kestra-io-kestra-2445 swr60-kestra-io-kestra-3429 swr60-kestra-io-kestra-5478; do
  bash start.sh create-template --name "$t" \
    --dockerfile "templates/replay62/$t.Dockerfile" \
    --cpu-count 2 --memory-mb 4096
done
```

同名重建会生成新 build，模板 ID 不变；构建失败时旧 build 保持可用。

## 约束

- 模板 rootfs 约 2.5G，预置内容需控制体积（Go 模块缓存约 720M，Gradle 约 220M），并为回放期间的编译产物留出余量。
- Dockerfile 中的 `ENV` 只作用于构建步骤，沙箱运行时不继承；运行时需要的环境变量请写在 catalog 条目的 `env` 字段。
- 依赖的镜像源：`goproxy.cn`、`mirrors.cloud.tencent.com/gradle`、Maven Central、`plugins.gradle.org`。

## 其余 workload 的排查结论

2026-09-29 对全部 62 个 workload 做过网络审计（无 delay 重放全部命令并采样 `/proc/net/tcp`）：除上述 4 个外，未发现被静默丢包卡住的命令。其余慢命令属于以下情况，按负载自身特性保留：

- Maven / npm 运行时从公网下载依赖（耗时稳定，但受公网带宽影响）；
- `vitest` 默认 watch 模式挂起：`prettier-plugin-pug-448`、`joshuakgoldberg-create-typescript-app-2050` 已在 catalog 中配置 `env: {"CI": "true"}`；
- 测试自身耗时长或卡死：`arabold-docs-mcp-server-78`、`softwaremill-jox-77`、`stoplightio-spectral-914`（测试访问外网资源）。

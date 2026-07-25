# E2B 部署工具

本目录包含 E2B 基础设施部署、Kubernetes 集群初始化、节点配置和沙箱模板工具。

## 使用说明

完整的安装、部署、升级、验证、故障排查和模板操作说明请参阅：

- [USAGE.md](./USAGE.md)

开始使用前：

1. 复制并填写部署环境配置：`dep/.env`
2. 按 `USAGE.md` 中的部署模式选择 Nomad 或 Kubernetes 流程
3. 不要将包含密码、Token 或 API Key 的本地配置提交到仓库

## 主要入口

| 文件 | 用途 |
| --- | --- |
| `build.sh` | 主机依赖、服务和部署流程入口 |
| `k8s-deploy.sh` | 使用 KubeKey 初始化 Kubernetes 集群 |
| `deploy-worker.sh` | 初始化 Kubernetes Worker 节点 |
| `dep/deploy.sh` | 部署 E2B 服务和 Helm Chart |
| `build_prod.py` | 构建 E2B 沙箱模板 |
| `create_sandbox.py` | 创建沙箱实例 |
| `create_template.py` | 创建模板 |

## 配置

`dep/.env` 仅用于本地部署，不应提交。模板和变量说明见 `USAGE.md` 的配置章节。

常用的敏感配置包括：

- `POSTGRES_CONNECTION_STRING`
- `HARBOR_PASSWORD`
- `NOMAD_ACL_TOKEN`
- `E2B_ACCESS_TOKEN`
- `E2B_API_KEY`

## 目录结构

```text
deploy/
├── build.sh
├── k8s-deploy.sh
├── deploy-worker.sh
├── build_prod.py
├── create_sandbox.py
├── create_template.py
└── dep/
    ├── deploy.sh
    ├── values-template.yaml
    ├── api.yaml
    ├── edge.yaml
    ├── template-manager.yaml
    ├── webhook.yaml
    └── wildcard-ingress.yaml
```

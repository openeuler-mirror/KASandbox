# kubelet 对接 Pod 保持 Running 修复总结

> 场景：cri-multiplex 对接 kubelet 后，通过 RuntimeClass=e2b 创建的 Pod 在容器创建后立即被 StopContainer / StopPodSandbox，导致状态变为 ExitCode:0 而非 Running。
> 目标：Pod 启动后保持 Running，仅在执行 delete 时销毁沙箱；StopContainer/StopPodSandbox 保留用于暂停沙箱的能力。

---

## 一、问题现象

- Pod 通过 `kubectl apply` 创建后，先短暂进入 ContainerCreating
- 随后 kubelet 反复调用 `StopContainer` + `CreateContainer`（restartCount 递增）
- 最终 Pod 状态为 `ExitCode:0`，RESTARTS 持续增长
- cri-multiplex 日志显示 StopContainer 紧跟在 CreateContainer 之后被调用

```
2026/07/02 00:17:11 [GrpcE2BEngine] CreateContainer: pod=d10cb931..., name=app
2026/07/02 00:17:35 [GrpcE2BEngine] StopContainer: id=d10cb931...-c
2026/07/02 00:17:35 [GrpcE2BEngine] CreateContainer: pod=d10cb931..., name=app   <- 重建
2026/07/02 00:17:37 [GrpcE2BEngine] StopPodSandbox: id=d10cb931...
```

---

## 二、根因定位

kubelet 在 `CreateContainer` 请求中将容器标识信息分散放置：

| 字段 | 位置 | 内容 |
|------|------|------|
| `io.kubernetes.container.name` | **labels** | 容器名（app） |
| `io.kubernetes.pod.name` | **labels** | Pod 名 |
| `io.kubernetes.pod.namespace` | **labels** | 命名空间 |
| `io.kubernetes.pod.uid` | **labels** | Pod UID |
| `io.kubernetes.container.hash` | **annotations** | 容器 hash（PLEG 用来判断是否需重建） |
| `io.kubernetes.container.restartCount` | **annotations** | 重启计数 |
| `io.kubernetes.container.terminationMessagePath` | **annotations** | 终止消息路径 |
| `io.kubernetes.container.terminationMessagePolicy` | **annotations** | 终止消息策略 |
| `io.kubernetes.pod.terminationGracePeriod` | **annotations** | 优雅终止宽限期 |

而 kubelet 的 PLEG（Pod Lifecycle Event Generator）在 `syncPod` 中通过 **`ListContainers` 返回的 labels** 提取 `io.kubernetes.container.hash`，与本地期望 hash 比对：
- hash 不匹配 → 认为容器配置已变更 → `StopContainer` + `CreateContainer` 重建容器
- hash 缺失（值为空） → 视为不匹配 → 同样触发重建

cri-multiplex 原实现把 `req.Config.Labels` 原样存储并返回，labels 中只有 4 个基础 label，**缺少 `io.kubernetes.container.hash` 和 `io.kubernetes.container.restartCount`**，导致 kubelet 每轮 PLEG 都判定容器需要重建。

---

## 三、修复方案

### 3.1 核心修改：CreateContainer 中将 annotations 的 hash/restartCount 补到 labels

文件：`pkg/engine/grpc_e2b.go`

```go
func (e *grpcE2BEngine) CreateContainer(ctx context.Context, req *runtime.CreateContainerRequest) (*runtime.CreateContainerResponse, error) {
    log.Printf("[GrpcE2BEngine] CreateContainer: pod=%s, name=%s, labels=%v, annotations=%v",
        req.PodSandboxId, req.Config.Metadata.Name, req.Config.Labels, req.Config.Annotations)
    containerID := req.PodSandboxId + "-c"
    if pod, ok := e.tracker.Get(req.PodSandboxId); ok {
        pod.imageRef = req.Config.Image.Image
        // kubelet 将 container.hash 和 restartCount 放在 annotations 中，
        // 但 PLEG 通过 ListContainers 的 labels 提取 hash 判断是否需要重建。
        // 需将 annotations 中的 hash/restartCount 复制到 labels，否则 kubelet 认为容器 hash 不匹配反复 kill。
        labels := make(map[string]string)
        for k, v := range req.Config.Labels {
            labels[k] = v
        }
        if hash, ok := req.Config.Annotations["io.kubernetes.container.hash"]; ok {
            labels["io.kubernetes.container.hash"] = hash
        }
        if rc, ok := req.Config.Annotations["io.kubernetes.container.restartCount"]; ok {
            labels["io.kubernetes.container.restartCount"] = rc
        }
        pod.containerLabels = labels
        pod.containerName = req.Config.Metadata.Name
        pod.containerAnnotations = req.Config.Annotations
    }
    return &runtime.CreateContainerResponse{ContainerId: containerID}, nil
}
```

### 3.2 配套：podInfo 增加 containerAnnotations 字段

文件：`pkg/engine/e2b.go`

```go
type podInfo struct {
    // ...
    containerLabels      map[string]string
    containerAnnotations map[string]string   // 新增
    containerName        string
    // ...
}
```

### 3.3 配套：ListContainers / ContainerStatus 返回 containerAnnotations

之前 ContainerStatus / ListContainers 返回的是 `pod.annotations`（sandbox 的 annotations），改为返回 `pod.containerAnnotations`（CreateContainer 时记录的容器 annotations），保持 labels / annotations 来源一致。

### 3.4 StopContainer / StopPodSandbox 保留暂停能力

不修改 Stop 行为，依旧调用 E2B `Pause` API 并将 `pod.state = statePaused`。修复后 kubelet 不再误调 Stop，Stop 仅在用户主动暂停或 Pod 删除前的优雅终止阶段被调用，符合"StopContainer/StopPodSandbox 能力需要用来暂停沙箱"的设计。

---

## 四、配套修复（本问题前置条件）

为保证 kubelet 完整链路打通，此前已修复以下问题（详见 git 历史 / e2b-verify-architecture.md）：

1. **ImageStatus / ListImages** 返回 `Size_=1`（原为 0），避免 kubelet 报 "Id or size of image ... is not set"
2. **ContainerStatus** 增加 `ImageRef` 字段，避免 kubelet 报 "status.ImageRef is not set"
3. **PodSandboxStatus** 在 hostIP 为空时回退到 nodeIP，避免 kubelet 报 "Pod Sandbox status doesn't have network information"
4. **ListContainers** 使用 `containerLabels` / `containerName`，补齐 `io.kubernetes.container.name` 等 label
5. **mux.go** 路由表未命中时回退到 containerEngine，保证 kubelet 对 containerd 既有容器的查询正常
6. **RunPodSandbox** 增加 `orchestrator.Create` 失败日志，便于定位 snapshot/load EOF 等错误

---

## 五、验证流程

### 5.1 标准化刷新 build_id（每次创建 Pod 前必须执行）

E2B build_id 一次性使用，每次创建 Pod 前需重新构建模板并提取参数：

```bash
bash /home/zrj/refresh_build_id.sh [pod_name]
# 自动执行：build_prod.py -> test.py -> 提取 build_id/execution_id/token -> 生成 /tmp/e2b-kubelet-pod.yaml
```

脚本位置：`/home/zrj/refresh_build_id.sh`

### 5.2 验证步骤

```bash
# 1. 删除旧 Pod
kubectl delete pod e2b-kubelet-test --force --grace-period=0

# 2. 重启 cri-multiplex
pkill -9 -f "cri-multiplex -socket"; sleep 2
cd /home/zrj/cri-multiplex
setsid ./cri-multiplex -socket /tmp/cri-multiplex.sock \
    -containerd-socket /run/containerd/containerd.sock \
    > /tmp/cri-multiplex.log 2>&1 < /dev/null &

# 3. 刷新 build_id 并生成 Pod YAML
bash /home/zrj/refresh_build_id.sh

# 4. 创建 Pod
kubectl apply -f /tmp/e2b-kubelet-pod.yaml

# 5. 观察状态（应保持 Running，RESTARTS=0）
kubectl get pod e2b-kubelet-test -w

# 6. 验证无 Stop 调用
grep -aE "StopContainer|StopPodSandbox" /tmp/cri-multiplex.log
# 期望：无输出（或仅有 Pod 删除时的 Stop）
```

### 5.3 预期结果

```
NAME               READY   STATUS    RESTARTS   AGE
e2b-kubelet-test   1/1     Running   0          42s
```

cri-multiplex 日志中只有一次 `sandbox created` 和一次 `CreateContainer`，无 `StopContainer` / `StopPodSandbox`。

---

## 六、自动化测试用例

已将上述验证流程封装为测试脚本：`e2b-verify/07_kubelet_pod_running.sh`

可通过 `run_all.sh` 统一调度：

```bash
./run_all.sh --only 07              # 单独执行
./run_all.sh --skip-setup           # 全量执行（跳过环境准备）
```

用例覆盖：
- Pod 创建后 30s 内持续 Running
- RESTARTS 保持 0
- cri-multiplex 日志无 StopContainer / StopPodSandbox
- Pod 删除后沙箱被销毁（RemovePodSandbox 触发）

---

## 七、关键经验

1. **CRI labels vs annotations**：kubelet 在 CreateContainer 时把 hash 放 annotations，但在 ListContainers 时从 labels 读 hash，CRI 实现需要做这层"搬运"
2. **PLEG 容器识别**：kubelet 不只看 container name，更依赖 hash 判断容器是否需要重建
3. **E2B build_id 一次性**：每次创建 Pod 前必须刷新，否则触发 snapshot/load EOF
4. **cri-multiplex 路由回退**：对未在路由表的容器（如 containerd 既有容器）需回退到 containerEngine，避免 kubelet 查询失败

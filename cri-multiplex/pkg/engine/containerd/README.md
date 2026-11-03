# containerd backend

cri-multiplex 的默认后端：把 CRI 调用透传给 containerd（lazy-init gRPC 连接）。

- `New(socketPath) *Engine`：无状态构造，首次调用时建立连接。
- `Module()`：只填 `Descriptor` + `Engine`；不实现 Cleaner/Reconciler/Restorer/Health。
- 无镜像前缀：未命中其它后端前缀的镜像都由默认后端处理。

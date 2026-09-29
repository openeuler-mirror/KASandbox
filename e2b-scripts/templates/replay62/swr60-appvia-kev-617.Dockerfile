# swr60-appvia-kev-617：预置 Go 模块缓存
# 问题：镜像内 /root/go/pkg/mod 为空，轨迹中的 `go doc k8s.io/api/...` 需联网下载模块；
#       proxy.golang.org / sum.golang.org 在内网被静默丢包，每条命令卡满 300s 超时。
# 修复：构建时经 goproxy.cn 只拉取轨迹所需依赖（go list -deps），并预热轨迹中的 go doc 查询；
#       还原 go.sum，保证 /kev 的 git 状态不变。
# 注意：ENV 仅作用于构建步骤，沙箱运行时不继承；运行时依赖本地模块缓存，不走网络。
# 验证：go doc 300s → 0.4s；124 档 appvia 任务 ≈4100s → ≈930s。
FROM swr.cn-north-4.myhuaweicloud.com/kunpeng-ai/swerebench-arm64-appvia-kev:617-6c81685
USER root
ENV GOPROXY=https://goproxy.cn
ENV GOSUMDB=sum.golang.google.cn
RUN cd /kev && cp go.sum /tmp/go.sum.bak && go list -deps -test ./pkg/kev/converter/kubernetes/... > /dev/null && (go list -deps ./... > /dev/null || echo "WARN list ./... failed") && for t in Ingress IngressSpec IngressBackend IngressServiceBackend ServiceBackendPort IngressRule HTTPIngressPath PathType PathTypeExact NetworkPolicy; do go doc k8s.io/api/networking/v1.$t > /dev/null 2>&1 || echo "WARN go doc $t"; done && go doc k8s.io/api/networking/v1 > /dev/null 2>&1 && cp /tmp/go.sum.bak go.sum && rm -f /tmp/go.sum.bak && du -sh /root/go/pkg/mod && df -h / && git -C /kev status --short | head -5
USER user

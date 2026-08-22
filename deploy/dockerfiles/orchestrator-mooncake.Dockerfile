ARG oe_arch=aarch64
ARG umdk_version=25.12.0-B106.oe2403sp3
FROM openeuler-24.03-lts-sp3 AS builder

ARG oe_arch=aarch64
ARG umdk_version=25.12.0-B106.oe2403sp3

RUN echo "sslverify=false" >> /etc/yum.conf

RUN yum update -y
RUN yum install -y iptables iproute iptables-nft rsync numactl-libs liburing glog jsoncpp libibverbs yaml-cpp xxhash-libs util-linux

RUN yum install -y umdk-urma-lib-${umdk_version}.${oe_arch} umdk-urma-bin-${umdk_version}.${oe_arch} umdk-urma-devel-${umdk_version}.${oe_arch} \
umdk-urma-tools-${umdk_version}.${oe_arch} umdk-urma-example-${umdk_version}.${oe_arch}

RUN update-alternatives --set iptables /usr/sbin/iptables-nft || true
COPY orchestrator /usr/bin/orchestrator
RUN chmod +x /usr/bin/orchestrator
RUN mkdir -p /opt/e2b-infra/bin/
COPY fc-netns-exec /opt/e2b-infra/bin/
RUN chmod +x /opt/e2b-infra/bin/fc-netns-exec

#将/opt/e2b-infra/bin下mooncake相关安装到系统路径下
COPY *.rpm /opt/e2b-infra/bin/

RUN if ls /opt/e2b-infra/bin/*.rpm >/dev/null 2>&1; then rpm -ivh /opt/e2b-infra/bin/*.rpm; else echo "No RPM packages found, skipping"; fi


RUN ldconfig

ENTRYPOINT ["/usr/bin/orchestrator"]
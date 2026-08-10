FROM openeuler-24.03-lts-sp3 AS builder

RUN echo "sslverify=false" >> /etc/yum.conf

RUN yum update -y
RUN yum install -y iptables iproute iptables-nft rsync numactl-libs liburing glog jsoncpp libibverbs yaml-cpp xxhash-libs util-linux

RUN yum install -y umdk-urma-lib-25.12.0-B106.oe2403sp3.aarch64 umdk-urma-bin-25.12.0-B106.oe2403sp3.aarch64 umdk-urma-devel-25.12.0-B106.oe2403sp3.aarch64 \
umdk-urma-tool-25.12.0-B106.oe2403sp3.aarch64 umdk-urma-example-25.12.0-B106.oe2403sp3.aarch64

RUN update-alternatives --set iptables /usr/sbin/iptables-nft || true
COPY orchestrator /usr/bin/orchestrator
RUN chmod +x /usr/bin/orchestrator
RUN mkdir -p /opt/e2b-infra/bin/
COPY fc-netns-exec /opt/e2b-infra/bin/
RUN chmod +x /opt/e2b-infra/bin/fc-netns-exec

#将/opt/e2b-infra/bin下mooncake相关安装到系统路径下
RUN cp -rf /opt/e2b-infra/bin/* /opt/e2b-infra/bin/

RUN rpm -ivh *.rpm

RUN ldconfig

ENTRYPOINT ["/usr/bin/orchestrator"]
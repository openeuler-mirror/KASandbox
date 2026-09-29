# swr60-kestra-io-kestra-2445：预置 Gradle 发行包与构建脚本依赖
# 问题：镜像内无 Gradle 发行包，./gradlew 每次从 services.gradle.org（重定向 GitHub）下载，
#       内网极慢，约 120s 后读超时失败；因命令带 `| tail`，回放记录的 rc=0 为假成功。
# 修复：按 gradle-wrapper.properties 从腾讯镜像下载同名发行包，放入 wrapper 期望目录；
#       预热 `tasks --all`（插件与构建脚本依赖），不预编译，编译在回放时真实执行。
# 注意：模板构建环境带 _JAVA_OPTIONS=-Djava.net.preferIPv6Addresses=true，预热步骤内改用 IPv4；
#       不要预热 compileTestJava（会触发前端 npm 构建，写满 2.5G rootfs）；构建结束前停止 daemon。
# 验证：./gradlew tasks ≈120s 失败 → 10s；:core:compileJava 真实编译成功；kestra 任务耗时约减半。
FROM swr.cn-north-4.myhuaweicloud.com/kunpeng-ai/swerebench-arm64-kestra-io-kestra:2445-3f34d58
USER root
RUN cd /kestra && Z=$(grep distributionUrl gradle/wrapper/gradle-wrapper.properties | sed "s#.*/##") && echo "dist=$Z" && (timeout 20 ./gradlew --version > /dev/null 2>&1 || true) && D=$(ls -d /root/.gradle/wrapper/dists/${Z%.zip}/*/ | head -1) && echo "dir=$D" && cd $D && rm -f *.part && curl -sSfL --retry 3 -o $Z https://mirrors.cloud.tencent.com/gradle/$Z && unzip -q $Z && touch $Z.ok && rm -f $Z && cd /kestra && ./gradlew --version | head -3 && df -h / | tail -1
RUN export _JAVA_OPTIONS=-Djava.net.preferIPv4Stack=true && cd /kestra && (for i in 1 2 3; do ./gradlew tasks --all > /dev/null 2>/tmp/w.err && break; echo retry$i; tail -3 /tmp/w.err; sleep 15; done) && ./gradlew --offline tasks --all > /dev/null && ./gradlew --stop && du -sh /root/.gradle && df -h / | tail -1 && git -C /kestra status --short | head -5
USER user

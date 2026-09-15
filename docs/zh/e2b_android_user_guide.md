# E2B 对接 StratoVirt 支持 Android 沙箱用户指南

本文说明如何使用已定制安卓14/15/16版本的 Cuttlefish Android 镜像和 cvd-host_package，在 arm64 节点上准备 E2B Android 模板运行环境，并通过 E2B SDK 验证模板与沙箱。

## 1. 前置条件

准备以下文件、软件和目录：

| **项目** | **说明** |
|----|----|
| Android 镜像 | aosp_cf_arm64_only_phone-img-eng.android-build.zip  |
| Cuttlefish host 包 | cvd-host_package.tar.gz |
| StratoVirt | 通过 RPM 安装，复制到/stratovirt-versions/v1.13.1/stratovirt |
| QEMU | 供 launch_cvd 初始化运行时文件 |
| 容器镜像工具 | 已安装 oras，可访问 Harbor |

多版本镜像和cuttlefish host包获取路径：（不同分支对应不同安卓版本, 注意clone需要使用lfs，可以参考代码仓里的readme）

[cvd-host-packages-and-images - AtomGit](https://gitcode.com/h3288824963/cvd-host-packages-and-images)

```bash
git clone https://gitcode.com/h3288824963/cvd-host-packages-and-images.git
cd cvd-host-packages-and-images

## 一次性下载所有远程分支引用的 LFS 文件（不会切换工作树）
yum install git-lfs
git lfs fetch --all

## 然后可以自由切换任意分支，LFS 文件都已就绪
git checkout android14  ##其他版本同理android15/android16
```

创建/cvd-host-packages目录，将刚刚clone的三个分支的代码分别放在该目录下的子文件夹中：

```bash
mkdir /cvd-host-packages
cd /cvd-host-packages
mkdir android-{14,15,16}

##使用 export 命令设置的环境变量仅在当前终端（Shell）会话中有效
export ANDROID_VERSION=14  # 其他版本设为 15 或 16
export CF_DIR="/cvd-host-packages/android-${ANDROID_VERSION}"
export INSTANCE=01
export LD_LIBRARY_PATH=   ##在宿主机上执行cuttlefish有关命令前必须做，不然会被环境变量中的系统库污染，不走内部的lib64
```

### 安装QEMU

```bash
dnf install -y qemu
cp /usr/bin/qemu-kvm /usr/bin/qemu-system-aarch64
```

### 准备cuttlefish环境

从下面链接中获取cuttlefish-base.zip

[u_boot_product - AtomGit](https://atomgit.com/super55/u_boot_product)

```bash
unzip -q cuttlefish-base.zip

cd cuttlefish-base

./setup_cuttlefish_env.sh

newgrp cuttlefish

groups

usermod -aG kvm,render,cuttlefish root
```

执行完后在新建会话里groups确认一下有没有kvm,cuttlefish,cvdnetwork这几个组

### 准备stratovirt

1\. 获取stratovirt的rpm包从[121.36.84.172/dailybuild/EBS-openEuler-24.03-LTS-SP3/](http://121.36.84.172/dailybuild/EBS-openEuler-24.03-LTS-SP3/) 最新的日构建everything目录获取并进行安装

```bash
# 使用 dnf 安装 RPM 包并自动处理依赖关系。
dnf install -y ./stratovirt-2.4.0-13.oe2403sp3.aarch64.rpm
```

2\. 创建 /stratovirt-versions/v1.13.1/ 目录，将/usr/bin/stratovirt二进制放到该目录下，并修改权限为755

```bash
mkdir -p /stratovirt-versions/v1.13.1/
cd /stratovirt-versions/v1.13.1/
cp /usr/bin/stratovirt ./
chmod 755 /stratovirt-versions/v1.13.1/stratovirt
```

## 2. 创建 TAP 网络接口

Android 启动需要一个移动网络 TAP 接口以及两个附加 TAP 接口。以下脚本创建实例 01 对应的接口。

```bash
#!/bin/bash

N="${INSTANCE:-01}"
groupadd cvdnetwork
# 移动网络 TAP：config_server 会使用该接口的 IP。
ip tuntap add dev "cvd-mtap-${N}" mode tap group cvdnetwork vnet_hdr
ip link set "cvd-mtap-${N}" up
ip addr add 192.168.97.1/30 broadcast + dev "cvd-mtap-${N}"

if ! iptables -t nat -C POSTROUTING -s 192.168.97.0/30 -j MASQUERADE 2>/dev/null; then
  iptables -t nat -A POSTROUTING -s 192.168.97.0/30 -j MASQUERADE
fi

# 附加网络 TAP：供 QEMU/StratoVirt 使用，无需配置 IP。
ip tuntap add dev "cvd-etap-${N}" mode tap group cvdnetwork vnet_hdr
ip link set "cvd-etap-${N}" up
ip tuntap add dev "cvd-wtap-${N}" mode tap group cvdnetwork vnet_hdr
ip link set "cvd-wtap-${N}" up

# 验证。
ip addr show "cvd-mtap-${N}" | grep 'inet '
ip tuntap show | grep "cvd-mtap-${N}"
ip tuntap show | grep "cvd-etap-${N}"
ip tuntap show | grep "cvd-wtap-${N}"
```

若创建接口时提示已存在，或需要清理上一次失败操作留下的接口，可先执行以下脚本，再重新执行创建脚本。

```bash
#!/bin/bash

N="${INSTANCE:-01}"

iptables -t nat -D POSTROUTING -s 192.168.97.0/30 -j MASQUERADE 2>/dev/null

ip addr del 192.168.97.1/30 dev "cvd-mtap-${N}"
ip link set "cvd-mtap-${N}" down
ip tuntap del dev "cvd-mtap-${N}" mode tap

ip link set "cvd-etap-${N}" down
ip tuntap del dev "cvd-etap-${N}" mode tap

ip link set "cvd-wtap-${N}" down
ip tuntap del dev "cvd-wtap-${N}" mode tap

ip tuntap show | grep "cvd-mtap-${N}" || echo "cvd-mtap-${N} 已删除"
ip tuntap show | grep "cvd-etap-${N}" || echo "cvd-etap-${N} 已删除"
ip tuntap show | grep "cvd-wtap-${N}" || echo "cvd-wtap-${N} 已删除"
```

## 3. 解压 Cuttlefish 文件

创建工作目录，将两个压缩包放入该目录后解压：

```bash
mkdir -p "$CF_DIR"
cd "$CF_DIR"

unzip aosp_cf_arm64_only_phone-img-eng.android-build.zip
tar -xzvf cvd-host_package.tar.gz
```

解压完成后，目录中应包含 bin/launch_cvd、bin/adb、etc/bootloader_aarch64/bootloader.qemu 等 Cuttlefish 文件。

注意需要从[u_boot_product - AtomGit](https://atomgit.com/super55/u_boot_product) 获取对应安卓版本的bootloader.stratovirt 放到 /\$CF_DIR/etc/bootloader_aarch64/ 目录下

## 4. 使用 Cuttlefish 和 QEMU 、stratovirt初始化验证 Android

在 \$CF_DIR 中启动 Cuttlefish。此步骤使用 QEMU 后端启动 Android，同时生成后续 StratoVirt 启动所需的运行时磁盘文件。

**注意：**

这里启动不同版本android时需要使用对应版本的run_qemu.sh和run_stratovirt2.sh， 注意脚本请从[cvd-host-packages-and-images - AtomGit](https://gitcode.com/h3288824963/cvd-host-packages-and-images)获取最新版本。

```bash
bash run_qemu.sh
#另开一个终端检查qemu和host侧模拟服务有没有启动成功，
ps aux|grep "$CF_DIR"
#确认host模拟服务拉起即可，15和16当前剪裁后只剩下modem、vsock_proxy、secure_env；14、15版本通过run_qemu.sh可以正常拉起qemu进程，另开一个终端kill掉qemu进程，执行run_stratovirt2.sh即可
#16版本由于sensor_fifo_vm（hvc13）管道没有类似--start_gnss_proxy=false控制开关，qemu进程会拉起失败，无影响，另开一个终端拉起stratovirt进程验证即可
bash run_stratovirt2.sh
```

新开终端，连接设备并检查 Android 状态：

```bash
cd "$CF_DIR"

./bin/adb connect 127.0.0.1:6520
./bin/adb devices
./bin/adb shell
```

如果devices显示该设备offline，则通过'cat \${CF_DIR}/cuttlefish/instances/cvd-1/internal/kernel-log-file' 查看下kernel的状态

在 Android shell 中检查网络和 envd：

```bash
ifconfig
ss -lntp | grep ':49983'
```

预期 buried_eth0 的地址为 192.168.97.2(安卓14) \| 255.255.255.255(安卓15,16)，eth1 的地址为 169.254.0.21，并且 envd 监听 49983 端口。

## 5. 准备 E2B 节点文件

### 5.1 准备固件目录

创建 /firmware，并从 \$CF_DIR 复制 persistent 磁盘。persistent_composite.img 在固件目录中使用 persistent.img 作为文件名。

同时从[u_boot_product - AtomGit](https://gitcode.com/super55/u_boot_product)下载对应安卓版本的bootloader.stratovirt 放入 /firmware/android-\<版本\>/ 目录下。

```bash
mkdir -p /firmware/android-{14,15,16}/
cp "$CF_DIR/cuttlefish/instances/cvd-1/persistent_composite.img" "/firmware/android-${ANDROID_VERSION}/persistent.img"
```

### 5.2 准备并上传 Android OS 镜像

准备 raw 镜像目录，并复制 os_composite.img：

```bash
mkdir -p "/opt/android_image/android-${ANDROID_VERSION}"
cp "$CF_DIR/cuttlefish/instances/cvd-1/os_composite.img" "/opt/android_image/android-${ANDROID_VERSION}/os_composite.img"
```

设置 Harbor 地址和账号后，使用 ORAS 推送镜像：

```bash
export REGISTRY='<Harbor_IP>:30443'
export PROJECT='e2b-orchestration'
export HARBOR_USER='<Harbor_用户名>'
export HARBOR_PASS='<Harbor_密码>'
export IMG_DIR="/opt/android_image/android-${ANDROID_VERSION}"
export MEDIA_TYPE='application/vnd.e2b.raw.image'

oras login "$REGISTRY" -u "$HARBOR_USER" -p "$HARBOR_PASS" --insecure

oras push --insecure "$REGISTRY/$PROJECT/android-os:${ANDROID_VERSION}" --disable-path-validation \
  "$IMG_DIR/os_composite.img:$MEDIA_TYPE"
```

## 6. 配置 template-manager

编辑 template-manager DaemonSet：

```bash
kubectl edit daemonset template-manager -ne2b
```

在 spec.template.spec.containers 中名为 template-manager 的容器下补充以下环境变量和挂载；在 spec.template.spec.volumes 下补充对应卷。CVD_HOST_PACKAGE_DIR 与 FIRMWARE_DIR 的值应分别与挂载路径一致。

```yaml
spec:
  template:
    spec:
      containers:
        - name: template-manager
          env:
            - name: ENVD_TIMEOUT
              value: 300s
            - name: CVD_HOST_PACKAGE_DIR
              value: /cvd-host-packages
          volumeMounts:
            - name: stratovirt-versions
              mountPath: /stratovirt-versions
            - name: cvd-host-package
              mountPath: /cvd-host-packages
            - name: firmware
              mountPath: /firmware
            - name: lib-pixman
              mountPath: /usr/lib/aarch64-linux-gnu/libpixman-1.so.0
              readOnly: true
      volumes:
        - name: stratovirt-versions
          hostPath:
            path: /stratovirt-versions
            type: DirectoryOrCreate
        - name: cvd-host-package
          hostPath:
            path: /cvd-host-packages
            type: Directory
        - name: firmware
          hostPath:
            path: /firmware
            type: Directory
        - name: lib-pixman
          hostPath:
            path: /usr/lib64/libpixman-1.so.0.42.2
            type: File
```

保存后pod会自动重启，等待 DaemonSet 就绪

## 7. 使用 E2B SDK 构建并验证 Android 模板

将以下脚本保存为 test_e2b_sdk.py。其中 E2B_CLUSTER_IP、Harbor 账号和密码均从环境变量读取；E2B_CONFIG_PATH 默认为 /root/.e2b/config.json。安卓多版本需要配置android_version="14"字段，15/16同理

```python
"""构建并验证单盘 Android raw 镜像模板。"""

"""
用法:
    python /home/xwk/test_e2b_sdk.py
"""
import json
import os
import sys
from pathlib import Path
from e2b import Template, default_build_logger
from e2b import Sandbox
from e2b.connection_config import ConnectionConfig
import time

CLUSTER_IP = os.getenv("E2B_CLUSTER_IP", "<本机IP>")
CONFIG_PATH = os.getenv("E2B_CONFIG_PATH", "/root/.e2b/config.json")
HARBOR_REGISTRY = f"{CLUSTER_IP}:30443"
HARBOR_PROJECT = "e2b-orchestration"
HARBOR_USER = os.getenv("HARBOR_USER", "admin")
HARBOR_PASS = os.getenv("HARBOR_PASS", "Harbor12345")
ANDROID_OS_URL = f"{HARBOR_REGISTRY}/{HARBOR_PROJECT}/android-os:14"

def _get_sandbox_url_http(self, sandbox_id, sandbox_domain):
    return f"http://{self.get_host(sandbox_id, sandbox_domain, ConnectionConfig.envd_port)}"
ConnectionConfig.get_sandbox_url = _get_sandbox_url_http

def load_and_set_env() -> None:
    if not Path(CONFIG_PATH).exists():
        print(f"[ERROR] 配置文件不存在: {CONFIG_PATH}")
        sys.exit(1)

    with open(CONFIG_PATH, "r", encoding="utf-8") as f:
        data = json.load(f)

    access_token = data.get("accessToken")
    team_api_key = data.get("teamApiKey")

    if not access_token or not team_api_key:
        print("[ERROR] 配置文件中未找到 accessToken 或 teamApiKey 字段")
        sys.exit(1)

    os.environ["E2B_ACCESS_TOKEN"] = access_token
    os.environ["E2B_API_KEY"] = team_api_key
    os.environ["E2B_API_URL"] = f"http://{CLUSTER_IP}:3000"
    os.environ["E2B_DOMAIN"] = "e2b.app"


def build_android_template() -> None:
    """构建 Android 模板：from_image_raw + Harbor 里的镜像。"""
    template = (
        Template()
        .from_image_raw(
            url=ANDROID_OS_URL,
            username=HARBOR_USER,
            password=HARBOR_PASS,
            os_type="android",
            android_version="14",
        )
    )
    Template.build(
        template,
        name="android",
        cpu_count=2,
        memory_mb=4096,
        on_build_logs=default_build_logger(),
        skip_cache=False,
    )

def create_android_sandbox() -> None:
    """创建 Android 沙箱并执行验证命令。"""
    sbx = Sandbox.create("android", timeout=300)
    try:
        print("[android] sandbox id:", sbx.sandbox_id)
        print(sbx.commands.run("getprop ro.build.version.release"))
        print(sbx.commands.run("getprop ro.product.model"))
        print(sbx.commands.run("whoami"))
        print(sbx.commands.run("getprop  | grep -ai boot | grep sys.boot_completed"))
        print(sbx.commands.run("ifconfig"))
        print(sbx.commands.run("getenforce"))
    finally:
        sbx.kill()

def main() -> None:
    load_and_set_env()
    build_android_template()
    create_android_sandbox()

if __name__ == "__main__":
    main()
```

脚本会构建名为 android 的模板，创建一个 300 秒超时的沙箱，并输出 Android 版本、设备型号、启动状态、网络信息和 SELinux 状态；验证结束后自动终止沙箱。

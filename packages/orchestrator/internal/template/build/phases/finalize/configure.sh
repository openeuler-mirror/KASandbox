#!/bin/sh
set -e

echo "Starting configuration script"

cat <<EOF > /.e2b
ENV_ID={{ .TemplateID }}
TEMPLATE_ID={{ .TemplateID }}
BUILD_ID={{ .BuildID }}
EOF

# 检测系统类型：Debian/Ubuntu 会存在此文件，openEuler/RHEL 不存在
IS_DEBIAN=0
if test -f /etc/debian_version; then
    IS_DEBIAN=1
    echo "Detected Debian-based system"
else
    echo "Detected RHEL-based system (openEuler/CentOS/RHEL)"
fi

# Create default user.
echo "Create default user 'user' (if doesn't exist yet)"

if [ "$IS_DEBIAN" -eq 1 ]; then
    ADDUSER_OUTPUT=$(adduser --disabled-password --gecos "" user 2>&1 || true)
    echo "$ADDUSER_OUTPUT"
    if echo "$ADDUSER_OUTPUT" | grep -q "The home directory \`/home/user' already exists"; then
        # Copy skeleton files if they don't exist in the home directory
        echo "Copy skeleton files to /home/user"
        cp -rn /etc/skel/. /home/user/
    fi
else
    if ! id -u user >/dev/null 2>&1; then
        echo "Creating user with useradd"
        # 如果家目录已存在，useradd -m 会报错，需要先检查
        if [ -d /home/user ]; then
            # 家目录已存在，不自动创建（-M），后续手动处理
            useradd -M -s /bin/bash -c "" user
        else
            # 正常创建用户和家目录
            useradd -m -s /bin/bash -c "" user
        fi
    fi
    # 无论如何都确保骨架文件被复制（-n 表示不覆盖已有文件）
    if [ -d /etc/skel ] && [ -d /home/user ]; then
        echo "Copy skeleton files to /home/user"
        cp -rn /etc/skel/. /home/user/
    fi
fi

echo "Add sudo to 'user' with no password"
if [ "$IS_DEBIAN" -eq 1 ]; then
    # Debian/Ubuntu 使用 sudo 组
    usermod -aG sudo user
else
    # openEuler/RHEL 使用 wheel 组[^4^][^10^]
    usermod -aG wheel user
fi

passwd -d user
echo "user ALL=(ALL:ALL) NOPASSWD: ALL" >>/etc/sudoers

echo "Give 'user' ownership to /home/user"
mkdir -p /home/user
chown -R user:user /home/user

echo "Give 777 permission to /usr/local"
chmod 777 -R /usr/local

echo "Create /code directory"
mkdir -p /code
echo "Give 777 permission to /code"
chmod 777 -R /code

echo "Finished configuration script"


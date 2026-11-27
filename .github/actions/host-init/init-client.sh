#!/usr/bin/env bash

set -euo pipefail

# Set timestamp format
PS4='[\D{%Y-%m-%d %H:%M:%S}] '
# Enable command tracing
set -x

# Add cache disk for orchestrator and swapfile
MOUNT_POINT="/orchestrator"

# Step 2: Create the mount point
sudo mkdir -p $MOUNT_POINT

sudo mkdir -p /orchestrator/sandbox
sudo mkdir -p /orchestrator/template
sudo mkdir -p /orchestrator/build

# Add swapfile
SWAPFILE="/swapfile"
if [ ! -e "$SWAPFILE" ]; then
    sudo fallocate -l 1G $SWAPFILE
    sudo chmod 600 $SWAPFILE
    sudo mkswap $SWAPFILE
    sudo swapon $SWAPFILE
else
    echo "Swapfile $SWAPFILE already exists, skipping creation."
fi

# Make swapfile persistent
echo "$SWAPFILE none swap sw 0 0" | sudo tee -a /etc/fstab

# Set swap settings
sudo sysctl vm.swappiness=10
sudo sysctl vm.vfs_cache_pressure=50

# Add tmpfs for snapshotting
# TODO: Parametrize this
sudo mkdir -p /mnt/snapshot-cache
sudo mount -t tmpfs -o size=65G tmpfs /mnt/snapshot-cache

ulimit -n 1048576
export GOMAXPROCS='nproc'

sudo tee -a /etc/sysctl.conf <<EOF
# Increase the maximum number of socket connections
net.core.somaxconn = 65535

# Increase the maximum number of backlogged connections
net.core.netdev_max_backlog = 65535

# Increase maximum number of TCP sockets
net.ipv4.tcp_max_syn_backlog = 65535

# Increase the maximum number of memory map areas
vm.max_map_count=1048576

EOF
sudo sysctl -p

echo "Disabling inotify for NBD devices"
# https://lore.kernel.org/lkml/20220422054224.19527-1-matthew.ruffell@canonical.com/
cat <<EOH >/etc/udev/rules.d/97-nbd-device.rules
# Disable inotify watching of change events for NBD devices
ACTION=="add|change", KERNEL=="nbd*", OPTIONS:="nowatch"
EOH

sudo udevadm control --reload-rules
sudo udevadm trigger

# Load the nbd module with 4096 devices
sudo modprobe nbd nbds_max=4096

# Create the directory for the fc mounts
mkdir -p /fc-vm

case "$(uname -m)" in
    aarch64) goarch=arm64 ;;
    x86_64) goarch=amd64 ;;
    *) goarch="$(uname -m)" ;;
esac

ROOT="${E2B_INFRA_ROOT:-/opt/e2b-infra}"
BIN="${ROOT}/bin"

# Download envd buckets
envd_dir="/fc-envd"
mkdir -p $envd_dir
if [ -f "${BIN}/envd" ]; then
    cp "${BIN}/envd" "${envd_dir}/."
elif [ -f ./bin/envd ]; then
    cp ./bin/envd "${envd_dir}/."
fi
chmod -R 755 $envd_dir
ls -lh $envd_dir || true

# Kernel: arch-prefixed layout with legacy fallback (HostKernelPath)
kernels_dir="/fc-kernels"
mkdir -p "${kernels_dir}/vmlinux-6.1.158/${goarch}"
mkdir -p "${kernels_dir}/vmlinux-6.6.0-132.0.0/${goarch}"
if [ -f "${BIN}/vmlinux.bin" ]; then
    cp "${BIN}/vmlinux.bin" "${kernels_dir}/vmlinux-6.1.158/vmlinux.bin"
    cp "${BIN}/vmlinux.bin" "${kernels_dir}/vmlinux-6.1.158/${goarch}/vmlinux.bin"
elif [ -f ./bin/vmlinux.bin ]; then
    cp ./bin/vmlinux.bin "${kernels_dir}/vmlinux-6.1.158/vmlinux.bin"
    cp ./bin/vmlinux.bin "${kernels_dir}/vmlinux-6.1.158/${goarch}/vmlinux.bin"
fi
if [ -f "${BIN}/vmlinux.bin.openeuler" ]; then
    cp "${BIN}/vmlinux.bin.openeuler" "${kernels_dir}/vmlinux-6.6.0-132.0.0/vmlinux.bin"
    cp "${BIN}/vmlinux.bin.openeuler" "${kernels_dir}/vmlinux-6.6.0-132.0.0/${goarch}/vmlinux.bin"
elif [ -f ./bin/vmlinux.bin.openeuler ]; then
    cp ./bin/vmlinux.bin.openeuler "${kernels_dir}/vmlinux-6.6.0-132.0.0/vmlinux.bin"
    cp ./bin/vmlinux.bin.openeuler "${kernels_dir}/vmlinux-6.6.0-132.0.0/${goarch}/vmlinux.bin"
fi
chmod -R 755 $kernels_dir
ls -lh $kernels_dir

# Firecracker: v1.14.4 with WP_ASYNC/memfd/sync-WP commits stripped for openEuler 6.6
fc_ver="${FIRECRACKER_VERSION:-1.14.4}"
fc_ver="${fc_ver#v}"
fc_versions_dir="/fc-versions"
mkdir -p "${fc_versions_dir}/v${fc_ver}/${goarch}"
fc_src=""
if [ -f "${BIN}/firecracker" ]; then
    fc_src="${BIN}/firecracker"
elif [ -f /opt/e2b-infra/bin/firecracker ]; then
    fc_src="/opt/e2b-infra/bin/firecracker"
fi
if [ -n "${fc_src}" ]; then
    cp "${fc_src}" "${fc_versions_dir}/v${fc_ver}/firecracker"
    cp "${fc_src}" "${fc_versions_dir}/v${fc_ver}/${goarch}/firecracker"
    chmod +x "${fc_versions_dir}/v${fc_ver}/firecracker" "${fc_versions_dir}/v${fc_ver}/${goarch}/firecracker"
fi
chmod -R 755 "${fc_versions_dir}/v${fc_ver}"
# Compat symlinks for old template metadata that still pins legacy FC dirs.
# These point at the installed v1.14.4 tree (not official GCS v1.14-0.2.0).
if [ -d "${fc_versions_dir}/v${fc_ver}" ]; then
    ln -sfn "v${fc_ver}" "${fc_versions_dir}/v1.13.1"
    ln -sfn "v${fc_ver}" "${fc_versions_dir}/v1.14-0.2.0"
fi
ls -lh $fc_versions_dir

# Busybox for template builds: HOST_BUSYBOX_DIR/<version>/<arch>/busybox
busybox_ver="${BUSYBOX_VERSION:-1.35}"
busybox_dir="/fc-busybox"
mkdir -p "${busybox_dir}/${busybox_ver}/${goarch}"
bb_src=""
if [ -f "${ROOT}/fc-busybox/${busybox_ver}/${goarch}/busybox" ]; then
    bb_src="${ROOT}/fc-busybox/${busybox_ver}/${goarch}/busybox"
elif [ -f "${BIN}/busybox" ]; then
    bb_src="${BIN}/busybox"
fi
if [ -n "${bb_src}" ]; then
    cp "${bb_src}" "${busybox_dir}/${busybox_ver}/${goarch}/busybox"
    chmod +x "${busybox_dir}/${busybox_ver}/${goarch}/busybox"
fi
chmod -R 755 $busybox_dir
ls -lh $busybox_dir

# Set up huge pages
echo "[Setting up huge pages]"
sudo mkdir -p /mnt/hugepages
mount -t hugetlbfs none /mnt/hugepages || true

available_ram=$(grep MemTotal /proc/meminfo | awk '{print $2}') # in KiB
available_ram=$(($available_ram / 1024))                        # in MiB
echo "- Total memory: $available_ram MiB"

min_normal_ram=$((4 * 1024))                             # 4 GiB
min_normal_percentage_ram=$(($available_ram * 16 / 100)) # 16% of the total memory
max_normal_ram=$((42 * 1024))                            # 42 GiB

max() {
    if (($1 > $2)); then
        echo "$1"
    else
        echo "$2"
    fi
}

min() {
    if (($1 < $2)); then
        echo "$1"
    else
        echo "$2"
    fi
}

ensure_even() {
    if (($1 % 2 == 0)); then
        echo "$1"
    else
        echo $(($1 - 1))
    fi
}

remove_decimal() {
    echo "$(echo $1 | sed 's/\..*//')"
}

reserved_normal_ram=$(max $min_normal_ram $min_normal_percentage_ram)
reserved_normal_ram=$(min $reserved_normal_ram $max_normal_ram)
echo "- Reserved RAM: $reserved_normal_ram MiB"

hugepages_ram=$(($available_ram - $reserved_normal_ram))
hugepages_ram=$(remove_decimal $hugepages_ram)
hugepages_ram=$(ensure_even $hugepages_ram)
echo "- RAM for hugepages: $hugepages_ram MiB"

hugepage_size_in_mib=2
echo "- Huge page size: $hugepage_size_in_mib MiB"
hugepages=$(($hugepages_ram / $hugepage_size_in_mib))

base_hugepages_percentage=20
base_hugepages=$(($hugepages * $base_hugepages_percentage / 100))
base_hugepages=$(remove_decimal $base_hugepages)
echo "- Allocating $base_hugepages huge pages ($base_hugepages_percentage%) for base usage"
if [ -w /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages ]; then
    echo $base_hugepages >/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages
else
    echo $base_hugepages >/proc/sys/vm/nr_hugepages
fi

overcommitment_hugepages_percentage=$((100 - $base_hugepages_percentage))
overcommitment_hugepages=$(($hugepages * $overcommitment_hugepages_percentage / 100))
overcommitment_hugepages=$(remove_decimal $overcommitment_hugepages)
echo "- Allocating $overcommitment_hugepages huge pages ($overcommitment_hugepages_percentage%) for overcommitment"
if [ -w /sys/kernel/mm/hugepages/hugepages-2048kB/nr_overcommit_hugepages ]; then
    echo $overcommitment_hugepages >/sys/kernel/mm/hugepages/hugepages-2048kB/nr_overcommit_hugepages
else
    echo $overcommitment_hugepages >/proc/sys/vm/nr_overcommit_hugepages
fi

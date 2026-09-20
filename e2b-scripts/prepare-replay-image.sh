#!/usr/bin/env bash
# 构建 bench replay 真实轨迹所需的任务镜像并推送到 registry。
#
# 用法：
#   bash prepare-replay-image.sh <registry前缀> [replay-aenv源码目录]
#
# 示例：
#   bash prepare-replay-image.sh 193.30.8.2:30443/e2b-orchestration /opt/fqy/replay-aenv-main
#
# 之后 bench replay 真实轨迹模式（--trajectory-dir 且未传 -t）会自动用该镜像
# 创建任务模板；registry 地址需同步到 bench.toml [replay].task_template_image。
#
# 环境变量：
#   TARGET_REPO   django-money 源码仓库（默认 gitcode 镜像，可换 gitee：
#                 https://gitee.com/mirrors_Django-money/django-money.git）
#   TARGET_COMMIT 基准 commit（默认 835c1ab8，与轨迹录制版本一致）
#   IMAGE_TAG     推送的镜像 tag（默认 poc_v2）
set -euo pipefail

REGISTRY="${1:?用法: bash prepare-replay-image.sh <registry前缀，如 193.30.8.2:30443/e2b-orchestration> [replay-aenv源码目录]}"
SRC="${2:-/opt/fqy/replay-aenv-main}"
TARGET_REPO="${TARGET_REPO:-https://gitcode.com/gh_mirrors/dj/django-money.git}"
TARGET_COMMIT="${TARGET_COMMIT:-835c1ab8}"
IMAGE_TAG="${IMAGE_TAG:-poc_v2}"
PIIP="${PIP_INDEX_URL:-https://mirrors.aliyun.com/pypi/simple}"

[[ -f "$SRC/dockerfiles/base-image/Dockerfile.base" ]] || { echo "未找到 $SRC/dockerfiles/base-image/Dockerfile.base"; exit 1; }
[[ -f "$SRC/dockerfiles/django-money_task/environment/Dockerfile" ]] || { echo "未找到任务镜像 Dockerfile"; exit 1; }

echo "==> 1/3 构建基础镜像 harbor-swe-tools:base"
docker build --progress=plain \
  --build-arg PIP_INDEX_URL="$PIIP" --build-arg PIP_DEFAULT_TIMEOUT=300 \
  -f "$SRC/dockerfiles/base-image/Dockerfile.base" \
  -t harbor-swe-tools:base "$SRC/dockerfiles/base-image"

echo "==> 2/3 构建任务镜像 django-money:$IMAGE_TAG（repo=$TARGET_REPO commit=$TARGET_COMMIT）"
docker build --progress=plain \
  --build-arg BASE_IMAGE=harbor-swe-tools:base \
  --build-arg TARGET_REPO="$TARGET_REPO" --build-arg TARGET_COMMIT="$TARGET_COMMIT" \
  --build-arg PIP_INDEX_URL="$PIIP" --build-arg PIP_DEFAULT_TIMEOUT=300 \
  -f "$SRC/dockerfiles/django-money_task/environment/Dockerfile" \
  -t "django-money:$IMAGE_TAG" "$SRC/dockerfiles/django-money_task"

echo "==> 3/3 推送 $REGISTRY/django-money:$IMAGE_TAG"
docker tag "django-money:$IMAGE_TAG" "$REGISTRY/django-money:$IMAGE_TAG"
docker push "$REGISTRY/django-money:$IMAGE_TAG"

echo
echo "完成。后续动作："
echo "  1. bench.toml [replay].task_template_image 改为 $REGISTRY/django-money:$IMAGE_TAG"
echo "  2. 验证：bash bench.sh replay --trajectory-dir $SRC/delay_time_trajectories --target-count 2 --running-concurrency 1"
echo "     （不传 -t 会自动创建任务模板 django-money-task-v2）"

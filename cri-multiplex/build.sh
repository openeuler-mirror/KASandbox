#!/usr/bin/env bash
# e2b-runtime 分支已不再包含 cri-multiplex 组件（该组件在 KASandbox 主包中
# 单独维护）。此桩脚本仅为兼容 openEuler 门禁 check_build 流程中对
# ./cri-multiplex/build.sh 的固定调用，直接成功返回。
echo "cri-multiplex is not part of the e2b-runtime tree; nothing to build."
exit 0

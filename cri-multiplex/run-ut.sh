#!/usr/bin/env bash
# e2b-runtime 分支不包含 cri-multiplex 组件（UT 已由根目录 run-ut.sh 覆盖
# e2b-infra 各包）。此桩脚本仅为兼容门禁 DT 流程中对
# ./cri-multiplex/run-ut.sh 的固定调用，直接成功返回。
echo "cri-multiplex is not part of the e2b-runtime tree; nothing to test."
exit 0

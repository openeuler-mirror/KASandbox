#!/usr/bin/env bash
# 公共输出函数库：build.sh 与 deploy.sh 共用（source 引入，勿直接执行）
# info/success/warn 输出带颜色与图标；error 输出后 exit 1（等价原 deploy.sh 预期的 err 语义）
info() {
    echo -e "\033[34mℹ️ $*\033[0m"
}
success() {
    echo -e "\033[32m✅ $*\033[0m"
}
error() {
    echo -e "\033[31m❌ $*\033[0m"
    exit 1
}
warn() {
    echo -e "\033[33m⚠️ $*\033[0m"
}

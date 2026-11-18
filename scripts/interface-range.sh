#!/usr/bin/env bash
# 仅解析比较范围，不拉取远端或修改工作区；由调用方处理失败退出码。
resolve_interface_range() {
    BASE_REF="${BASE:-${1:-}}"
    TARGET_REF="${TARGET:-${2:-HEAD}}"
    local target_branch="${CHANGE_TARGET:-${gitlabTargetBranch:-}}"

    # 此固定 Jenkins 任务检查发往 master 的 PR，旧任务未导出目标分支变量。
    if [ -z "$target_branch" ] && [ "${JOB_NAME:-}" = "multiarch/openeuler/check_interface/KASandbox" ]; then
        target_branch=master
    fi

    if [ -z "$BASE_REF" ] && [ -n "$target_branch" ]; then
        git check-ref-format "refs/heads/$target_branch" >/dev/null 2>&1 || {
            echo "无效 PR 目标分支: $target_branch" >&2
            return 2
        }
        local target_ref="refs/remotes/origin/$target_branch"
        git rev-parse --verify "${target_ref}^{commit}" >/dev/null 2>&1 || {
            echo "缺少 PR 目标分支 $target_ref；请先 fetch 目标分支，或显式指定 BASE/TARGET" >&2
            return 2
        }
        BASE_REF="$(git merge-base "$target_ref" "$TARGET_REF")" || {
            echo "无法找到 PR 共同祖先；请补齐 Git 历史，或显式指定 BASE/TARGET" >&2
            return 2
        }
        echo "PR 比较基线: $target_ref 的共同祖先 $BASE_REF"
    fi
    BASE_REF="${BASE_REF:-HEAD^}"
}

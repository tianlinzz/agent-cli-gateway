#!/usr/bin/env bash
# release.sh — 发布脚本：生成日期版本 tag 并 push，触发 CI 流水线。
#
# 职责：只负责打 tag + push，**不管构建**。push 后云平台流水线监听到 tag，
# 自动跑 kaniko 构建并推送镜像。
#
# 版本号规则（日期）：
#   v<YYYYMMDD>-<序号>
#   同一天多次发布会自动递增序号：v20260702-1, v20260702-2, ...
#
# 用法：
#   ./scripts/release.sh              # 自动算今天的版本号
#   ./scripts/release.sh v2.0.0       # 手动指定（覆盖自动版本，发特殊版本时用）
#
# 前提：
#   - 当前在 main 分支（避免从特性分支误发）
#   - 工作区干净（无未提交改动）
#   - 远程 origin 可达
set -euo pipefail

cd "$(dirname "$0")/.."

# --- 前置检查 ---

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if [ "$BRANCH" != "main" ]; then
    echo "❌ 当前分支: $BRANCH，发布必须在 main 分支" >&2
    exit 1
fi

if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "❌ 工作区有未提交改动，请先 commit" >&2
    git status --short >&2
    exit 1
fi

# --- 确定版本号 ---

if [ $# -ge 1 ]; then
    # 手动指定版本号
    TAG="$1"
else
    # 日期版本：v<YYYYMMDD>-<序号>
    TODAY="$(date -u '+%Y%m%d')"
    BASE="v${TODAY}"

    # 找今天已发布的最大序号，+1
    SEQ=1
    while git rev-parse -q --verify "refs/tags/${BASE}-${SEQ}" >/dev/null; do
        SEQ=$((SEQ + 1))
    done
    TAG="${BASE}-${SEQ}"
fi

# 校验 tag 格式
if [[ ! "$TAG" =~ ^v[0-9]{8}(-[0-9]+)?$ ]] && [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    echo "❌ tag '$TAG' 格式不对" >&2
    echo "   日期格式: v<YYYYMMDD>-<序号>（如 v20260702-1）" >&2
    echo "   或语义化: v<major>.<minor>.<patch>（如 v1.0.0）" >&2
    exit 1
fi

# 检查 tag 是否已存在
if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
    echo "❌ tag '$TAG' 已存在" >&2
    exit 1
fi

# --- 执行 ---

COMMIT="$(git rev-parse --short HEAD)"
echo "==> 准备发布"
echo "    分支:   $BRANCH"
echo "    commit: $COMMIT"
echo "    tag:    $TAG"
echo

# 确认
read -rp "确认打 tag 并 push？(y/N) " CONFIRM
case "$CONFIRM" in
    y|Y|yes) ;;
    *)
        echo "已取消"
        exit 0
        ;;
esac

git tag "$TAG"
echo "✅ 已打 tag: $TAG"

git push origin "$TAG"
echo "✅ 已 push: $TAG"
echo
echo "==> 流水线应已触发，监控构建状态"
echo "    镜像将发布为: micr.cloud.mioffice.cn/agent-getaway/agent-gateway:${TAG}"

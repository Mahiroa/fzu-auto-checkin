#!/bin/sh
# 给当前克隆启用仓库内钩子。
#
# 为什么需要：core.hooksPath 不是随仓库分发的东西（.git/config 不进版本控制），
# 新克隆默认用的是 .git/hooks/ 里的空壳；而本机全局配置可能把它指向 ~/.ai-secret-guard。
# 所以每个克隆都要跑一次这个脚本，钩子才会真的在提交前执行。
#
# 用法：sh ./scripts/install-hooks.sh
# 幂等：重复跑只改一次配置并补执行位。

set -eu

root=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "不在 git 仓库里" >&2
  exit 2
}
cd "$root"

git config core.hooksPath .githooks

# Windows 上执行位没意义，POSIX 上没有执行位的钩子会被 git 静默忽略。
chmod +x .githooks/* 2>/dev/null || true
chmod +x scripts/*.sh 2>/dev/null || true

echo "[install-hooks] core.hooksPath = $(git config --get core.hooksPath)"
for h in .githooks/*; do
  [ -f "$h" ] || continue
  printf '  %-12s %s\n' "$(basename "$h")" "$(head -n 1 "$h" | cut -c1-40)"
done
echo "[install-hooks] 完成：提交前会依次跑 repo-guard → privacy-guard → secret-scan，commit-msg 再扫提交信息"

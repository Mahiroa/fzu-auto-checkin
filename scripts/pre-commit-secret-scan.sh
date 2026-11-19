#!/bin/sh
# pre-commit 本地密钥扫描（betterleaks）
#
# 为什么有这一层：无前缀的随机串肉眼 review 看不出来，CI 有全历史兜底，
# 这里是提交前的闸门。
#
# betterleaks v2 不再有 `protect --staged`：暂存区扫描是把 git diff --cached
# 喂给 `stdin --git-diff`（官方迁移文档给的就是这条命令）。
#
# 本机说明（2026-11-18 实测）：这台机器的 ~/.gitconfig 设了
#   core.hooksPath = ~/.ai-secret-guard/hooks
# 所以仓库内的 .git/hooks/pre-commit **不会被调用**——那个全局钩子会自己在仓库根找
# betterleaks.toml（找不到才退回 ~/.ai-secret-guard/config.toml）。本脚本因此是
# "同一套规则的手动入口"，而不是实际生效的钩子；换一台没有全局配置的机器时，
# 仓库内的钩子会自动接管（它 source 本脚本）。
#
# 工具缺失时只提示、不阻塞（否则换台机器就没法提交）。

root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
cd "$root" || exit 0

bl="$root/.recon/tools/betterleaks.exe"
[ -x "$bl" ] || bl="$root/.recon/tools/betterleaks"
[ -x "$bl" ] || bl=$(command -v betterleaks 2>/dev/null)

if [ -z "$bl" ] || [ ! -x "$bl" ]; then
  echo "[secret-scan] 未找到 betterleaks，跳过本地扫描（CI 仍会全历史扫）"
  exit 0
fi

# 暂存区入口在两个大版本之间换过一次：
#   v1.x：betterleaks git --staged
#   v2.x：git diff --cached | betterleaks stdin --git-diff（v2 删掉了 --staged）
# 所以先问二进制自己支持哪个，别再假设 PATH 里那份和 .recon/tools 里那份同版本——
# 之前正是这个假设让"换台机器/清掉 .recon"之后的钩子直接报 unknown flag。
if "$bl" stdin --help 2>&1 | grep -q -- '--git-diff'; then
  git diff --cached --no-ext-diff --no-textconv --no-color \
    --src-prefix=a/ --dst-prefix=b/ \
    | "$bl" stdin --git-diff --config "$root/betterleaks.toml" --redact
  code=$?
elif "$bl" git --help 2>&1 | grep -q -- '--staged'; then
  "$bl" git --staged --config "$root/betterleaks.toml" --redact
  code=$?
else
  echo "[secret-scan] $bl 既不支持 stdin --git-diff 也不支持 git --staged，跳过本地扫描"
  echo "              （CI 的 secret-scan.yml 仍会全历史扫）"
  exit 0
fi
if [ $code -ne 0 ]; then
  echo ""
  echo "[secret-scan] 暂存区里有疑似密钥（已 REDACT）。确认是合成夹具后，"
  echo "              往 betterleaks.toml 的 prefilter（路径）或 filter（形状）里加豁免，"
  echo "              不要用 git commit -n 绕过。"
fi
exit $code

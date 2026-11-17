#!/bin/sh
# pre-commit 本地密钥扫描（betterleaks）
#
# 为什么有这一层：2026-11-16 把真实 Access-Token 当测试夹具提交过一次，
# 它是无前缀随机串，肉眼 review 看不出来。CI 有全历史兜底，这里是提交前的闸门。
#
# betterleaks v2 不再有 `protect --staged`：暂存区扫描是把 git diff --cached
# 喂给 `stdin --git-diff`（官方迁移文档给的就是这条命令）。
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

git diff --cached --no-ext-diff --no-textconv --no-color \
  --src-prefix=a/ --dst-prefix=b/ \
  | "$bl" stdin --git-diff --config "$root/betterleaks.toml" --redact

code=$?
if [ $code -ne 0 ]; then
  echo ""
  echo "[secret-scan] 暂存区里有疑似密钥（已 REDACT）。确认是合成夹具后，"
  echo "              往 betterleaks.toml 的 prefilter（路径）或 filter（形状）里加豁免，"
  echo "              不要用 git commit -n 绕过。"
fi
exit $code

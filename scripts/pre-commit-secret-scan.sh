#!/bin/sh
# pre-commit 本地密钥扫描（gitleaks）
#
# 为什么有这一层：2026-11-16 把真实 Access-Token 当测试夹具提交过一次，
# 它是无前缀随机串，肉眼 review 看不出来。CI 有全历史兜底，这里是提交前的闸门。
#
# 工具缺失时只提示、不阻塞（否则换台机器就没法提交）。

root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
gl="$root/.recon/tools/gitleaks.exe"
[ -x "$gl" ] || gl="$root/.recon/tools/gitleaks"
[ -x "$gl" ] || gl=$(command -v gitleaks 2>/dev/null)

if [ -z "$gl" ] || [ ! -x "$gl" ]; then
  echo "[secret-scan] 未找到 gitleaks，跳过本地扫描（CI 仍会全历史扫）"
  exit 0
fi

"$gl" protect --staged --config "$root/.gitleaks.toml" --redact --verbose
code=$?
if [ $code -ne 0 ]; then
  echo ""
  echo "[secret-scan] 暂存区里有疑似密钥（已 REDACT）。确认是合成夹具后，"
  echo "              往 .gitleaks.toml 的 allowlist 加「形状」，不要用 git commit -n 绕过。"
fi
exit $code

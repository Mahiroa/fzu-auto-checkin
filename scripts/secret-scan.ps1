# 本地密钥扫描（Windows / PowerShell）
#
# 为什么需要：2026-11-16 那次是把真实 Access-Token 当成测试夹具提交了，
# 而它是"无前缀的随机串"，肉眼和常规 review 都看不出来。CI 里已经有一份全历史扫描
# （.github/workflows/secret-scan.yml），这个脚本是提交前的一道本地闸门。
#
# 用法：
#   ./scripts/secret-scan.ps1            # 扫全历史（等价于 CI）
#   ./scripts/secret-scan.ps1 -Staged    # 只扫已 git add 的内容（提交前用）
#   ./scripts/secret-scan.ps1 -Files .   # 扫工作区文件（--no-git）
#
# 退出码：0 = 没有命中；1 = 命中（提交前应停下来看清楚）。
#
# 安装成 pre-commit 钩子（一次性）：
#   cp scripts/secret-scan.ps1 .git/hooks/  # 或直接运行下面的 install 片段
#   bash -c 'cat > .git/hooks/pre-commit <<EOF
#   #!/bin/sh
#   exec pwsh -NoProfile -File "$(git rev-parse --show-toplevel)/scripts/secret-scan.ps1" -Staged
#   EOF
#   chmod +x .git/hooks/pre-commit'

[CmdletBinding()]
param(
    [switch]$Staged,
    [switch]$Files,
    [string]$ReportPath
)

$ErrorActionPreference = 'Stop'
$root = git rev-parse --show-toplevel
if (-not $root) { throw "不在 git 仓库里" }

# 依次尝试：仓库内的便携版 → PATH
$gitleaks = Join-Path $root '.recon/tools/gitleaks.exe'
if (-not (Test-Path $gitleaks)) {
    $cmd = Get-Command gitleaks -ErrorAction SilentlyContinue
    if ($cmd) { $gitleaks = $cmd.Source }
}
if (-not (Test-Path $gitleaks)) {
    Write-Warning "未找到 gitleaks：放一个到 .recon/tools/gitleaks.exe，或把 gitleaks 加进 PATH。"
    Write-Warning "本地不扫，CI 的全历史扫描仍然有效。"
    exit 0
}

$config = Join-Path $root '.gitleaks.toml'
$args = @()

if ($Staged) {
    $args += @('protect', '--staged')
} else {
    $args += @('detect', '--source', $root)
    if ($Files) { $args += '--no-git' } else { $args += @('--log-opts', '--all') }
}

$args += @('--config', $config, '--redact', '--verbose', '--exit-code', '1')
if ($ReportPath) { $args += @('--report-format', 'json', '--report-path', $ReportPath) }

Push-Location $root
try {
    & $gitleaks @args
    $code = $LASTEXITCODE
} finally {
    Pop-Location
}

if ($code -ne 0) {
    Write-Host ""
    Write-Host "发现疑似密钥（上面已 REDACT）。确认是合成夹具再往 .gitleaks.toml 的 allowlist 里加形状，不要靠 -n 跳过。" -ForegroundColor Yellow
}
exit $code

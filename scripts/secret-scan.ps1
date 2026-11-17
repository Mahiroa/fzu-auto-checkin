# 本地密钥扫描（Windows / PowerShell，betterleaks）
#
# 为什么需要：2026-11-16 那次是把真实 Access-Token 当成测试夹具提交了，
# 而它是"无前缀的随机串"，肉眼和常规 review 都看不出来。CI 里已经有一份全历史扫描
# （.github/workflows/secret-scan.yml），这个脚本是同一套规则的手动入口。
#
# 用法：
#   ./scripts/secret-scan.ps1            # 扫全历史（等价于 CI）
#   ./scripts/secret-scan.ps1 -Staged    # 只扫已 git add 的内容（等同于 pre-commit 钩子）
#   ./scripts/secret-scan.ps1 -Files     # 扫工作区文件（等价于 betterleaks fs .）
#   ./scripts/secret-scan.ps1 -ReportPath .shots/leaks.json
#
# 退出码：0 = 没有命中；1 = 命中（提交前应停下来看清楚）。
#
# 规则与豁免都在仓库根的 betterleaks.toml 里（v2 的 Expr 写法），别在命令行上堆参数。

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
$bl = Join-Path $root '.recon/tools/betterleaks.exe'
if (-not (Test-Path $bl)) {
    $cmd = Get-Command betterleaks -ErrorAction SilentlyContinue
    if ($cmd) { $bl = $cmd.Source }
}
if (-not (Test-Path $bl)) {
    Write-Warning "未找到 betterleaks：放一个到 .recon/tools/betterleaks.exe，或把 betterleaks 加进 PATH。"
    Write-Warning "本地不扫，CI 的全历史扫描仍然有效。"
    exit 0
}

$config = Join-Path $root 'betterleaks.toml'
if (-not (Test-Path $config)) { throw "缺少规则文件：$config" }

Push-Location $root
try {
    if ($Staged) {
        # v2 没有 protect --staged：暂存区扫描 = git diff --cached 喂给 stdin --git-diff。
        # 走 cmd 的字节管道，避免 PowerShell 对象管道破坏补丁文本（中文路径尤其容易出事）。
        $tail = '--git-diff --config "{0}" --redact' -f $config
        if ($ReportPath) { $tail += ' --output "{0}"' -f (Join-Path $root $ReportPath) }
        $line = 'git diff --cached --no-ext-diff --no-textconv --no-color --src-prefix=a/ --dst-prefix=b/ | "{0}" stdin {1}' -f $bl, $tail
        cmd /c $line
        $code = $LASTEXITCODE
    }
    else {
        # betterleaks 的 findings 退出码默认是 1，与 gitleaks 的 --exit-code 1 等价
        $argv = @()
        if ($Files) { $argv += @('fs', '.') } else { $argv += @('git', '.') }
        $argv += @('--config', $config, '--redact')
        if ($ReportPath) { $argv += @('--output', (Join-Path $root $ReportPath)) }
        & $bl @argv
        $code = $LASTEXITCODE
    }
}
finally {
    Pop-Location
}

if ($code -ne 0) {
    Write-Host ""
    Write-Host "发现疑似密钥（上面已 REDACT）。确认是合成夹具后，往 betterleaks.toml 的 prefilter（路径）或 filter（形状）里加豁免，不要靠 -n 跳过。" -ForegroundColor Yellow
}
exit $code

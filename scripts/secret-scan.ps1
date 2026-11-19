# 本地密钥扫描（Windows / PowerShell，betterleaks）
#
# 为什么需要：无前缀的随机串肉眼和常规 review 都看不出来，CI 里已经有一份全历史扫描
# （.github/workflows/secret-scan.yml），这个脚本是同一套规则的手动入口。
#
# 用法：
#   ./scripts/secret-scan.ps1            # 扫全历史（等价于 CI）
#   ./scripts/secret-scan.ps1 -Staged    # 只扫已 git add 的内容（等同于 pre-commit 钩子）
#   ./scripts/secret-scan.ps1 -Files     # 扫工作区文件（等价于 betterleaks fs .）
#   ./scripts/secret-scan.ps1 -ReportPath .shots/leaks.json
#   ./scripts/secret-scan-selftest.ps1   # 反向自检：确认 filter 还放行合成值、仍抓得住高熵串
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
        # 暂存区入口在两个大版本之间换过一次：
        #   v1.x：betterleaks git --staged
        #   v2.x：git diff --cached | betterleaks stdin --git-diff（v2 删掉了 --staged）
        # 先问二进制自己支持哪个，别再假设 PATH 里那份和 .recon/tools 里那份同版本。
        $stdinHelp = (& $bl stdin --help 2>&1 | Out-String)
        if ($stdinHelp -match '--git-diff') {
            # 走 cmd 的字节管道，避免 PowerShell 对象管道破坏补丁文本（中文路径尤其容易出事）。
            $tail = '--git-diff --config "{0}" --redact' -f $config
            if ($ReportPath) { $tail += ' --output "{0}"' -f (Join-Path $root $ReportPath) }
            $line = 'git diff --cached --no-ext-diff --no-textconv --no-color --src-prefix=a/ --dst-prefix=b/ | "{0}" stdin {1}' -f $bl, $tail
            cmd /c $line
            $code = $LASTEXITCODE
        }
        elseif ((& $bl git --help 2>&1 | Out-String) -match '--staged') {
            $argv = @('git', '--staged', '--config', $config, '--redact')
            if ($ReportPath) { $argv += @('--output', (Join-Path $root $ReportPath)) }
            & $bl @argv
            $code = $LASTEXITCODE
        }
        else {
            Write-Warning "这个 betterleaks 既不支持 stdin --git-diff 也不支持 git --staged，跳过本地暂存区扫描（CI 仍会全历史扫）。"
            $code = 0
        }
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

# filter 自检：确认「真·高熵串会被抓到，合成值与中文说明会被放行」
#
# 为什么需要：betterleaks.toml 的 filter 是这套扫描唯一的降噪机制，写宽了会漏真凭据、
# 写窄了会天天报假警。以前靠往树里放一个 .shots/probe/leak.txt 当正样本，但那个目录
# 本身已进 prefilter（本机暂存，不入库），留着文件也不会被扫——于是改成"现造现扫"：
# 在临时目录里生成三行探针，跑一次 betterleaks，断言只命中第一行。
#
# 用法：./scripts/secret-scan-selftest.ps1
# 退出码：0 = filter 行为符合预期；1 = 规则被改坏了（该抓的没抓 / 该放行的被抓）
#
# 探针内容：第一行是高熵无前缀 token（应命中 opaque-token-literal）；第二行带
# SYNTHETIC_ 标记（应被 filter 丢弃）；第三行是中文说明（应被 filter 丢弃）。

[CmdletBinding()]
param([switch]$KeepTemp)

$ErrorActionPreference = 'Stop'
$root = git rev-parse --show-toplevel
if (-not $root) { throw "不在 git 仓库里" }

$bl = Join-Path $root '.recon/tools/betterleaks.exe'
if (-not (Test-Path $bl)) {
    $cmd = Get-Command betterleaks -ErrorAction SilentlyContinue
    if ($cmd) { $bl = $cmd.Source }
}
if (-not (Test-Path $bl)) { throw "未找到 betterleaks：放一个到 .recon/tools/betterleaks.exe，或加进 PATH。" }
$config = Join-Path $root 'betterleaks.toml'
if (-not (Test-Path $config)) { throw "缺少规则文件：$config" }

$tmp = Join-Path $env:TEMP ('bl-selftest-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
$report = Join-Path $tmp 'findings.json'
# 探针值在源码里故意拆成三段（每段 12 字符）：它本身是高熵串，写成一整段的话，
# 扫描工作区时这条规则会先把**这个自检脚本**命中（20 字符起判），于是每次 -Files
# 都要多一条自带噪音。拆开只影响源码里的字面量，落盘生成的探针文件仍是完整一串，
# 所以"该抓的能抓到"这件事依然被真实验证。
$probeValue = 'aB3dE5fG7hI9' + 'jK1mN3pQ5rS7' + 'tU9vW1xY3zA5'
try {
    $probe = @(
        '# 自测探针：第一行应当命中，后两行应当被 filter 放行'
        ('api_key = "' + $probeValue + '"')
        'client_secret = "SYNTHETIC_AND_SHOULD_BE_IGNORED_0123456789"'
        'password = "登录智汇福大 App 的密码"'
    ) -join "`n"
    Set-Content -Path (Join-Path $tmp 'leak.txt') -Value $probe -Encoding utf8

    Push-Location $tmp
    try {
        & $bl fs . --config $config --output $report *> $null
        $code = $LASTEXITCODE
    }
    finally { Pop-Location }

    if (-not (Test-Path $report)) { throw "扫描没有产出报告：$report" }
    $found = (Get-Content $report -Raw | ConvertFrom-Json).findings
    $ids = @($found | ForEach-Object { $_.rule_id })

    $problems = @()
    if (@($found).Count -ne 1) { $problems += "命中数应为 1，实际 $(@($found).Count)（$(($ids -join ', '))）" }
    elseif ($ids[0] -ne 'opaque-token-literal') { $problems += "命中的规则应为 opaque-token-literal，实际 $($ids[0])" }
    if ($code -ne 1) { $problems += "退出码应为 1（有命中），实际 $code" }

    if ($problems.Count -gt 0) {
        Write-Host ("filter 自检失败：" + ($problems -join '；')) -ForegroundColor Red
        Write-Host "报告保留在 $report"
        exit 1
    }
    Write-Host "filter 自检通过：高熵探针命中 1 条，SYNTHETIC_ 与中文说明各放行 1 条。" -ForegroundColor Green
    exit 0
}
finally {
    if ($KeepTemp) { Write-Host "临时目录保留：$tmp" } else { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }
}

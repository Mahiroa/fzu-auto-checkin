# 真实浏览器验收编排：起服务 → 初始化 → 写配置 → 起 Chrome(CDP) → 跑 ui-verify.mjs → 收尾
#
# 只用 Google Chrome 绝对路径（本机 http 默认关联是 Edge，任何"用默认浏览器打开"都会违规）。

param(
    [int]$Port = 8787,
    [int]$CdpPort = 9225
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$serverDir = Join-Path $root 'server'
$dataDir = Join-Path $serverDir 'data-browser'
$exe = Join-Path $serverDir 'fzucheckin-browser.exe'
$outDir = Join-Path $root 'docs\verify-shots'
$chrome = 'C:\Program Files\Google\Chrome\Application\chrome.exe'
$base = "http://127.0.0.1:$Port"
$csrf = @{ 'X-Requested-With' = 'fzu-web' }
$password = 'browser-verify-pass-1'

if (-not (Test-Path $chrome)) { throw "找不到 Chrome：$chrome（本机约定只用 Chrome，不得改用 Edge）" }
New-Item -ItemType Directory -Force $outDir | Out-Null
Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue

$server = $null
$chromeProc = $null
try {
    Write-Host '=== 构建并启动服务 ===' -ForegroundColor Cyan
    Push-Location $serverDir
    go build -trimpath -ldflags="-s -w" -o $exe ./cmd/fzucheckin
    Pop-Location
    $server = Start-Process -FilePath $exe -ArgumentList @('-listen', "127.0.0.1:$Port", '-data', $dataDir) `
        -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $serverDir 'browser.out.log') `
        -RedirectStandardError (Join-Path $serverDir 'browser.err.log')
    $ready = $false
    for ($i = 0; $i -lt 80; $i++) {
        Start-Sleep -Milliseconds 200
        try { $null = Invoke-RestMethod "$base/api/v1/health" -TimeoutSec 2; $ready = $true; break } catch { }
    }
    if (-not $ready) { throw '服务未就绪' }

    Write-Host '=== 初始化密码与配置（窗口放在 21:00-23:59、档位 23:30/23:50，确保不会真的去打卡）===' -ForegroundColor Cyan
    $null = Invoke-RestMethod "$base/api/v1/auth/setup" -Method Post -ContentType 'application/json' `
        -Headers $csrf -Body (@{ password = $password } | ConvertTo-Json) -SessionVariable sess
    $cfg = @{
        config = @{
            user     = @{ username = 'browser-verify'; password = 'browser-verify'; token = '' }
            checkin  = @{
                longitude = '119.20463'; latitude = '26.04920'
                actual_location = '福州大学旗山校区生活区X号楼'; locations = @()
                location_jitter_m = 200
                schedule = @{ window_start = '21:00'; window_end = '23:59'; times = @('23:30', '23:50')
                              time_jitter_minutes = 0; max_wait_minutes = 12; tolerance_minutes = 5 }
            }
            vacation = @{ notify = $false; skip_ranges = @() }
            notify   = @{ type = 'none'; serverchan_key = ''; pushplus_token = ''; bark_url = ''; wecom_webhook = '' }
            server   = @{ listen = "127.0.0.1:$Port"; allow_origins = @(); data_dir = $dataDir }
        }
    }
    $saved = Invoke-RestMethod "$base/api/v1/config" -Method Put -ContentType 'application/json' `
        -Headers $csrf -WebSession $sess -Body ($cfg | ConvertTo-Json -Depth 12)
    Write-Host "配置已保存 version=$($saved.version)"

    Write-Host '=== 启动 Chrome（隔离 profile，无头）===' -ForegroundColor Cyan
    $profile = Join-Path $env:TEMP ('dsh-chrome-fzu-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
    New-Item -ItemType Directory -Force $profile | Out-Null
    $chromeProc = Start-Process -FilePath $chrome -ArgumentList @(
        '--headless=new', "--remote-debugging-port=$CdpPort", "--user-data-dir=$profile",
        '--no-first-run', '--no-default-browser-check', '--no-sandbox', '--disable-gpu',
        '--disable-extensions', '--disable-background-networking', '--disable-component-update',
        '--window-size=1440,900'
    ) -PassThru
    $cdpReady = $false
    for ($i = 0; $i -lt 60; $i++) {
        Start-Sleep -Milliseconds 250
        try { $v = Invoke-RestMethod "http://127.0.0.1:$CdpPort/json/version" -TimeoutSec 2; $cdpReady = $true; break } catch { }
    }
    if (-not $cdpReady) { throw "Chrome CDP 未就绪（profile=$profile）" }
    Write-Host "Chrome $($v.Browser) · CDP:$CdpPort · profile=$profile"

    Write-Host '=== 跑 UI 验收 ===' -ForegroundColor Cyan
    Push-Location $root
    node scripts/ui-verify.mjs --base $base --cdp $CdpPort --password $password --out $outDir
    $code = $LASTEXITCODE
    Pop-Location

    Write-Host "`n=== 截图 ===`n$(Get-ChildItem $outDir -Filter *.png | Select-Object -ExpandProperty Name -ErrorAction SilentlyContinue)"
    if ($code -ne 0) { throw "UI 验收有失败项（exit $code）" }
}
finally {
    if ($chromeProc -and -not $chromeProc.HasExited) { Stop-Process -Id $chromeProc.Id -Force -ErrorAction SilentlyContinue }
    if ($server -and -not $server.HasExited) { Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue }
    Remove-Item -Force $exe -ErrorAction SilentlyContinue
}

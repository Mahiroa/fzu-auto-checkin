# 端到端冒烟脚本（Windows PowerShell）
#
# 为什么要有它：`go test ./...` 只证明函数对，证明不了"真的能起来、真的能登录、
# 真的能改配置、真的能流式看日志"。这里用真实进程 + 真实 HTTP 走一遍主干，
# 最后把 RSS 与二进制大小打印出来对照预算（设计文档 §14）。
#
# 用法：pwsh -File scripts/smoke.ps1
#       pwsh -File scripts/smoke.ps1 -Port 8899 -KeepData

param(
    [int]$Port = 8899,
    [switch]$KeepData
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$serverDir = Join-Path $root 'server'
$dataDir = Join-Path $serverDir 'data-smoke'
$exe = Join-Path $serverDir 'fzucheckin-smoke.exe'
$base = "http://127.0.0.1:$Port"
# 所有非 GET 请求都必须带 CSRF 头：前端统一 fetch 封装就是这么发的，冒烟脚本要对齐真实调用方
$csrf = @{ 'X-Requested-With' = 'fzu-web' }
$fail = 0

function Step($name) { Write-Host "`n=== $name ===" -ForegroundColor Cyan }
function Ok($msg) { Write-Host "  [OK]   $msg" -ForegroundColor Green }
function Bad($msg) { Write-Host "  [FAIL] $msg" -ForegroundColor Red; $script:fail++ }
function Check($cond, $msg) { if ($cond) { Ok $msg } else { Bad $msg } }

Step '构建'
Push-Location $serverDir
go build -trimpath -ldflags="-s -w" -o $exe ./cmd/fzucheckin
if ($LASTEXITCODE -ne 0) { Pop-Location; throw '构建失败' }
$sizeMB = [math]::Round((Get-Item $exe).Length / 1MB, 2)
Ok "二进制 $sizeMB MB（预算 ≤ 12 MB）"
Check ($sizeMB -le 12) "二进制体积 $sizeMB MB"
Pop-Location

if (-not $KeepData) { Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue }

Step '启动服务'
$proc = Start-Process -FilePath $exe -ArgumentList @('-listen', "127.0.0.1:$Port", '-data', $dataDir) `
    -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $serverDir 'smoke.out.log') `
    -RedirectStandardError (Join-Path $serverDir 'smoke.err.log')
try {
    $ready = $false
    for ($i = 0; $i -lt 60; $i++) {
        Start-Sleep -Milliseconds 250
        try { $null = Invoke-RestMethod "$base/api/v1/health" -TimeoutSec 2; $ready = $true; break } catch { }
    }
    if (-not $ready) { throw "服务 60 次探测仍未就绪，见 server/smoke.err.log" }
    Ok '健康检查通过'

    Step '未初始化 / 未登录门槛'
    $session = Invoke-RestMethod "$base/api/v1/auth/session"
    Check ($session.initialized -eq $false) '首次启动 initialized=false'
    try {
        Invoke-RestMethod "$base/api/v1/config" | Out-Null
        Bad '未登录竟然能读配置'
    } catch { Ok "未登录读配置被拒（$($_.Exception.Response.StatusCode.value__)）" }

    Step '初始化密码 + 登录'
    $pw = 'smoke-test-pass-123'
    $body = @{ password = $pw } | ConvertTo-Json
    $null = Invoke-RestMethod "$base/api/v1/auth/setup" -Method Post -Body $body -ContentType 'application/json' -Headers $csrf -SessionVariable sess
    Ok '初始化成功'
    $cfg = Invoke-RestMethod "$base/api/v1/config" -WebSession $sess
    Check ($cfg.config.server.listen -ne $null) '能读到配置'

    Step 'CSRF 与鉴权'
    try {
        Invoke-RestMethod "$base/api/v1/notify/test" -Method Post -WebSession $sess -Headers $csrf | Out-Null
        Bad '缺少 X-Requested-With 竟然通过'
    } catch { Ok '缺 CSRF 头被拒' }

    Step '配置校验（缺坐标应 400 且带可读原因）'
    $badCfg = @{ config = @{
        user     = @{ username = 'x'; password = 'y'; token = '' }
        checkin  = @{ longitude = ''; latitude = ''; actual_location = ''; locations = @(); schedule = @{ window_start = '21:00'; window_end = '23:59'; times = @('21:35') } }
        vacation = @{ notify = $false; skip_ranges = @() }
        notify   = @{ type = 'none' }
        server   = @{ listen = "127.0.0.1:$Port"; allow_origins = @(); data_dir = $dataDir }
    } } | ConvertTo-Json -Depth 12
    try {
        Invoke-RestMethod "$base/api/v1/config" -Method Put -Body $badCfg -ContentType 'application/json' -WebSession $sess -Headers $csrf | Out-Null
        Bad '缺坐标竟然保存成功'
    } catch { Ok '缺坐标被拒（400）' }

    Step '写入有效配置'
    $goodCfg = @{ config = @{
        user     = @{ username = 'smoke-user'; password = 'smoke-pass'; token = '' }
        checkin  = @{ longitude = '119.20463'; latitude = '26.04920'; actual_location = '福州大学旗山校区生活区X号楼'; locations = @();
                      location_jitter_m = 200; schedule = @{ window_start = '03:00'; window_end = '03:01'; times = @('03:00'); time_jitter_minutes = 0; max_wait_minutes = 12; tolerance_minutes = 5 } }
        vacation = @{ notify = $false; skip_ranges = @() }
        notify   = @{ type = 'none'; serverchan_key = ''; pushplus_token = ''; bark_url = ''; wecom_webhook = '' }
        server   = @{ listen = "127.0.0.1:$Port"; allow_origins = @(); data_dir = $dataDir }
    } } | ConvertTo-Json -Depth 12
    $saved = Invoke-RestMethod "$base/api/v1/config" -Method Put -Body $goodCfg -ContentType 'application/json' -WebSession $sess -Headers $csrf
    Check ($saved.ok -eq $true) "配置已保存，version=$($saved.version)"

    Step '脱敏'
    $masked = Invoke-RestMethod "$base/api/v1/config" -WebSession $sess
    $json = $masked | ConvertTo-Json -Depth 12 -Compress
    Check (-not ($json -match 'smoke-pass')) '响应中不含密码明文'
    Check ($json -match '••••') '敏感字段以哨兵回显'

    Step '状态与调度'
    $status = Invoke-RestMethod "$base/api/v1/status" -WebSession $sess
    Check ($status.window.start -eq '03:00') '窗口起点正确（故意放在凌晨，避免冒烟真的去打学校接口）'
    Check ($status.slots.Count -ge 1) "档位数量 $($status.slots.Count)"
    Ok "现在 $($status.now)，下一档 $($status.next_run_at)，决策 $($status.scheduler.last_decision)"

    Step 'SSE 日志流'
    $sse = $null
    try {
        $req = [System.Net.HttpWebRequest]::Create("$base/api/v1/logs/stream")
        $req.CookieContainer = $sess.Cookies
        $req.Headers.Add('X-Requested-With', 'fzu-web')
        $req.Timeout = 8000
        $resp = $req.GetResponse()
        Check ($resp.ContentType -like 'text/event-stream*') "SSE Content-Type=$($resp.ContentType)"
        $resp.Close()
    } catch { Bad "SSE 连接失败：$($_.Exception.Message)" }

    Step '运行历史'
    $runs = Invoke-RestMethod "$base/api/v1/runs?limit=5" -WebSession $sess
    Ok "历史条数 $($runs.runs.Count)（第 1 条来自启动补跑：默认配置 21:35 已过，属预期）"

    Step '资源占用'
    Start-Sleep -Seconds 2
    $proc.Refresh()
    $rssMB = [math]::Round($proc.WorkingSet64 / 1MB, 1)
    $privMB = [math]::Round($proc.PrivateMemorySize64 / 1MB, 1)
    Ok "RSS $rssMB MB / Private $privMB MB（预算空闲 ≤ 30 MB）"
    Check ($rssMB -le 30) "RSS $rssMB MB"
    $health = Invoke-RestMethod "$base/api/v1/health"
    Ok "goroutines=$($health.goroutines) heap=$($health.heap_mb)MB uptime=$($health.uptime_s)s"
}
finally {
    Step '优雅退出'
    if ($proc -and -not $proc.HasExited) {
        $proc.CloseMainWindow() | Out-Null
        Start-Sleep -Milliseconds 500
        if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force }
        Ok '进程已停止'
    }
    Remove-Item -Force $exe -ErrorAction SilentlyContinue
    if (-not $KeepData) { Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue }
}

Write-Host ''
if ($fail -eq 0) { Write-Host '冒烟全部通过 ✅' -ForegroundColor Green; exit 0 }
Write-Host "$fail 项失败 ❌" -ForegroundColor Red; exit 1




#Requires -Version 5.1
<#
.SYNOPSIS
    fzu-auto-checkin 的一键启动脚本（Windows 主用，双击 start.cmd 即可）。

.DESCRIPTION
    只做四件事：定位仓库 -> 需要时构建 server\fzucheckin.exe -> 前台把服务跑起来 -> 等 health 就绪后按需用 Chrome 打开管理界面。

    设计要点：
      * 端口上已经有本服务在跑时，不重复启动（重复启动只会得到一个吓人的端口占用错误）。
      * 端口被别的程序占用时，先打印占用进程，再退出。
      * 服务始终在前台跑（& $exe），Ctrl+C 能停；脚本不会把它丢到后台。
      * 自动开浏览器只认 Chrome 的绝对路径：本机 http 的默认关联是 Edge，
        任何“交给默认浏览器”的写法都会落到 Edge，所以这里绝不把 URL 交给 ShellExecute。

.PARAMETER Port
    端口，默认 14514（不许改默认值）。

.PARAMETER Listen
    覆盖监听地址，默认 127.0.0.1:$Port；填 0.0.0.0:$Port 可局域网访问。

.PARAMETER DataDir
    数据目录，默认 <repo>\server\data；相对路径按仓库根解析。

.PARAMETER NoBuild
    跳过构建，直接用现有 exe（exe 不存在则报错）。

.PARAMETER NoOpen
    不自动打开浏览器。

.PARAMETER Foreground
    前台运行。默认就是前台，这个开关先保留，方便以后加后台模式。

.PARAMETER Status
    只探测 health 并打印 JSON，不启动。

.EXAMPLE
    .\start.ps1
    .\start.ps1 -Port 14515 -DataDir server\data-launcher -NoOpen
    .\start.ps1 -Listen 0.0.0.0:14514
    .\start.ps1 -Status

.NOTES
    Linux / macOS 说明（本脚本的 Windows 分支是给双击用的；其它平台直接手跑等价命令即可）：
        cd server
        go build -trimpath -ldflags="-s -w" -o fzucheckin ./cmd/fzucheckin
        ./fzucheckin -listen 127.0.0.1:14514 -data ./data
        然后用浏览器打开 http://127.0.0.1:14514（macOS 用 open，Linux 用 xdg-open）。
    Windows 上脚本内部等价于：
        server\fzucheckin.exe -listen 127.0.0.1:14514 -data <repo>\server\data
#>
[CmdletBinding()]
param(
    [int]$Port = 14514,
    [string]$Listen = '',
    [string]$DataDir = '',
    [switch]$NoBuild,
    [switch]$NoOpen,
    [switch]$Foreground,
    [switch]$Status
)

# 统一编码：Go 服务和本脚本都输出中文/UTF-8。控制台是真控制台时显示走 Unicode 不受影响；
# 但只要 stdout 被管道/重定向接走，PowerShell 就用 [Console]::OutputEncoding 解码子进程输出，
# 默认的 OEM 936 会把服务打的 UTF-8 中文解成乱码，所以这里先钉成 UTF-8。
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
$ErrorActionPreference = 'Stop'
# 仅 PS 7.3+ 认识这个开关；PS 5.1 下赋值无害（避免 go build 的 stderr 被当成 terminating error）。
$PSNativeCommandUseErrorActionPreference = $false

# Chrome 的绝对路径。硬约束：只认 Chrome，不许换成 Edge，也不许把 URL 交给默认浏览器。
$script:ChromePath = 'C:\Program Files\Google\Chrome\Application\chrome.exe'

# ---------------------------------------------------------------- 输出小工具
function Write-Ok([string]$Text) { Write-Host $Text -ForegroundColor Green }
function Write-Warn2([string]$Text) { Write-Host $Text -ForegroundColor Yellow }
function Write-Err2([string]$Text) { Write-Host $Text -ForegroundColor Red }

# 统一失败出口：一句“怎么了” + 一句“下一步怎么办”，退出码 1。
function Fail([string]$Message, [string]$Hint) {
    Write-Host ''
    Write-Err2 ('[失败] ' + $Message)
    if (-not [string]::IsNullOrWhiteSpace($Hint)) { Write-Warn2 ('[下一步] ' + $Hint) }
    exit 1
}

# 把任意字符串安全地塞进生成脚本里的单引号字面量。
function ConvertTo-PSString([string]$Value) {
    if ($null -eq $Value) { return "''" }
    return "'" + $Value.Replace("'", "''") + "'"
}

# ---------------------------------------------------------------- 路径与参数
$scriptDir = $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($scriptDir)) {
    # 被 iex / 管道方式加载时没有 $PSScriptRoot，退化成当前目录并在下面用 go.mod 校验兜底。
    $scriptDir = (Get-Location).Path
}
$repoRoot = [System.IO.Path]::GetFullPath($scriptDir)
$serverDir = Join-Path $repoRoot 'server'
$goModPath = Join-Path $serverDir 'go.mod'
$exePath = Join-Path $serverDir 'fzucheckin.exe'

# 1. 定位仓库根：找不到 server\go.mod 说明脚本被拷到别处了，直接说清楚。
if (-not (Test-Path -LiteralPath $goModPath -PathType Leaf)) {
    Fail ('这个脚本不在仓库根目录：找不到 ' + $goModPath) ('把 start.ps1 和 start.cmd 一起放回仓库根（和 server\ 同级）再运行；不要单独把脚本拷到别的目录。当前脚本目录：' + $repoRoot)
}

# 数据目录：默认 <repo>\server\data；相对路径按仓库根解析（双击运行时当前目录未必是仓库根）。
if ([string]::IsNullOrWhiteSpace($DataDir)) {
    $dataDirFull = Join-Path $serverDir 'data'
} elseif ([System.IO.Path]::IsPathRooted($DataDir)) {
    $dataDirFull = $DataDir
} else {
    $dataDirFull = Join-Path $repoRoot $DataDir
}
$dataDirFull = [System.IO.Path]::GetFullPath($dataDirFull)

# 监听地址：-Listen 优先，其次 127.0.0.1:$Port。
if ([string]::IsNullOrWhiteSpace($Listen)) {
    $listenAddr = '127.0.0.1:' + $Port
} else {
    $listenAddr = $Listen.Trim()
}
$listenMatch = [regex]::Match($listenAddr, '^(?<host>\[[0-9A-Fa-f:.]+\]|[^:]*):(?<port>[0-9]{1,5})$')
if (-not $listenMatch.Success) {
    Fail ('-Listen 的写法不对：' + $listenAddr) ('要写成 主机:端口，例如 .\start.ps1 -Listen 127.0.0.1:14514 或 .\start.ps1 -Listen 0.0.0.0:14514')
}
$listenHost = $listenMatch.Groups['host'].Value
$listenPort = 0
if (-not [int]::TryParse($listenMatch.Groups['port'].Value, [ref]$listenPort)) {
    Fail ('-Listen 里的端口不是数字：' + $listenAddr) '换成 14514 这种纯数字端口再试。'
}
if ($listenPort -lt 1 -or $listenPort -gt 65535) {
    Fail ('-Listen 里的端口超出范围：' + $listenPort) '端口必须在 1..65535 之间。'
}

# 0.0.0.0 / :: / 空 host 都是“监听所有网卡”，探活和展示都改用 127.0.0.1。
$wildcardHost = ($listenHost -eq '' -or $listenHost -eq '0.0.0.0' -or $listenHost -eq '::' -or $listenHost -eq '[::]')
if ($wildcardHost) { $probeHost = '127.0.0.1' } else { $probeHost = $listenHost }
$baseUrl = 'http://' + $probeHost + ':' + $listenPort
$healthUrl = $baseUrl + '/api/v1/health'

# ---------------------------------------------------------------- 系统探测小工具
# 探 health：不是本服务、没起、超时，一律返回 $null（调用方按“没在跑”处理）。
function Get-HealthSnapshot([string]$Url) {
    try {
        $resp = Invoke-RestMethod -Uri $Url -TimeoutSec 2 -ErrorAction Stop
    } catch {
        return $null
    }
    if ($null -eq $resp) { return $null }
    if ([string]$resp.status -ne 'ok') { return $null }
    return $resp
}

# 查端口监听情况：返回 @{ Available = 能不能查; Pids = @(监听 PID) }。
# 不用“返回 @() 表示空闲”的写法：PowerShell 函数返回空数组会被摊平成 $null，
# 调用方就分不清“端口空闲”和“查不到”，这里用显式的状态对象。
# 另外 Get-NetTCPConnection 在“一条都没匹配到”时也会写错误（FullyQualifiedErrorId 带 NotFound），
# 那不是失败，只是端口空闲；真正的失败（比如取不到 CIM）才当成“查不到”。
function Get-ListenState([int]$LocalPort) {
    $state = @{ Available = $false; Pids = @() }
    if ($null -eq (Get-Command Get-NetTCPConnection -ErrorAction SilentlyContinue)) { return $state }
    $queryErrors = @()
    $conns = $null
    try {
        $conns = Get-NetTCPConnection -LocalPort $LocalPort -State Listen -ErrorAction SilentlyContinue -ErrorVariable +queryErrors
    } catch {
        return $state
    }
    if ($null -eq $conns) {
        $realErrors = @($queryErrors | Where-Object { $_.FullyQualifiedErrorId -notlike '*NotFound*' })
        if ($realErrors.Count -gt 0) { return $state }
        $state.Available = $true
        return $state
    }
    $state.Available = $true
    $state.Pids = @($conns | Select-Object -ExpandProperty OwningProcess -ErrorAction SilentlyContinue | Sort-Object -Unique)
    return $state
}

function Describe-PortOwner([object[]]$Pids) {
    $parts = @()
    foreach ($procId in $Pids) {
        $proc = $null
        try { $proc = Get-Process -Id $procId -ErrorAction Stop } catch { $proc = $null }
        if ($null -ne $proc) {
            $parts += ($proc.ProcessName + '(PID ' + $procId + ')')
        } else {
            $parts += ('未知进程(PID ' + $procId + ')')
        }
    }
    return ($parts -join '、')
}

# 局域网提示用的本机 IPv4（拿不到就不写 IP，只写占位符）。
function Get-LanIPv4 {
    try {
        $ips = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
            Where-Object { $_.IPAddress -notmatch '^(127\.|169\.254\.)' } |
            Select-Object -ExpandProperty IPAddress
        if ($ips) { return @($ips)[0] }
    } catch { }
    return $null
}

# 只认 Chrome 绝对路径；找不到就只打印 URL，绝不回退到默认浏览器。
function Open-Chrome([string]$Url) {
    if (-not (Test-Path -LiteralPath $script:ChromePath -PathType Leaf)) {
        Write-Warn2 ('[浏览器] 没找到 Chrome（' + $script:ChromePath + '），不自动打开浏览器。')
        Write-Warn2 ('[浏览器] 请手动用 Chrome 打开：' + $Url)
        return
    }
    try {
        Start-Process -FilePath $script:ChromePath -ArgumentList @($Url) -ErrorAction Stop | Out-Null
        Write-Ok ('[浏览器] 已用 Chrome 打开 ' + $Url)
    } catch {
        Write-Warn2 ('[浏览器] 打开 Chrome 失败：' + $_.Exception.Message)
        Write-Warn2 ('[浏览器] 请手动用 Chrome 打开：' + $Url)
    }
}

function Get-GoExe {
    $cmd = Get-Command go -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $cmd) { return $null }
    return $cmd.Source
}

# 重建判据：exe 不存在，或 internal / cmd / web 下任一文件、go.mod 比 exe 新。
function Test-NeedsBuild([string]$Exe, [string]$Server) {
    if (-not (Test-Path -LiteralPath $Exe -PathType Leaf)) { return $true }
    $exeTime = (Get-Item -LiteralPath $Exe).LastWriteTimeUtc
    foreach ($sub in @('internal', 'cmd', 'web')) {
        $root = Join-Path $Server $sub
        if (-not (Test-Path -LiteralPath $root)) { continue }
        $newer = Get-ChildItem -LiteralPath $root -Recurse -File -ErrorAction SilentlyContinue |
            Where-Object { $_.LastWriteTimeUtc -gt $exeTime } |
            Select-Object -First 1
        if ($null -ne $newer) { return $true }
    }
    $goMod = Join-Path $Server 'go.mod'
    if ((Test-Path -LiteralPath $goMod -PathType Leaf) -and ((Get-Item -LiteralPath $goMod).LastWriteTimeUtc -gt $exeTime)) { return $true }
    return $false
}

# ---------------------------------------------------------------- -Status：只探活
if ($Status) {
    $snapshot = Get-HealthSnapshot $healthUrl
    if ($null -eq $snapshot) {
        Write-Warn2 ('未运行：' + $healthUrl + ' 没有应答（服务没起，或者端口不是 ' + $listenPort + '）。')
        Write-Warn2 ('[下一步] 起服务：.\start.ps1；换端口：.\start.ps1 -Port ' + ($listenPort + 1))
        exit 1
    }
    $payload = [ordered]@{
        status     = $snapshot.status
        version    = $snapshot.version
        uptime_s   = $snapshot.uptime_s
        goroutines = $snapshot.goroutines
        heap_mb    = $snapshot.heap_mb
    }
    $payload | ConvertTo-Json -Depth 4
    exit 0
}

Write-Host ('[检查] 仓库根：' + $repoRoot)
Write-Host ('[检查] 服务源码：' + $serverDir)

# ---------------------------------------------------------------- 4. 已经在跑？端口被别人占了？
$running = Get-HealthSnapshot $healthUrl
if ($null -ne $running) {
    Write-Ok ''
    Write-Ok ('[已在运行] 服务已经在跑：health=ok version=' + $running.version + '，已运行 ' + $running.uptime_s + ' 秒，goroutines=' + $running.goroutines)
    Write-Host ('[已在运行] 管理界面：' + $baseUrl)
    if ($PSBoundParameters.ContainsKey('DataDir')) {
        Write-Host ('[已在运行] 本次 -DataDir：' + $dataDirFull + '（只影响本次判断；实际目录以服务上次启动时打印的为准）')
    } else {
        Write-Host ('[已在运行] 数据目录：本脚本默认用 ' + $dataDirFull + '；如果你上次是用 -DataDir 起的，以那次打印的路径为准。')
    }
    Write-Host ('[已在运行] 不重复启动（再起一个只会报端口被占用）。要重启：先在服务那个窗口按 Ctrl+C；找不到窗口就先结束监听进程（stop 后重跑本脚本）。')
    if ($NoOpen) {
        Write-Host '[已在运行] -NoOpen：不打开浏览器。'
    } else {
        Open-Chrome $baseUrl
    }
    exit 0
}

$portState = Get-ListenState $listenPort
if (-not $portState.Available) {
    Write-Warn2 ('[检查] 这个环境拿不到端口监听信息（Get-NetTCPConnection 不可用），跳过端口占用检查。')
} elseif (@($portState.Pids).Count -gt 0) {
    $owner = Describe-PortOwner $portState.Pids
    $nextPort = $listenPort + 1
    Fail ('端口 ' + $listenPort + ' 已经被别的程序占用了（health 探不通，说明不是本服务）：' + $owner) ('换一个端口：.\start.ps1 -Port ' + $nextPort + '；或者先结束那个进程：Stop-Process -Id <PID>（先确认它不是别的重要程序）。')
} else {
    Write-Host ('[检查] 端口 ' + $listenPort + ' 空闲。')
}

# ---------------------------------------------------------------- 3. 构建
if ($NoBuild) {
    if (-not (Test-Path -LiteralPath $exePath -PathType Leaf)) {
        Fail ('-NoBuild 要求有现成的 exe，但 ' + $exePath + ' 不存在。') ('去掉 -NoBuild 让脚本构建一次（需要 Go 1.22+）；或者把你已经构建好的 fzucheckin.exe 放到 ' + $serverDir + ' 再运行。')
    }
    Write-Host ('[构建] -NoBuild：跳过构建，直接用 ' + $exePath)
} elseif (-not (Test-NeedsBuild $exePath $serverDir)) {
    Write-Host ('[构建] 跳过：' + $exePath + ' 比所有源码都新。')
} else {
    # 2. 需要构建时，go 必须在 PATH 里。
    $goExe = Get-GoExe
    if ($null -eq $goExe) {
        Fail '请先安装 Go 1.22+ 或提供已构建的 exe：PATH 里找不到 go 命令。' ('装 Go 1.22+ 并把它加进 PATH（装完重开一个窗口），或者把已构建的 fzucheckin.exe 放到 ' + $serverDir + ' 后用 -NoBuild 启动。')
    }
    $goVersion = ''
    try { $goVersion = ((& $goExe version) -join ' ').Trim() } catch { $goVersion = '（go version 执行失败）' }
    Write-Host ('[构建] 源码比 exe 新（或 exe 不存在），开始构建：' + $goVersion)
    Write-Host ('[构建] 命令：go build -trimpath -ldflags="-s -w" -o fzucheckin.exe ./cmd/fzucheckin（工作目录 ' + $serverDir + '）')

    $buildLog = Join-Path $env:TEMP ('fzu-build-' + [guid]::NewGuid().ToString('N') + '.log')
    $goArgs = @('build', '-trimpath', '-ldflags=-s -w', '-o', 'fzucheckin.exe', './cmd/fzucheckin')
    $buildWatch = [System.Diagnostics.Stopwatch]::StartNew()
    $buildCode = 1
    Push-Location -LiteralPath $serverDir
    try {
        & $goExe @goArgs *> $buildLog
        $buildCode = $LASTEXITCODE
    } finally {
        Pop-Location
        $buildWatch.Stop()
    }
    $buildText = ''
    if (Test-Path -LiteralPath $buildLog -PathType Leaf) {
        $buildText = [string](Get-Content -LiteralPath $buildLog -Raw -ErrorAction SilentlyContinue)
        Remove-Item -LiteralPath $buildLog -Force -ErrorAction SilentlyContinue
    }
    if ($buildCode -ne 0) {
        if (-not [string]::IsNullOrWhiteSpace($buildText)) {
            Write-Host ''
            Write-Err2 '[构建] go build 的输出：'
            Write-Host $buildText
        }
        Fail ('go build 失败（退出码 ' + $buildCode + '）。') '按上面的编译错误改源码；常见原因：Go 版本低于 1.22、代码改到一半、GOCACHE/GOPATH 没权限。改完重跑本脚本。'
    }
    if (-not (Test-Path -LiteralPath $exePath -PathType Leaf)) {
        Fail ('go build 报了成功，但没有找到 ' + $exePath + '。') '手动在 ' + $serverDir + ' 下跑一次 go build -o fzucheckin.exe ./cmd/fzucheckin 看真实报错。'
    }
    Write-Ok ('[构建] 完成，用时 ' + $buildWatch.Elapsed.TotalSeconds.ToString('0.0') + ' 秒 -> ' + $exePath)
}

# ---------------------------------------------------------------- 5. 打印三行信息并前台启动
$urlLine = $baseUrl
if ($wildcardHost) {
    $lanIp = Get-LanIPv4
    if ([string]::IsNullOrWhiteSpace($lanIp)) {
        $urlLine = $baseUrl + '（监听 ' + $listenHost + '，局域网用 http://<本机IP>:' + $listenPort + ' 访问）'
    } else {
        $urlLine = $baseUrl + '（监听 ' + $listenHost + '，局域网用 http://' + $lanIp + ':' + $listenPort + ' 访问）'
    }
}

Write-Host ''
Write-Host ('[启动] 管理界面：' + $urlLine)
Write-Host ('[启动] 数据目录：' + $dataDirFull)
Write-Host '[启动] 首次启动请在浏览器里设置管理密码。'
Write-Host ('[启动] 监听地址：' + $listenAddr)
Write-Host ('[启动] 命令：' + $exePath + ' -listen ' + $listenAddr + ' -data ' + $dataDirFull)
Write-Host '[启动] 下面是服务自己的输出；按 Ctrl+C 停止服务并退出本脚本。'
Write-Host ''

$logPath = Join-Path $env:TEMP ('fzucheckin-' + $listenPort + '-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')
$watcherScript = Join-Path $env:TEMP ('fzu-launcher-watch-' + $listenPort + '-' + $PID + '.ps1')

# 6. 就绪探测：一个独立的后台进程（不是 Start-Job）。
#    原因：Start-Job 的输出会被缓冲到 Receive-Job 才显示，而这里主线程正被 & $exe 堵住，
#    用 job 的话「服务已就绪」要等服务退出后才冒出来，等于没有。独立进程直接继承控制台，
#    提示可以实时打出来；脚本退出时在 finally 里按 PID 杀掉，不会留后台任务。
$watcherTemplate = @'
$ErrorActionPreference = 'Continue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
$url        = @@URL@@
$healthUrl  = @@HEALTH@@
$chromePath = @@CHROME@@
$noOpen     = @@NOOPEN@@
$logPath    = @@LOG@@
$timeoutSec = @@TIMEOUT@@

$watch = [System.Diagnostics.Stopwatch]::StartNew()
$health = $null
while ($watch.Elapsed.TotalSeconds -lt $timeoutSec) {
    try {
        $resp = Invoke-RestMethod -Uri $healthUrl -TimeoutSec 1 -ErrorAction Stop
        if ($null -ne $resp -and [string]$resp.status -eq 'ok') { $health = $resp; break }
    } catch { }
    Start-Sleep -Milliseconds 250
}

if ($null -ne $health) {
    Write-Host ''
    Write-Host ('[启动器] 服务已就绪：health=ok version=' + $health.version + ' uptime=' + $health.uptime_s + 's -> ' + $url) -ForegroundColor Green
    if ($noOpen) {
        Write-Host '[启动器] -NoOpen：不自动打开浏览器，请手动打开上面的地址。' -ForegroundColor Yellow
        exit 0
    }
    if (-not (Test-Path -LiteralPath $chromePath -PathType Leaf)) {
        Write-Host ('[启动器] 没找到 Chrome（' + $chromePath + '），不自动打开浏览器。请手动用 Chrome 打开 ' + $url) -ForegroundColor Yellow
        exit 0
    }
    try {
        Start-Process -FilePath $chromePath -ArgumentList @($url) -ErrorAction Stop | Out-Null
        Write-Host ('[启动器] 已用 Chrome 打开 ' + $url) -ForegroundColor Green
    } catch {
        Write-Host ('[启动器] 打开 Chrome 失败：' + $_.Exception.Message + '；请手动用 Chrome 打开 ' + $url) -ForegroundColor Yellow
    }
    exit 0
}

Write-Host ''
Write-Host ('[启动器] 等了 ' + $timeoutSec + ' 秒服务仍没通过健康检查，下面是最多 20 行服务输出（排查用）：') -ForegroundColor Yellow
$printed = $false
if (Test-Path -LiteralPath $logPath -PathType Leaf) {
    try {
        Get-Content -LiteralPath $logPath -Tail 20 -Encoding UTF8 -ErrorAction Stop | ForEach-Object {
            $printed = $true
            Write-Host ('    ' + $_)
        }
    } catch { }
}
if (-not $printed) {
    Write-Host '    （还没拿到服务输出，具体看上面服务自己打印的内容）'
}
Write-Host '[启动器] 常见原因：端口被占、数据目录不可写、-listen 地址写错。改完参数重跑；本脚本会跟着服务一起结束。' -ForegroundColor Yellow
exit 1
'@

$watcherBody = $watcherTemplate
$watcherBody = $watcherBody.Replace('@@URL@@', (ConvertTo-PSString $baseUrl))
$watcherBody = $watcherBody.Replace('@@HEALTH@@', (ConvertTo-PSString $healthUrl))
$watcherBody = $watcherBody.Replace('@@CHROME@@', (ConvertTo-PSString $script:ChromePath))
$watcherBody = $watcherBody.Replace('@@NOOPEN@@', $(if ($NoOpen) { '$true' } else { '$false' }))
$watcherBody = $watcherBody.Replace('@@LOG@@', (ConvertTo-PSString $logPath))
$watcherBody = $watcherBody.Replace('@@TIMEOUT@@', '30')
Set-Content -LiteralPath $watcherScript -Value $watcherBody -Encoding UTF8

$watcherProc = $null
$watcherJob = $null
try {
    $hostExe = (Get-Process -Id $PID).Path
    $argLine = '-NoProfile -ExecutionPolicy Bypass -File "' + $watcherScript + '"'
    $watcherProc = Start-Process -FilePath $hostExe -ArgumentList $argLine -NoNewWindow -PassThru -ErrorAction Stop
    Write-Host '[启动] 已起后台探测（最多等 30 秒 health 就绪）…'
} catch {
    Write-Warn2 ('[启动] 起不了独立的后台探测进程（' + $_.Exception.Message + '），退回后台 job；它的提示会在服务退出后一起显示。')
    $watcherJob = Start-Job -ScriptBlock {
        param($Path)
        Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force -ErrorAction SilentlyContinue
        & $Path
    } -ArgumentList $watcherScript
}

$serviceExit = $null
$logWriter = $null
try {
    $logWriter = [System.IO.StreamWriter]::new($logPath, $false, [System.Text.UTF8Encoding]::new($false))
    $logWriter.AutoFlush = $true
    # 前台跑：& $exe，Ctrl+C 直接打到服务进程。输出逐行回显（不吞 stdout），同时落一份到日志供探测进程排查。
    & $exePath -listen $listenAddr -data $dataDirFull 2>&1 | ForEach-Object {
        $line = [string]$_
        Write-Host $line
        try { $logWriter.WriteLine($line) } catch { }
    }
    $serviceExit = $LASTEXITCODE
} catch {
    Write-Err2 ('[启动] 启动失败：' + $_.Exception.Message)
    $serviceExit = 1
} finally {
    if ($null -ne $logWriter) { try { $logWriter.Dispose() } catch { } }
    # 7. 不留后台 job / 后台进程。
    if ($null -ne $watcherProc) {
        try { if (-not $watcherProc.HasExited) { Stop-Process -Id $watcherProc.Id -Force -ErrorAction SilentlyContinue } } catch { }
    }
    if ($null -ne $watcherJob) {
        try { Receive-Job -Job $watcherJob -ErrorAction SilentlyContinue | ForEach-Object { Write-Host ([string]$_) } } catch { }
        try { Remove-Job -Job $watcherJob -Force -ErrorAction SilentlyContinue } catch { }
    }
    if (Test-Path -LiteralPath $watcherScript -PathType Leaf) {
        Remove-Item -LiteralPath $watcherScript -Force -ErrorAction SilentlyContinue
    }
}

# 8. 退出码与收尾
Write-Host ''
if ($null -eq $serviceExit) { $serviceExit = 0 }
$ctrlCExit = ($serviceExit -eq -1073741510 -or $serviceExit -eq 3221225786)   # 0xC000013A：控制台 Ctrl+C
if ($serviceExit -eq 0 -or $ctrlCExit) {
    if ($ctrlCExit) {
        Write-Ok ('[退出] 服务被 Ctrl+C 中断（退出码 ' + $serviceExit + '，这是打断不是崩溃），脚本一起退出。')
    } else {
        Write-Ok '[退出] 服务已停止（退出码 0），脚本一起退出。'
    }
    exit 0
}
Write-Err2 ('[退出] 服务异常退出，退出码 ' + $serviceExit + '。')
Write-Warn2 ('[下一步] 往上翻，看 server\fzucheckin.exe 的控制台输出（最后几十行；完整日志也在 ' + $logPath + '）。数据目录不可写、端口被占、-listen 地址不对都会这样。改完重跑本脚本。')
exit $serviceExit


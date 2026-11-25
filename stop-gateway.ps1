#Requires -Version 5.1
<#
.SYNOPSIS
    关闭 wb2api（WorkBuddy2API Panel）网关进程，并等端口真正释放。

.DESCRIPTION
    7863/panel 是 wb2api.exe 起的本地 HTTP 服务，面板本身没有停机按钮
    （handler.go 的路由表里只有 /v1/*、/status、/healthz、/panel/、/admin/tasks/{name}/run、/metrics），
    所以只能从进程侧关。本脚本做四件事：

      1. 从 config.json 读 "listen"，解析出端口（缺省 7863），不硬编码；
      2. 找出监听该端口的 PID，并校验它确实是 wb2api（进程名/命令行），不匹配就跳过，绝不误杀；
      3. 结束进程，并清掉 `cmd /c ... wb2api.exe` 那层父 cmd，避免残留空窗口；
      4. 轮询等端口释放（server.shutdown_timeout 缺省 30s，所以默认最多等 45s）。

.PARAMETER Port
    强制指定端口。缺省从 config.json 的 "listen" 解析，解析失败回落 7863。

.PARAMETER All
    除了监听目标端口的实例，把所有 wb2api.exe 进程一起结束（清理崩溃残留/多开的实例）。

.PARAMETER Graceful
    先尝试关闭主窗口（只对带可见控制台的进程有效），失败才终止进程。
    注意：wb2api 通常由 `cmd /c ... >> logs\gateway.log 2>&1` 后台拉起，没有控制台，
    这种情况下 Windows 无法向它发 Ctrl+C，只能 TerminateProcess（在途 SSE 会被硬断）。
    想要真正优雅关停，请到当初那个前台窗口按 Ctrl+C。

.PARAMETER TimeoutSec
    等端口释放的最长时间，默认 45 秒。

.PARAMETER NoWait
    结束进程后不等端口释放，直接返回。

.PARAMETER Quiet
    精简输出（只打印结论，供其它脚本调用）。

.EXAMPLE
    .\stop-gateway.ps1
    默认关闭 config.json 里那个网关。

.EXAMPLE
    .\stop-gateway.ps1 -WhatIf
    只报告"会关掉哪些进程"，不真的动手（安全干跑）。

.EXAMPLE
    .\stop-gateway.ps1 -All -Force:$false -NoWait
    结束所有 wb2api 实例，不等端口。

.NOTES
    退出码：0 = 已停止或本来就没运行；1 = 停止失败（进程仍在 / 端口仍被占用）。
#>
[CmdletBinding()]
param(
    [int]$Port = 0,
    [switch]$All,
    [switch]$Graceful,
    [int]$TimeoutSec = 45,
    [switch]$NoWait,
    [switch]$Quiet,
    # 干跑：只报告会终止哪些进程，不动手。故意不用 SupportsShouldProcess/WhatIf——
    # PowerShell 5.1 在 -WhatIf 传播下会让 CimCmdlets 导入期的别名创建也走 ShouldProcess，
    # 刷出十几行 "设置别名" 噪音，淹掉真正的结论。
    [Alias('WhatIf', 'DryRun')]
    [switch]$WhatIfOnly
)

$ErrorActionPreference = 'Stop'

# -WhatIf 会让模块在导入期创建别名时也走 ShouldProcess，刷出一堆 "设置别名" 噪音。
# 先把 CimCmdlets 模块/别名准备好，再进入主流程，输出就干净了。
try { Import-Module CimCmdlets -ErrorAction SilentlyContinue } catch { }

$script:Root = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
$script:LogDir = Join-Path $script:Root 'logs'
$script:LogFile = Join-Path $script:LogDir ('stop-gateway-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.log')
$script:DefaultPort = 7863
$script:Lines = New-Object System.Collections.Generic.List[string]

function Write-Log {
    param([string]$Message, [string]$Level = 'INFO')
    $line = ('[{0}] {1,-5} {2}' -f (Get-Date -Format 'HH:mm:ss'), $Level, $Message)
    $script:Lines.Add($line)
    if (-not $Quiet) {
        switch ($Level) {
            'OK'   { Write-Host $Message -ForegroundColor Green }
            'WARN' { Write-Host $Message -ForegroundColor Yellow }
            'FAIL' { Write-Host $Message -ForegroundColor Red }
            'DIM'  { Write-Host $Message -ForegroundColor DarkGray }
            default { Write-Host $Message }
        }
    }
}

function Save-Log {
    try {
        if (-not (Test-Path $script:LogDir)) { New-Item -ItemType Directory -Path $script:LogDir -Force | Out-Null }
        # 用 .NET 直写：Set-Content 会被 -WhatIf 拦住，而干跑恰恰最需要留痕
        [System.IO.File]::WriteAllLines($script:LogFile, $script:Lines.ToArray(), (New-Object System.Text.UTF8Encoding($true)))
    } catch {
        # 日志写不进去不该让关闭动作失败
    }
}

function Resolve-GatewayPort {
    param([int]$Explicit)
    if ($Explicit -gt 0) { return @{ Port = $Explicit; Source = '参数 -Port' } }

    $cfg = Join-Path $script:Root 'config.json'
    if (Test-Path $cfg) {
        try {
            $obj = Get-Content -Path $cfg -Raw -Encoding UTF8 | ConvertFrom-Json
            $listen = $obj.listen
            if (-not $listen -and $obj.server -and $obj.server.listen) { $listen = $obj.server.listen }
            if ($listen -match ':(\d{2,5})\s*$') {
                return @{ Port = [int]$Matches[1]; Source = "config.json listen=$listen" }
            }
            Write-Log "config.json 里 listen 字段解析不出端口（值：'$listen'），改用默认 $($script:DefaultPort)" 'WARN'
        } catch {
            Write-Log "config.json 读取/解析失败（$($_.Exception.Message)），改用默认 $($script:DefaultPort)" 'WARN'
        }
    } else {
        Write-Log "找不到 config.json（$cfg），改用默认 $($script:DefaultPort)" 'WARN'
    }
    return @{ Port = $script:DefaultPort; Source = '默认值' }
}

function Get-ListenerPid {
    param([int]$P)
    $pids = @()
    try {
        $pids = @(Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue |
                  Select-Object -ExpandProperty OwningProcess -Unique)
    } catch {
        $pids = @()
    }
    if (-not $pids -or $pids.Count -eq 0) {
        # 兜底：老系统 / NetTCPIP 模块缺失时用 netstat
        $out = & netstat -ano 2>$null | Select-String -Pattern (":{0}\s+.*LISTENING" -f $P)
        foreach ($m in $out) {
            $parts = ($m.Line -split '\s+') | Where-Object { $_ }
            if ($parts.Count -ge 5 -and $parts[-1] -match '^\d+$') { $pids += [int]$parts[-1] }
        }
        $pids = @($pids | Select-Object -Unique)
    }
    return $pids
}

function Get-ProcInfo {
    param([int]$ProcId)
    $ci = Get-CimInstance Win32_Process -Filter "ProcessId=$ProcId" -ErrorAction SilentlyContinue
    if (-not $ci) { return $null }
    return [pscustomobject]@{
        PID          = $ci.ProcessId
        Name         = $ci.Name
        ParentPID    = $ci.ParentProcessId
        CommandLine  = $ci.CommandLine
        CreationDate = $ci.CreationDate
    }
}

function Test-IsGatewayProcess {
    param($Info)
    if (-not $Info) { return $false }
    if ($Info.Name -and $Info.Name -ieq 'wb2api.exe') { return $true }
    if ($Info.CommandLine -and $Info.CommandLine -match '(^|[\\/\s"])wb2api(\.exe)?(["\s]|$)') { return $true }
    return $false
}

function Stop-TargetProcess {
    param([int]$ProcId, [string]$Label)
    $p = Get-Process -Id $ProcId -ErrorAction SilentlyContinue
    if (-not $p) { Write-Log "$Label (PID $ProcId) 已经不存在，跳过" 'DIM'; return $true }

    if ($WhatIfOnly) {
        Write-Log "[DRY-RUN] 将终止 $Label (PID $ProcId)" 'WARN'
        return $true
    }

    if ($Graceful) {
        try {
            if ($p.MainWindowHandle -ne 0 -and $p.CloseMainWindow()) {
                Write-Log "$Label 已请求关闭主窗口，等 3 秒确认..." 'DIM'
                if ($p.WaitForExit(3000)) { Write-Log "$Label 已优雅退出（PID $ProcId）" 'OK'; return $true }
                Write-Log "$Label 未响应窗口关闭，改为终止进程" 'WARN'
            } else {
                Write-Log "$Label 没有可交互控制台窗口，Windows 无法发 Ctrl+C，改为终止进程" 'DIM'
            }
        } catch { }
    }

    try {
        Stop-Process -Id $ProcId -Force -ErrorAction Stop
    } catch {
        Write-Log "终止 $Label (PID $ProcId) 失败：$($_.Exception.Message)" 'FAIL'
        return $false
    }

    try { $p.WaitForExit(5000) | Out-Null } catch { }
    $still = Get-Process -Id $ProcId -ErrorAction SilentlyContinue
    if ($still) { Write-Log "$Label (PID $ProcId) 仍在运行" 'FAIL'; return $false }
    Write-Log "$Label 已终止（PID $ProcId）" 'OK'
    return $true
}

function Wait-PortFree {
    param([int]$P, [int]$Seconds)
    if ($Seconds -le 0) { return (Test-PortFree -P $P) }
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-PortFree -P $P) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return (Test-PortFree -P $P)
}

function Test-PortFree {
    param([int]$P)
    $l = @(Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue)
    return ($l.Count -eq 0)
}

# ---------------------------------------------------------------- main
try {
    if (-not $Quiet) {
        Write-Host ''
        Write-Host '=== 关闭 wb2api 网关 ===' -ForegroundColor Cyan
    }

    $isWhatIf = [bool]$WhatIfOnly
    if ($isWhatIf -and -not $Quiet) { Write-Log '— DRY-RUN（-WhatIf）：只报告会做什么，不终止任何进程 —' 'WARN' }

    $resolved = Resolve-GatewayPort -Explicit $Port
    $targetPort = [int]$resolved.Port
    Write-Log "目标端口：$targetPort（来源：$($resolved.Source)）" 'DIM'

    $listenerPids = @(Get-ListenerPid -P $targetPort)
    $candidates = New-Object System.Collections.Generic.List[object]

    foreach ($lp in $listenerPids) {
        $info = Get-ProcInfo -ProcId $lp
        if (-not $info) { continue }
        if (Test-IsGatewayProcess -Info $info) {
            $candidates.Add([pscustomobject]@{ Info = $info; Reason = "监听 $targetPort" })
        } else {
            Write-Log "端口 $targetPort 被 $($info.Name) (PID $lp) 占用，但它不是 wb2api —— 不动它" 'WARN'
            Write-Log "  命令行：$($info.CommandLine)" 'DIM'
        }
    }

    if ($All) {
        $others = @(Get-Process -Name 'wb2api' -ErrorAction SilentlyContinue | Where-Object { $listenerPids -notcontains $_.Id })
        foreach ($o in $others) {
            $info = Get-ProcInfo -ProcId $o.Id
            if ($info -and -not ($candidates | Where-Object { $_.Info.PID -eq $info.PID })) {
                $candidates.Add([pscustomobject]@{ Info = $info; Reason = '-All：非监听实例' })
            }
        }
    }

    if ($candidates.Count -eq 0) {
        Write-Log "端口 $targetPort 上没有 wb2api，网关本来就没在运行。" 'OK'
        Write-Log "（若想清理崩溃残留进程，用 -All 再跑一次）" 'DIM'
        Save-Log
        if (-not $Quiet) { Write-Host '' }
        exit 0
    }

    foreach ($c in $candidates) {
        $i = $c.Info
        Write-Log "发现 $($i.Name) PID $($i.PID)（$($c.Reason)），父进程 PID $($i.ParentPID)"
        Write-Log "  命令行：$($i.CommandLine)" 'DIM'
    }

    # 收集父 cmd（`cmd /c cd /d ... && wb2api.exe ...`）——子进程退出后它通常自动退，退不掉再清
    $parentCmds = @()
    foreach ($c in $candidates) {
        $pi = $c.Info
        if ($pi.ParentPID -and $pi.ParentPID -ne 0) {
            $pin = Get-ProcInfo -ProcId $pi.ParentPID
            if ($pin -and $pin.Name -ieq 'cmd.exe' -and $pin.CommandLine -match 'wb2api') {
                $parentCmds += $pin
            }
        }
    }

    $failed = @()
    foreach ($c in $candidates) {
        if (-not (Stop-TargetProcess -ProcId $c.Info.PID -Label $c.Info.Name)) {
            $failed += $c.Info.PID
        }
    }

    # 清父 cmd
    foreach ($pc in ($parentCmds | Sort-Object PID -Unique)) {
        Start-Sleep -Milliseconds 400
        $alive = Get-Process -Id $pc.PID -ErrorAction SilentlyContinue
        if ($alive) {
            if (Stop-TargetProcess -ProcId $pc.PID -Label 'cmd.exe(父壳)') { } else { }
        } else {
            Write-Log "父 cmd.exe (PID $($pc.PID)) 已随子进程退出" 'DIM'
        }
    }

    if ($isWhatIf) {
        Write-Log 'DRY-RUN：跳过端口释放等待（进程并未真的被终止）' 'DIM'
        $free = $true
    } elseif ($NoWait) {
        Write-Log '跳过端口释放等待（-NoWait）' 'DIM'
        $free = Test-PortFree -P $targetPort
    } else {
        Write-Log "等待端口 $targetPort 释放（最多 $TimeoutSec 秒）..."
        $free = Wait-PortFree -P $targetPort -Seconds $TimeoutSec
    }

    Write-Host ''
    if ($isWhatIf) {
        Write-Log "结论：DRY-RUN —— 会终止 $($candidates.Count) 个网关实例（含父 cmd 壳），端口 $targetPort 将被释放（未实际执行）" 'OK'
        Save-Log
        exit 0
    }
    if ($failed.Count -gt 0 -or -not $free) {
        if ($failed.Count -gt 0) { Write-Log "以下 PID 未能终止：$($failed -join ', ')" 'FAIL' }
        if (-not $free) {
            $left = @(Get-ListenerPid -P $targetPort)
            Write-Log "端口 $targetPort 仍被占用（PID：$($left -join ', ')）" 'FAIL'
        }
        Write-Log '结论：关闭失败' 'FAIL'
        Save-Log
        exit 1
    }

    Write-Log "结论：网关已关闭，端口 $targetPort 已释放，/panel/ 不再响应。" 'OK'
    Write-Log "重启：双击「启动网关.cmd」，或在仓库根执行 wb2api.exe -config config.json" 'DIM'
    Write-Log "日志：$($script:LogFile)" 'DIM'
    Save-Log
    if (-not $Quiet) { Write-Host '' }
    exit 0
}
catch {
    Write-Log "脚本异常：$($_.Exception.Message)" 'FAIL'
    Write-Log $_.ScriptStackTrace 'DIM'
    Save-Log
    exit 1
}

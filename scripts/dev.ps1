# dev.ps1 — 本地开发：构建网关二进制并在前台运行（PowerShell 版）。
#
# 用途：开发者本地调试网关。编译二进制，然后直接运行，
# 监听 4096 端口。需要本地已装 Go。
#
# 参数/环境变量：
#   -Port      监听端口（默认 4096，或 $env:PORT）
#   -Token     网关 Bearer token（默认空 = 不鉴权，或 $env:TOKEN）
#   -DataDir   会话数据目录（默认空，或 $env:DATA_DIR）
#
# 用法：
#   .\scripts\dev.ps1
#   .\scripts\dev.ps1 -Port 8080 -Token secret
#   $env:TOKEN='secret'; .\scripts\dev.ps1
[CmdletBinding()]
param(
    [string]$Port    = $(if ($env:PORT)     { $env:PORT }     else { '4096' }),
    [string]$Token   = $(if ($env:TOKEN)    { $env:TOKEN }    else { '' }),
    [string]$DataDir = $(if ($env:DATA_DIR) { $env:DATA_DIR } else { '' })
)

$ErrorActionPreference = 'Stop'
Set-Location -Path (Join-Path $PSScriptRoot '..')

$App  = 'acg'
$Bin  = Join-Path 'bin' "$App.exe"

# 版本信息注入（与 Makefile / dev.sh 保持一致）
$Version   = if ($env:VERSION) { $env:VERSION } else { 'dev' }
$Commit    = if (git rev-parse --short HEAD 2>$null) { (git rev-parse --short HEAD) } else { 'none' }
$BuildTime = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$LdFlags   = "-s -w -X main.version=$Version -X main.commit=$Commit -X main.buildTime=$BuildTime"

Write-Host "==> 构建网关二进制 ($Bin)"
$env:CGO_ENABLED = '0'
go build -ldflags $LdFlags -o $Bin ./cmd/gateway
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

Write-Host "==> 启动网关 (端口 $Port)"
Write-Host "    Token: $(if ($Token) { $Token } else { '<无，开发模式不鉴权>' })"
Write-Host '    按 Ctrl+C 停止'
Write-Host ''

$args = @('-port', $Port)
if ($Token)   { $args += @('-token', $Token) }
if ($DataDir) { $args += @('-data-dir', $DataDir) }

& $Bin @args

param(
    [string]$InnoCompiler = "$env:LOCALAPPDATA\Programs\Inno Setup 7\ISCC.exe"
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$stage = Join-Path $root 'dist\PhoneBridge'
$scrcpySource = Join-Path $root 'third_party\scrcpy\scrcpy-win64-v4.1'

if (-not (Test-Path -LiteralPath $InnoCompiler)) {
    throw "Inno Setup compiler was not found: $InnoCompiler"
}

New-Item -ItemType Directory -Path $stage -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stage 'runtime\scrcpy') -Force | Out-Null

Push-Location $root
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed; installer was not created.' }
    & go build -buildvcs=false '-ldflags=-H=windowsgui' -o (Join-Path $stage 'PhoneBridge.exe') .\cmd\phonebridge
    if ($LASTEXITCODE -ne 0) { throw 'PhoneBridge Windows EXE build failed.' }
    Copy-Item -Path (Join-Path $scrcpySource '*') -Destination (Join-Path $stage 'runtime\scrcpy') -Recurse -Force
    & $InnoCompiler (Join-Path $root 'installer\PhoneBridge.iss')
    if ($LASTEXITCODE -ne 0) { throw 'Inno Setup installer build failed.' }
} finally {
    Pop-Location
}

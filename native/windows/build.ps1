[CmdletBinding()]
param(
    [ValidateSet('Release', 'Debug')][string]$Configuration = 'Release',
    [ValidateSet('x64', 'ARM64')][string]$Architecture = 'x64',
    [string]$BuildDirectory = (Join-Path $PSScriptRoot 'build'),
    [string]$Generator = 'Visual Studio 17 2022',
    [switch]$RunAcceptance,
    [switch]$RequireHardware
)
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'This script requires Windows, MSVC and the Windows SDK.' }
if (-not (Get-Command cmake -ErrorAction SilentlyContinue)) { throw 'CMake 3.24+ is required; this script does not install tools.' }
& cmake -S $PSScriptRoot -B $BuildDirectory -G $Generator -A $Architecture '-DBUILD_TESTING=ON'
if ($LASTEXITCODE -ne 0) { throw 'CMake configuration failed' }
& cmake --build $BuildDirectory --config $Configuration --parallel
if ($LASTEXITCODE -ne 0) { throw 'C++ build failed' }
& ctest --test-dir $BuildDirectory -C $Configuration --output-on-failure
if ($LASTEXITCODE -ne 0) { throw 'Protocol core tests failed' }
$Helper = Join-Path $BuildDirectory "$Configuration/vantry-remote-helper.exe"
if (-not (Test-Path $Helper)) { throw 'Expected helper executable was not produced' }
Write-Output "Built: $Helper"
if ($RunAcceptance) {
    $TestArgs = @((Join-Path $PSScriptRoot 'tests/acceptance.py'), '--helper', $Helper, '--artifacts', (Join-Path $BuildDirectory 'acceptance'))
    if ($RequireHardware) { $TestArgs += '--require-hardware' }
    & python @TestArgs
    if ($LASTEXITCODE -ne 0) { throw 'Pattern/decode acceptance failed or required hardware is unavailable' }
}

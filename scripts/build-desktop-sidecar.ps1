$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$desktopRoot = Join-Path $repoRoot "desktop"
$binaryDir = Join-Path $desktopRoot "src-tauri\binaries"
$targetTriple = "x86_64-pc-windows-msvc"
$output = Join-Path $binaryDir ("moonbridge-" + $targetTriple + ".exe")

New-Item -ItemType Directory -Force $binaryDir | Out-Null
Push-Location $repoRoot
try {
  go build -trimpath -o $output ./cmd/moonbridge
  $buildInfo = go version -m $output | Out-String
  if ($buildInfo -notmatch "vcs.revision") {
    throw "sidecar build is missing VCS metadata"
  }
  Write-Host "Built $output"
} finally {
  Pop-Location
}

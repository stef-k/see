$version   = "v1.0.2"
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$targets = @(
    @{ GOOS="windows"; GOARCH="amd64"; Ext=".exe" },
    @{ GOOS="linux";   GOARCH="amd64"; Ext="" },
    @{ GOOS="linux";   GOARCH="arm64"; Ext="" },
    @{ GOOS="darwin";  GOARCH="amd64"; Ext="" },
    @{ GOOS="darwin";  GOARCH="arm64"; Ext="" }
)

# Backup env vars
$oldGOOS   = $env:GOOS
$oldGOARCH = $env:GOARCH

foreach ($t in $targets) {
    $goos   = $t["GOOS"]
    $goarch = $t["GOARCH"]
    $ext    = $t["Ext"]

    $binary  = "see$ext"
    $zipName = "see_${version}_${goos}_${goarch}.zip"

    Write-Host "→ Building $zipName"

    $env:GOOS   = $goos
    $env:GOARCH = $goarch

    if (Test-Path $binary) { Remove-Item $binary -Force }

    go build -ldflags "-X main.version=$version -X main.buildDate=$buildDate" -o $binary

    if (Test-Path $zipName) { Remove-Item $zipName -Force }
    Compress-Archive -Path $binary -DestinationPath $zipName -Force

    Remove-Item $binary -Force
}

# Restore env vars
$env:GOOS   = $oldGOOS
$env:GOARCH = $oldGOARCH

Write-Host "`n✅ Build complete! Each platform build is zipped separately."

param(
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$Dist = Join-Path $Root "dist"
New-Item -ItemType Directory -Force -Path $Dist | Out-Null
Get-ChildItem $Dist -File -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -Like "docker-control-*" -or $_.Name -eq "SHA256SUMS.txt" } |
    Remove-Item -Force

$Targets = @(
    @{ OS = "windows"; Arch = "amd64"; Extension = ".exe" },
    @{ OS = "linux"; Arch = "amd64"; Extension = "" },
    @{ OS = "linux"; Arch = "arm64"; Extension = "" }
)

Push-Location $Root
try {
    foreach ($Target in $Targets) {
        $env:GOOS = $Target.OS
        $env:GOARCH = $Target.Arch
        $env:CGO_ENABLED = "0"
        $Name = "docker-control-$($Version)-$($Target.OS)-$($Target.Arch)$($Target.Extension)"
        go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $Dist $Name) ./cmd/docker-control
        if ($LASTEXITCODE -ne 0) { throw "go build failed for $Name" }
    }
    Get-ChildItem $Dist -File |
        Where-Object Name -Like "docker-control-*" |
        Get-FileHash -Algorithm SHA256 |
        ForEach-Object { "$($_.Hash.ToLower())  $([IO.Path]::GetFileName($_.Path))" } |
        Set-Content -Encoding ascii (Join-Path $Dist "SHA256SUMS.txt")
}
finally {
    Pop-Location
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

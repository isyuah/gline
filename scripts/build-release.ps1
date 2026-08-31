[CmdletBinding()]
param(
    [string]$Version = "dev",
    [string]$OutputDirectory = "dist/release"
)

$ErrorActionPreference = "Stop"
$repositoryRoot = Split-Path -Parent $PSScriptRoot
$outputPath = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot $OutputDirectory))
$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Suffix = ".exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Suffix = "" }
)
$binaries = @(
    @{ Package = "./cmd/agent"; Name = "gline-agent" },
    @{ Package = "./cmd/server"; Name = "gline-server" }
)
$environmentNames = @("CGO_ENABLED", "GOOS", "GOARCH")
$previousEnvironment = @{}
$artifactPaths = @()
foreach ($name in $environmentNames) {
    $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name)
}

try {
    Push-Location $repositoryRoot
    New-Item -ItemType Directory -Force -Path $outputPath | Out-Null
    foreach ($target in $targets) {
        $env:CGO_ENABLED = "0"
        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        foreach ($binary in $binaries) {
            $name = "{0}_{1}_{2}{3}" -f $binary.Name, $target.GOOS, $target.GOARCH, $target.Suffix
            $destination = Join-Path $outputPath $name
            $ldflags = "-s -w"
            if ($binary.Name -eq "gline-server") {
                $ldflags = "-s -w -X main.version=$Version"
            }
            & go build -trimpath -ldflags $ldflags -o $destination $binary.Package
            if ($LASTEXITCODE -ne 0) {
                throw "go build failed for $name"
            }
            $artifactPaths += $destination
        }
    }
    $checksums = $artifactPaths |
        Sort-Object |
        ForEach-Object {
            $file = Get-Item -LiteralPath $_
            $hash = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            "{0}  {1}" -f $hash, $file.Name
        }
    $checksums | Set-Content -LiteralPath (Join-Path $outputPath "SHA256SUMS") -Encoding ascii
    Write-Output "Release artifacts written to $outputPath"
}
finally {
    Pop-Location
    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name])
    }
}

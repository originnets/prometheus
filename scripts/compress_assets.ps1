# PowerShell equivalent of scripts/compress_assets.sh
# Generates web/ui/embed.go with gzip-compressed static assets.

$ErrorActionPreference = "Stop"

$UiDir = Join-Path $PSScriptRoot ".." | Join-Path -ChildPath "web" | Join-Path -ChildPath "ui"
$UiDir = (Resolve-Path $UiDir).Path
$StaticDir = Join-Path $UiDir "static"

Set-Location $UiDir

# Start embed.go from the template.
Copy-Item "embed.go.tmpl" "embed.go" -Force

# Remove any previously generated .gz files.
Get-ChildItem -Path $StaticDir -Recurse -Filter '*.gz' -File | Remove-Item -Force

Add-Type -AssemblyName System.IO.Compression.FileSystem

function Compress-File {
    param([string]$Source, [string]$Destination)
    $srcBytes = [System.IO.File]::ReadAllBytes($Source)
    $destStream = [System.IO.File]::Create($Destination)
    try {
        $gz = New-Object System.IO.Compression.GZipStream($destStream, [System.IO.Compression.CompressionMode]::Compress, $false)
        try {
            $gz.Write($srcBytes, 0, $srcBytes.Length)
        } finally {
            $gz.Dispose()
        }
    } finally {
        $destStream.Dispose()
    }
}

# Compress every non-.gz file in static/.
$filesToCompress = Get-ChildItem -Path $StaticDir -Recurse -File | Where-Object { $_.Extension -ne '.gz' }
$staticPrefixLen = $StaticDir.Length + 1  # +1 for trailing slash
foreach ($file in $filesToCompress) {
    $relativePath = $file.FullName.Substring($staticPrefixLen).Replace('\', '/')
    $dest = Join-Path $StaticDir ($relativePath + '.gz')
    $destDir = Split-Path $dest -Parent
    if (-not (Test-Path $destDir)) { New-Item -ItemType Directory -Force -Path $destDir | Out-Null }
    Compress-File -Source $file.FullName -Destination $dest
}

# Collect all .gz paths (relative to UiDir, forward-slash), sorted.
$gzFiles = Get-ChildItem -Path $StaticDir -Recurse -Filter '*.gz' -File |
    Sort-Object FullName |
    ForEach-Object {
        $rel = $_.FullName.Substring($UiDir.Length + 1).Replace('\', '/')
        $rel
    }

# Append go:embed directives and the EmbedFS variable.
$lines = New-Object System.Collections.Generic.List[string]
foreach ($p in $gzFiles) { $lines.Add("//go:embed $p") }
$lines.Add('var EmbedFS embed.FS')
Add-Content -Path (Join-Path $UiDir 'embed.go') -Value $lines -Encoding utf8

Write-Host "Generated embed.go with $($gzFiles.Count) embedded files at $UiDir\embed.go"

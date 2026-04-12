# Packages the release/ folder into a zip for GitHub Releases.
param(
    [string]$Version = (git describe --tags --always 2>$null) ?? "dev"
)

$Out = "dist\usenet-pipeline-$Version.zip"

New-Item -ItemType Directory -Force -Path dist | Out-Null

# Remove old zip if it exists
if (Test-Path $Out) { Remove-Item $Out }

Compress-Archive -Path `
    "release\docker-compose.yml", `
    "release\.env.example", `
    "release\install.sh", `
    "release\install.bat" `
    -DestinationPath $Out

Write-Host "Packaged: $Out"
Write-Host ""
Write-Host "To create a GitHub release:"
Write-Host "  gh release create v$Version $Out --title `"v$Version`" --notes `"See install instructions in the zip.`""

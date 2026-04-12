#!/usr/bin/env bash
# Packages the release/ folder into a zip for GitHub Releases.
set -euo pipefail

VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo "dev")}"
OUT="dist/usenet-pipeline-${VERSION}.zip"

mkdir -p dist

# Create zip with the release files (flat, no parent directory)
cd release
zip -r "../${OUT}" \
    docker-compose.yml \
    .env.example \
    install.sh \
    install.bat
cd ..

echo "Packaged: ${OUT}"
echo ""
echo "To create a GitHub release:"
echo "  gh release create v${VERSION} ${OUT} --title \"v${VERSION}\" --notes \"See install instructions in the zip.\""

#!/bin/bash
# Usage: ./scripts/release.sh v0.2.0
#
# The version is derived from the git tag at build time (see Makefile and
# .goreleaser.yaml), so a release is just an annotated tag — no VERSION file to
# bump.

set -e

VERSION="$1"

if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 v0.2.0"
    exit 1
fi

case "$VERSION" in
    v*) ;;
    *) echo "Version should look like v0.2.0 (with leading 'v')"; exit 1 ;;
esac

if git rev-parse "$VERSION" >/dev/null 2>&1; then
    echo "Tag $VERSION already exists"
    exit 1
fi

git tag -a "$VERSION" -m "Release $VERSION"

echo "Tagged $VERSION (version is derived from the tag at build time)"
echo "Run 'git push origin main && git push origin $VERSION' to release"

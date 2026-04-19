#!/bin/bash
# Usage: ./scripts/release.sh v0.1.0

set -e

VERSION="$1"

if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 v0.1.0"
    exit 1
fi

# Strip 'v' prefix for VERSION file
VERSION_NUM="${VERSION#v}"

# Update VERSION file
echo "$VERSION_NUM" > VERSION

# Commit and tag
git add VERSION
git commit -m "Bump version to $VERSION"
git tag "$VERSION"

echo "Version bumped to $VERSION"
echo "Run 'git push origin main && git push origin $VERSION' to release"

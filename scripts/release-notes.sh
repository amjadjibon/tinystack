#!/bin/sh
# Prints Markdown release notes for a tag, grouped from the conventional commit subjects since the previous tag.
# Usage: scripts/release-notes.sh v0.0.2 (needs the full history and tags, as in a fetch-depth: 0 checkout).
set -eu

tag=${1:?usage: release-notes.sh <tag>}
repo=${GITHUB_REPOSITORY:-amjadjibon/tinystack}
previous=$(git describe --tags --abbrev=0 --match 'v*' "$tag^" 2>/dev/null || true)

# section prints the commits whose subject type matches the pattern, with the type prefix removed.
section() {
	title=$1 pattern=$2
	lines=$(git log --no-merges --format='%s (%h)' "$range" | grep -E "^($pattern)(\([^)]*\))?!?: " |
		sed -E 's/^[a-z]+(\(([^)]*)\))?!?: /- **\2:** /; s/^- \*\*:\*\* /- /' || true)
	[ -n "$lines" ] && printf '### %s\n\n%s\n\n' "$title" "$lines"
	return 0
}

if [ -z "$previous" ]; then
	printf 'First release of TinyStack.\n\n'
else
	range="$previous..$tag"
	section Features feat
	section Fixes fix
	section Performance perf
	section Documentation docs
	section 'Other changes' 'refactor|test|build|ci|chore|style'
fi

version=${tag#v}
cat <<EOF
### Install

\`\`\`sh
curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | TINYSTACK_VERSION=$version sh
docker run --rm -p 4566:4566 -p 4567:4567 ghcr.io/$repo:$version
\`\`\`

An installed binary upgrades itself with \`tinystack update\`.
EOF
if [ -n "$previous" ]; then
	printf '\n**Full changelog:** https://github.com/%s/compare/%s...%s\n' "$repo" "$previous" "$tag"
fi

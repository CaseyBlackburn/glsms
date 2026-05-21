#!/usr/bin/env bash
# Cut a release: validate, tag, and push to trigger the GoReleaser workflow.
#
# Usage:
#   ./scripts/release.sh v0.1.0
#   ./scripts/release.sh v0.1.0 -m "First public release"
#   ./scripts/release.sh v0.1.0-rc.1     # pre-release
#   ./scripts/release.sh v0.1.0 --skip-build --force
set -euo pipefail

REMOTE="origin"
MESSAGE=""
SKIP_BUILD=0
FORCE=0
VERSION=""

usage() {
    cat >&2 <<EOF
usage: $0 <version> [-m MESSAGE] [--remote NAME] [--skip-build] [--force]

  <version>       e.g. v0.1.0 or v0.1.0-rc.1
  -m, --message   annotated-tag message (default: "Release <version>")
  --remote NAME   git remote to push to (default: origin)
  --skip-build    skip the local 'go build' sanity check
  --force         skip interactive confirmations
EOF
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -m|--message)    MESSAGE="$2"; shift 2 ;;
        --remote)        REMOTE="$2"; shift 2 ;;
        --skip-build)    SKIP_BUILD=1; shift ;;
        --force)         FORCE=1; shift ;;
        -h|--help)       usage ;;
        -*)              echo "unknown flag: $1" >&2; usage ;;
        *)
            if [[ -z "$VERSION" ]]; then VERSION="$1"; else echo "extra arg: $1" >&2; usage; fi
            shift ;;
    esac
done

[[ -n "$VERSION" ]] || usage

fail()    { echo "error: $*" >&2; exit 1; }
confirm() {
    [[ $FORCE -eq 1 ]] && return 0
    read -r -p "$1 [y/N] " ans
    [[ "$ans" =~ ^[yY] ]]
}

# 1. Version format.
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    fail "version must look like v1.2.3 or v1.2.3-rc.1 (got '$VERSION')"
fi

# 2. Inside a git repo, at its root.
REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || fail "not inside a git repository"
cd "$REPO_ROOT"

# 3. Working tree clean.
if [[ -n "$(git status --porcelain)" ]]; then
    git status --short
    fail "working tree is dirty — commit or stash first"
fi

# 4. On the main branch (warn-only override).
BRANCH=$(git rev-parse --abbrev-ref HEAD)
if [[ "$BRANCH" != "main" ]]; then
    echo "warning: not on 'main' (current: $BRANCH)" >&2
    confirm "tag and release from '$BRANCH' anyway?" || exit 1
fi

# 5. Up to date with the remote.
git fetch --tags "$REMOTE" >/dev/null 2>&1 || true
LOCAL=$(git rev-parse HEAD)
if REMOTE_HEAD=$(git rev-parse "$REMOTE/$BRANCH" 2>/dev/null); then
    if [[ "$LOCAL" != "$REMOTE_HEAD" ]]; then
        echo "warning: HEAD differs from $REMOTE/$BRANCH" >&2
        confirm "continue anyway?" || exit 1
    fi
fi

# 6. Tag must not already exist (locally or on the remote).
if git tag --list "$VERSION" | grep -q .; then
    fail "tag $VERSION already exists locally"
fi
if git ls-remote --tags "$REMOTE" "refs/tags/$VERSION" | grep -q .; then
    fail "tag $VERSION already exists on $REMOTE"
fi

# 7. Build sanity check.
if [[ $SKIP_BUILD -eq 0 ]]; then
    echo "building ./cmd/glsms ..."
    TMP_BIN=$(mktemp -u -t glsms-release-check.XXXXXX)
    if ! go build -o "$TMP_BIN" ./cmd/glsms; then
        fail "build failed — fix before tagging"
    fi
    rm -f "$TMP_BIN"
fi

# 8. Confirm and tag.
TAG_MSG=${MESSAGE:-"Release $VERSION"}
echo
echo "  version: $VERSION"
echo "  commit:  $LOCAL"
echo "  branch:  $BRANCH"
echo "  remote:  $REMOTE"
echo "  message: $TAG_MSG"
echo
confirm "create and push tag $VERSION?" || exit 1

git tag -a "$VERSION" -m "$TAG_MSG"

if ! git push "$REMOTE" "$VERSION"; then
    echo "push failed — local tag still exists. Remove with: git tag -d $VERSION" >&2
    exit 1
fi

# 9. Point at the Actions run.
ORIGIN_URL=$(git remote get-url "$REMOTE")
if [[ "$ORIGIN_URL" =~ [:/]([^/:]+)/([^/]+?)(\.git)?$ ]]; then
    SLUG="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
    echo
    echo "released $VERSION"
    echo "  actions:  https://github.com/$SLUG/actions"
    echo "  releases: https://github.com/$SLUG/releases"
fi

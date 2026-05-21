#!/usr/bin/env pwsh
# Cut a release: validate, tag, and push to trigger the GoReleaser workflow.
#
# Usage:
#   ./scripts/release.ps1 v0.1.0
#   ./scripts/release.ps1 v0.1.0 -Message "First public release"
#   ./scripts/release.ps1 v0.1.0-rc.1   # pre-release (marked as such by GoReleaser)

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$Version,

    [string]$Message,

    [string]$Remote = "origin",

    [switch]$SkipBuild,

    [switch]$Force
)

$ErrorActionPreference = "Stop"

function Fail($msg) {
    Write-Host "error: $msg" -ForegroundColor Red
    exit 1
}

function Confirm($prompt) {
    if ($Force) { return $true }
    $answer = Read-Host "$prompt [y/N]"
    return $answer -match '^[yY]'
}

# 1. Version format: vMAJOR.MINOR.PATCH with optional pre-release suffix.
if ($Version -notmatch '^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$') {
    Fail "version must look like v1.2.3 or v1.2.3-rc.1 (got '$Version')"
}

# 2. Inside a git repo, at its root.
$repoRoot = (git rev-parse --show-toplevel 2>$null)
if ($LASTEXITCODE -ne 0) { Fail "not inside a git repository" }
Set-Location $repoRoot

# 3. Working tree clean.
$dirty = git status --porcelain
if ($dirty) {
    Write-Host $dirty
    Fail "working tree is dirty — commit or stash first"
}

# 4. On the main branch (warn-only override).
$branch = git rev-parse --abbrev-ref HEAD
if ($branch -ne "main") {
    Write-Host "warning: not on 'main' (current: $branch)" -ForegroundColor Yellow
    if (-not (Confirm "tag and release from '$branch' anyway?")) { exit 1 }
}

# 5. Up to date with the remote.
git fetch --tags $Remote 2>&1 | Out-Null
$local  = git rev-parse HEAD
$remote = git rev-parse "$Remote/$branch" 2>$null
if ($LASTEXITCODE -eq 0 -and $local -ne $remote) {
    Write-Host "warning: HEAD differs from $Remote/$branch" -ForegroundColor Yellow
    if (-not (Confirm "continue anyway?")) { exit 1 }
}

# 6. Tag must not already exist (locally or on the remote).
if (git tag --list $Version) { Fail "tag $Version already exists locally" }
$remoteTag = git ls-remote --tags $Remote "refs/tags/$Version"
if ($remoteTag) { Fail "tag $Version already exists on $Remote" }

# 7. Build sanity check.
if (-not $SkipBuild) {
    Write-Host "building ./cmd/glsms ..." -ForegroundColor Cyan
    go build -o (Join-Path ([IO.Path]::GetTempPath()) "glsms-release-check.exe") ./cmd/glsms
    if ($LASTEXITCODE -ne 0) { Fail "build failed — fix before tagging" }
}

# 8. Confirm and tag.
$tagMessage = if ($Message) { $Message } else { "Release $Version" }
Write-Host ""
Write-Host "  version: $Version"
Write-Host "  commit:  $local"
Write-Host "  branch:  $branch"
Write-Host "  remote:  $Remote"
Write-Host "  message: $tagMessage"
Write-Host ""
if (-not (Confirm "create and push tag $Version?")) { exit 1 }

git tag -a $Version -m $tagMessage
if ($LASTEXITCODE -ne 0) { Fail "git tag failed" }

git push $Remote $Version
if ($LASTEXITCODE -ne 0) {
    Write-Host "push failed — local tag still exists. Remove with: git tag -d $Version" -ForegroundColor Yellow
    exit 1
}

# 9. Point at the Actions run.
$originUrl = git remote get-url $Remote
if ($originUrl -match '[:/]([^/:]+)/([^/]+?)(\.git)?$') {
    $slug = "$($Matches[1])/$($Matches[2])"
    Write-Host ""
    Write-Host "released $Version" -ForegroundColor Green
    Write-Host "  actions: https://github.com/$slug/actions"
    Write-Host "  releases: https://github.com/$slug/releases"
}

#!/usr/bin/env bash
# scripts/version.sh — derive the next release version from commit history.
#
# Versioning scheme (conventional commits, no release PRs — every push to main
# yields a release):
#   commit with "!" type or "BREAKING CHANGE:" footer  -> +1 major
#   feat: / feature:                                    -> +1 minor
#   fix: / perf: (or anything else)                     -> +1 patch
#
# Usage:
#   scripts/version.sh next     # print next version, e.g. v1.8.0 (default)
#   scripts/version.sh current  # print latest tag, e.g. v1.7.0 (v1.7.0 if none)
#   scripts/version.sh explain  # show which commits drive the bump
#
# The script never writes to the repo; CI tags v<next> and injects it into the
# binary with -ldflags.
set -euo pipefail

# Base version when the repo has no tags yet (matches the last hardcoded
# GatewayVersion before release automation).
BASE_VERSION="1.7.0"

latest_tag() {
  git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo ""
}

current() {
  local tag
  tag="$(latest_tag)"
  if [ -z "$tag" ]; then
    echo "v${BASE_VERSION}"
  else
    echo "$tag"
  fi
}

semver_bump() {
  # $1 = current version (vX.Y.Z), $2 = major|minor|patch -> prints vX.Y.Z'
  local ver="$1" bump="$2"
  local major minor patch
  IFS='.' read -r major minor patch <<<"${ver#v}"
  case "$bump" in
    major) major=$((major + 1)); minor=0; patch=0 ;;
    minor) minor=$((minor + 1)); patch=0 ;;
    patch) patch=$((patch + 1)) ;;
  esac
  echo "v${major}.${minor}.${patch}"
}

# Classify a single commit message; prints major|minor|patch|none
classify() {
  local msg="$1" first_line
  first_line="$(printf '%s' "$msg" | head -n 1)"
  # Breaking: "feat!:" style or a BREAKING CHANGE: footer anywhere in the body
  if printf '%s' "$msg" | grep -qiE '(^|[[:space:]])BREAKING[ -]CHANGE' || \
     printf '%s' "$first_line" | grep -qE '^[a-zA-Z]+(\([^)]*\))?!:'; then
    echo "major"
    return
  fi
  if printf '%s' "$first_line" | grep -qiE '^feat(ure)?(\([^)]*\))?:'; then
    echo "minor"
    return
  fi
  echo "none" # fallback patch handled by caller (push with no bump-worthy commits still releases a patch)
}

next() {
  local tag range bump="none"
  tag="$(latest_tag)"
  if [ -z "$tag" ]; then
    range="HEAD"
    [ "$(git rev-list --count HEAD 2>/dev/null || echo 0)" -eq 0 ] && { echo "v${BASE_VERSION}"; return; }
  else
    range="${tag}..HEAD"
    if [ -z "$(git rev-list "${tag}..HEAD" 2>/dev/null)" ]; then
      echo "$tag" # nothing new since the tag — no release
      return
    fi
  fi

  local msg c
  while IFS= read -r c; do
    msg="$(git log -1 --pretty=%B "$c")"
    c_out="$(classify "$msg")"
    case "$c_out" in
      major) bump="major"; break ;;
      minor) [ "$bump" != "major" ] && bump="minor" ;;
    esac
  done < <(git rev-list $range 2>/dev/null)

  if [ "$bump" = "none" ]; then
    bump="patch" # default: every push to main releases (patch bump)
  fi
  semver_bump "$(current)" "$bump"
}

explain() {
  local tag range
  tag="$(latest_tag)"
  if [ -z "$tag" ]; then
    echo "No tags yet — basing next version on v${BASE_VERSION}."
    range="HEAD"
  else
    echo "Latest tag: $tag"
    range="${tag}..HEAD"
  fi
  echo "Commits considered:"
  local msg c c_out
  while IFS= read -r c; do
    msg="$(git log -1 --pretty=%B "$c")"
    c_out="$(classify "$msg")"
    printf '  %-5s %s  %s\n' "$c_out" "${c:0:8}" "$(printf '%s' "$msg" | head -n 1)"
  done < <(git rev-list $range 2>/dev/null)
  echo "Next version: $(next)"
}

case "${1:-next}" in
  next) next ;;
  current) current ;;
  explain) explain ;;
  *) echo "usage: $0 [next|current|explain]" >&2; exit 2 ;;
esac

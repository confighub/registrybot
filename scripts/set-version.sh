#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# Sets the registrybot release the deployments name, before that release is
# tagged: the image tag in manifests/registrybot.yaml, which the config bundle
# publishes, and in the installer package in package/, whose version is the
# release's without the "v", as package/README.md's commands pull it. Commit
# the change and merge it, then push the tag. release.yml fails for a tag the
# deployments don't name.
#
# With --check it changes nothing and fails when the deployments name another
# release.
#
# Needs yq, diff, patch and perl.
#
# Usage: scripts/set-version.sh [--check] vX.Y.Z

set -euo pipefail

check=false
if [ "${1:-}" = "--check" ]; then
  check=true
  shift
fi
tag="${1:-}"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "usage: $0 [--check] vX.Y.Z" >&2; exit 2; }

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pkg="${repo}/package"
readme="${pkg}/README.md"
manifest="${repo}/manifests/registrybot.yaml"
export IMAGE=ghcr.io/confighub/registrybot TAG="$tag" VERSION="${tag#v}"

if $check; then
  wrong=0
  grep -q "image: ${IMAGE}:${tag}\$" "$manifest" || { echo "manifests/registrybot.yaml does not run ${IMAGE}:${tag}" >&2; wrong=1; }
  [ "$(yq -r '.images[] | select(.name == strenv(IMAGE)) | .newTag' "${pkg}/bases/default/kustomization.yaml")" = "$tag" ] \
    || { echo "package/bases/default/kustomization.yaml does not pin ${tag}" >&2; wrong=1; }
  [ "$(yq -r .installerMetadata.version "${pkg}/installer.yaml")" = "$VERSION" ] \
    || { echo "package/installer.yaml is not version ${VERSION}" >&2; wrong=1; }
  ! grep -E "packages/registrybot:[0-9]" "$readme" | grep -qv "packages/registrybot:${VERSION} " \
    || { echo "package/README.md does not pull version ${VERSION}" >&2; wrong=1; }
  [ "$wrong" = 0 ] || { echo "Run scripts/set-version.sh ${tag}, merge it, and tag again." >&2; exit 1; }
  echo "The deployments run ${IMAGE}:${tag}."
  exit 0
fi

# yq_edit <file> <expression>: applies a yq expression in place, keeping the
# file's blank lines, which yq drops: only yq's other changes are patched in.
yq_edit() {
  local file=$1 expression=$2 changes
  changes="$(yq "$expression" "$file" | diff -B "$file" - || true)"
  [ -z "$changes" ] || printf '%s\n' "$changes" | patch -s "$file"
}

perl -pi -e 's{^(\s*image: \Q$ENV{IMAGE}\E):v[0-9]+\.[0-9]+\.[0-9]+$}{$1:$ENV{TAG}}' "$manifest"
yq_edit "${pkg}/bases/default/kustomization.yaml" '(.images[] | select(.name == strenv(IMAGE))).newTag = strenv(TAG)'
yq_edit "${pkg}/installer.yaml" '.installerMetadata.version = strenv(VERSION)'
perl -pi -e 's{(packages/registrybot):[0-9]+\.[0-9]+\.[0-9]+ }{$1:$ENV{VERSION} }g' "$readme"

"$0" --check "$tag"

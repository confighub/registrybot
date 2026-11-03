#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# Renders the installer package in package/ the way an operator would, with
# `installer setup`, for each preset, and checks the renders it has to refuse.
# The package's validators (vet-schemas, vet-images, vet-pod-security-standard,
# ...) run in every render. It also checks that the package runs the release
# it is versioned as, as manifests/registrybot.yaml does, and that its
# Deployment has the environment that manifests/registrybot.yaml's has.
#
# Needs installer 0.3.0 or later (or $INSTALLER), and yq on PATH, and no
# ConfigHub server.
#
# Usage: scripts/test-package.sh

set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pkg="${repo}/package"
installer="${INSTALLER:-installer}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

common=(--non-interactive --namespace registrybot)

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# The package's version is the registrybot release it runs, and the plain
# manifest runs the same one.
version="$(yq -r .installerMetadata.version "${pkg}/installer.yaml")"
"${repo}/scripts/set-version.sh" --check "v${version}" >/dev/null || fail "the package and manifests/registrybot.yaml name different releases"

# render <case> <installer setup args...> renders the package.
render() {
  local name=$1
  shift
  "$installer" setup --pull "$pkg" --work-dir "${work}/${name}" "${common[@]}" "$@" \
    > "${work}/${name}.log" 2>&1
}

# env_of <case> <variable>: the variable's value in registrybot's environment.
env_of() {
  yq -r ".spec.template.spec.containers[0].env[] | select(.name == \"$2\") | .value" \
    "${work}/$1/out/manifests/deployment-registrybot-registrybot.yaml"
}

# env_list <file>: the environment of the registrybot Deployment in file,
# sorted by name, one variable per line.
env_list() {
  yq -o json -I 0 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env | sort_by(.name) | .[] | sort_keys(..)' "$1"
}

# secret_keys_of <case>: the registrybot-secrets keys the Deployment reads.
secret_keys_of() {
  yq -r '[.spec.template.spec.containers[0].env[].valueFrom.secretKeyRef.key | select(. != null)] | join(",")' \
    "${work}/$1/out/manifests/deployment-registrybot-registrybot.yaml"
}

# expect_render <case> <components> <installer setup args...>: the render
# succeeds and selects exactly those components.
expect_render() {
  local name=$1 want=$2
  shift 2
  if ! render "$name" "$@"; then
    fail "${name}: render failed"
    sed 's/^/  /' "${work}/${name}.log" >&2
    return
  fi
  local out="${work}/${name}/out"
  local components
  components="$(yq -r '(.spec.components // []) | join(",")' "${out}/record/selection.yaml")"
  [ "$components" = "$want" ] || fail "${name}: selected '${components}', want '${want}'"
  [ -z "$(ls -A "${out}/secrets" 2>/dev/null)" ] || fail "${name}: rendered a Secret; registrybot-secrets is supplied out of band"
  echo "ok: ${name}"
}

# expect_refusal <case> <message> <installer setup args...>: the render fails,
# saying message.
expect_refusal() {
  local name=$1 want=$2
  shift 2
  if render "$name" "$@"; then
    fail "${name}: rendered, want a refusal saying: ${want}"
    return
  fi
  if ! grep -qF -- "$want" "${work}/${name}.log"; then
    fail "${name}: refused without saying: ${want}"
    sed 's/^/  /' "${work}/${name}.log" >&2
    return
  fi
  echo "ok: ${name}"
}

connected=(--input config_space=registry-facts --input confighub_url=https://hub.example.com)
expect_render base ""
expect_render default "" --components default "${connected[@]}"
expect_render all webhooks --components all "${connected[@]}"
expect_render webhooks webhooks --select webhooks "${connected[@]}"

# Rendered with no inputs, as a base, the Deployment has the plain manifest's
# environment.
if ! diff -u <(env_list "${repo}/manifests/registrybot.yaml") \
  <(env_list "${work}/base/out/manifests/deployment-registrybot-registrybot.yaml") >&2; then
  fail "base: the environment differs from manifests/registrybot.yaml"
fi
for v in REGISTRYBOT_CONFIG_SPACE:registry-facts CONFIGHUB_URL:https://hub.example.com; do
  [ "$(env_of default "${v%%:*}")" = "${v#*:}" ] || fail "default: ${v%%:*} is $(env_of default "${v%%:*}"), want ${v#*:}"
done
# Webhooks are off unless the component adds their secret.
[ "$(secret_keys_of default)" = CONFIGHUB_AUTH_PRIVATE_KEY,GITHUB_TOKEN ] ||
  fail "default: reads registrybot-secrets keys $(secret_keys_of default)"
[ "$(secret_keys_of webhooks)" = CONFIGHUB_AUTH_PRIVATE_KEY,GITHUB_TOKEN,GITHUB_WEBHOOK_SECRET ] ||
  fail "webhooks: reads registrybot-secrets keys $(secret_keys_of webhooks)"
expect_refusal latest-image vet-images \
  --set-image ghcr.io/confighub/registrybot=ghcr.io/confighub/registrybot:latest

if [ "$failures" -gt 0 ]; then
  echo "${failures} check(s) failed" >&2
  exit 1
fi
echo "All package checks passed."

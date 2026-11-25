#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# Renders the installer package in package/ the way an operator would, with
# `installer setup`, for each preset, and checks the renders it has to refuse.
# The package's validators (vet-schemas, vet-images, vet-pod-security-standard,
# ...) run in every render. It also checks that the package runs the release
# it is versioned as, as manifests/registrybot.yaml does, and that its
# Deployment has the environment that manifests/registrybot.yaml's has, and
# that every render gives registrybot exactly one configuration source.
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
# One of config-configmap and config-unit is always selected: the ConfigMap
# unless config-unit is asked for.
expect_render base config-configmap
expect_render default config-configmap --components default "${connected[@]}"
expect_render all config-configmap,webhooks --components all "${connected[@]}"
expect_render webhooks config-configmap,webhooks --select webhooks "${connected[@]}"
expect_render unit config-unit --select config-unit "${connected[@]}"
expect_refusal both-sources 'alternatives in group "config-source"' \
  --select config-configmap --select config-unit

# Rendered with no inputs, as a base, the Deployment has the plain manifest's
# environment.
if ! diff -u <(env_list "${repo}/manifests/registrybot.yaml") \
  <(env_list "${work}/base/out/manifests/deployment-registrybot-registrybot.yaml") >&2; then
  fail "base: the environment differs from manifests/registrybot.yaml"
fi
[ "$(env_of default CONFIGHUB_URL)" = https://hub.example.com ] || fail "default: CONFIGHUB_URL is $(env_of default CONFIGHUB_URL)"

# The configuration document comes from one place. registrybot refuses to start
# with both a file and a unit, so a render must never set both.
# config_file_of <file>: the configuration document in the registrybot-config
# ConfigMap in file, without the configHub block ConfigHub strips from it.
config_file_of() {
  yq 'select(.kind == "ConfigMap" and .metadata.name == "registrybot-config") | .data."registrybot.yaml"' "$1" |
    yq 'del(.configHub) | ... comments=""'
}
# mount_of <case>: the ConfigMap mounted at /etc/registrybot.
mount_of() {
  yq -r '.spec.template.spec as $p | $p.containers[0].volumeMounts[] | select(.mountPath == "/etc/registrybot") | .name as $n | $p.volumes[] | select(.name == $n) | .configMap.name' \
    "${work}/$1/out/manifests/deployment-registrybot-registrybot.yaml" 2>/dev/null
}
for c in base default all webhooks; do
  [ "$(env_of "$c" REGISTRYBOT_CONFIG_FILE)" = /etc/registrybot/registrybot.yaml ] ||
    fail "${c}: REGISTRYBOT_CONFIG_FILE is $(env_of "$c" REGISTRYBOT_CONFIG_FILE)"
  [ -z "$(env_of "$c" REGISTRYBOT_CONFIG_SPACE)$(env_of "$c" REGISTRYBOT_CONFIG_UNIT)" ] ||
    fail "${c}: names a configuration unit as well as a file"
  [ "$(mount_of "$c")" = registrybot-config ] || fail "${c}: /etc/registrybot mounts '$(mount_of "$c")', want registrybot-config"
  cm="${work}/${c}/out/manifests/configmap-registrybot-registrybot-config.yaml"
  [ "$(yq -r '.metadata.annotations."confighub.com/toolchain"' "$cm")" = AppConfig/YAML ] ||
    fail "${c}: the registrybot-config ConfigMap is not marked AppConfig/YAML"
done
[ "$(env_of unit REGISTRYBOT_CONFIG_SPACE)" = registry-facts ] || fail "unit: REGISTRYBOT_CONFIG_SPACE is $(env_of unit REGISTRYBOT_CONFIG_SPACE), want registry-facts"
[ "$(env_of unit REGISTRYBOT_CONFIG_UNIT)" = registrybot ] || fail "unit: REGISTRYBOT_CONFIG_UNIT is $(env_of unit REGISTRYBOT_CONFIG_UNIT), want registrybot"
[ -z "$(env_of unit REGISTRYBOT_CONFIG_FILE)" ] || fail "unit: names a configuration file as well as a unit"
[ -z "$(mount_of unit)" ] || fail "unit: mounts a configuration ConfigMap"
[ ! -e "${work}/unit/out/manifests/configmap-registrybot-registrybot-config.yaml" ] || fail "unit: renders the registrybot-config ConfigMap"
# The plain manifest starts from the same document as the package.
if ! diff -u <(config_file_of "${repo}/manifests/registrybot.yaml") \
  <(config_file_of "${work}/base/out/manifests/configmap-registrybot-registrybot-config.yaml") >&2; then
  fail "base: the configuration document differs from manifests/registrybot.yaml"
fi
# Webhooks are off unless the component adds their secret.
[ "$(secret_keys_of default)" = CONFIGHUB_AUTH_PRIVATE_KEY,GITHUB_TOKEN ] ||
  fail "default: reads registrybot-secrets keys $(secret_keys_of default)"
[ "$(secret_keys_of webhooks)" = CONFIGHUB_AUTH_PRIVATE_KEY,GITHUB_TOKEN,GITHUB_WEBHOOK_SECRET ] ||
  fail "webhooks: reads registrybot-secrets keys $(secret_keys_of webhooks)"
# Metrics are on unless the metrics input turns them off.
[ "$(env_of base METRICS_ENABLED)" = true ] || fail "base: METRICS_ENABLED is $(env_of base METRICS_ENABLED), want true"
# ports_of <case>: the names of the registrybot container's ports.
ports_of() {
  yq -r '[.spec.template.spec.containers[0].ports[].name] | join(",")' \
    "${work}/$1/out/manifests/deployment-registrybot-registrybot.yaml"
}
[ "$(ports_of base)" = http,metrics ] || fail "base: ports are $(ports_of base), want http,metrics"
expect_render no-metrics config-configmap --input metrics=false
[ "$(env_of no-metrics METRICS_ENABLED)" = false ] || fail "no-metrics: METRICS_ENABLED is $(env_of no-metrics METRICS_ENABLED), want false"
[ "$(ports_of no-metrics)" = http ] || fail "no-metrics: ports are $(ports_of no-metrics), want http"
expect_refusal latest-image vet-images \
  --set-image ghcr.io/confighub/registrybot=ghcr.io/confighub/registrybot:latest

if [ "$failures" -gt 0 ]; then
  echo "${failures} check(s) failed" >&2
  exit 1
fi
echo "All package checks passed."

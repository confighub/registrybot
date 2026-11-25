# registrybot installer package

An [installer](https://github.com/confighub/installer) package for registrybot, published as `oci://ghcr.io/confighub/packages/registrybot:<version>`.

The base holds all of registrybot:
- its Namespace (Pod Security Standards restricted)
- a ServiceAccount with no token mounted: registrybot needs no Kubernetes API access
- the single-replica Deployment of `manifests/registrybot.yaml`, at a pinned image tag, with registrybot's environment in its `env`, reading its credentials from the `registrybot-secrets` Secret, which is not part of the package
- a ClusterIP Service in front of the webhook receiver and `/status`

One of two components says where registrybot's [configuration document](../README.md#where-the-document-comes-from) comes from. They are alternatives; `config-configmap` is selected unless you select `config-unit`.

| Component | Adds |
|---|---|
| `config-configmap` (default) | The document as `registrybot.yaml` in the `registrybot-config` ConfigMap, mounted at `/etc/registrybot`. Uploaded, it is the AppConfig/YAML Unit `registrybot-config` in the install's Space, which ConfigHub renders into the ConfigMap's Unit, `registrybot-config-rendered`. Edit `registrybot-config` and publish the Space; the pod is replaced when the document changes. |
| `config-unit` | `REGISTRYBOT_CONFIG_SPACE` and `REGISTRYBOT_CONFIG_UNIT`: registrybot reads the document from a ConfigHub Unit itself, every poll cycle, with no publish. The Unit is yours to create. |
| `webhooks` | `GITHUB_WEBHOOK_SECRET` from `registrybot-secrets`, which turns on `POST /webhooks/github`. Exposing the Service to GitHub is up to the cluster. |

A render fails validation if:
- the image is `:latest` or untagged
- the pod doesn't meet the Pod Security Standards restricted level

### registrybot's environment

registrybot's variables are in its Deployment's `env`, as in `manifests/registrybot.yaml`, and are described in the [README](../README.md#configuration); `scripts/test-package.sh` fails when the two differ. Change one in ConfigHub with `set-env`, for example:

```sh
cub function do --space registrybot-my-cluster --unit registrybot \
  set-env registrybot REGISTRYBOT_POLL_INTERVAL=10m
```

## Install

registrybot needs the following before installing it, set up as under [Setup](../README.md#setup):
- a service account, and a key of it (`cub serviceaccount key add --generate`)
- its grants on the Spaces its fact Units go to
- with `config-unit`: its configuration Unit, and its grants on that Unit's Space

Installing takes cub v0.11.19 or later, which has the installer built in. The credentials never go to ConfigHub: put them in the `registrybot-secrets` Secret in the install namespace yourself, or with your secret store, under the keys `CONFIGHUB_AUTH_PRIVATE_KEY`, `GITHUB_TOKEN`, and, with the `webhooks` component, `GITHUB_WEBHOOK_SECRET`.

```sh
cub package install registrybot:0.5.3 --space registrybot-my-cluster --namespace registrybot
kubectl create namespace registrybot
kubectl create secret generic registrybot-secrets -n registrybot \
  --from-file=CONFIGHUB_AUTH_PRIVATE_KEY=$HOME/.confighub/keys/registrybot.jwk \
  --from-literal=GITHUB_TOKEN=ghp_...
```

Add `--select webhooks` to receive webhooks, and `--select config-unit` to have registrybot read its configuration from a ConfigHub Unit.

`registrybot:0.5.3` is short for `oci://ghcr.io/confighub/packages/registrybot:0.5.3`; without a version, cub installs the newest release. Installing again into the same Space, at the same or a newer version, keeps the answers given before, so it is also how to upgrade.

The document the package ships watches nothing and sends fact Units to the Space `registrybot`. After the install, say what to watch in the `registrybot-config` Unit:

```sh
cub unit data-edit --space registrybot-my-cluster registrybot-config
```

| Input | Default | Description |
|---|---|---|
| `config_space` | `registrybot` | With `config-unit` only: the Space of registrybot's configuration Unit, `REGISTRYBOT_CONFIG_SPACE`. |
| `confighub_url` | `https://hub.confighub.com` | The ConfigHub URL as the cluster reaches it. |
| `metrics` | `true` | Whether registrybot serves Prometheus metrics. `false` sets `METRICS_ENABLED=false` and drops the Deployment's `metrics` port. |

To run another image, such as a mirror's copy, pass `--set-image ghcr.io/confighub/registrybot=<registry>/registrybot:<tag>`.

### As a base for variants

Every input has a default, so one install can be the base that each cluster's variant is cloned from. Set what differs in each variant's Units: `registrybot-config` for what it watches, and `registrybot`, which holds the Deployment, for its environment:

```sh
cub package install registrybot:0.5.3 --space registrybot-base --namespace registrybot
cub variant create --target my-cluster/target my-cluster registrybot-base
cub function do --space registrybot-my-cluster --unit registrybot \
  set-env registrybot REGISTRYBOT_POLL_INTERVAL=10m
```

## Testing

`scripts/test-package.sh` renders the package for each preset with `installer setup`, running its validators, and checks the renders it has to refuse. It also checks that the Deployment has the environment of `manifests/registrybot.yaml`, that the two ship the same configuration document, and that every render gives registrybot one configuration source. It needs installer 0.3.0 or later (or `$INSTALLER`) and yq, and no ConfigHub server. The `package` workflow runs it on every change.

## Publishing

The package's version is the registrybot release it runs: package 0.5.0 runs registrybot v0.5.0. Pushing a release tag publishes it after the image (`release.yml`), which sets `installerMetadata.version` and the `images:` tag in `bases/default/kustomization.yaml` from the tag first, so neither is bumped by hand. A version is published once, so a change to the package alone ships with the next registrybot release.

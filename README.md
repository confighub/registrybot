# registrybot

A ConfigHub bot that watches container repositories and records what they hold as ConfigHub units.

For every repository it is told to watch, registrybot maintains one unit — a *fact unit* — holding a few named *streams* that each pick out the one tag a consumer most likely wants ("newest", "semver", or streams you define such as "stable" or "main"). It learns about changes by polling the GitHub Packages API and, when a webhook is configured, by receiving GitHub `package` events that make it look sooner.

That is its whole job. What happens when a fact unit changes — which deployments pick up the new tag, through which promotion steps, with what approvals — is ConfigHub's, expressed as links from the units that consume the fact. registrybot never touches those units. See [docs/design.md](docs/design.md) for the reasoning and the fact schema.

registrybot follows the [argobot](https://github.com/confighub/argobot) pattern: a small Go process built into a public image, authenticated to ConfigHub as a worker identity, deployed from a manifest that ConfigHub itself manages. Only ghcr.io is supported in this version.

## How it works

Two things make registrybot look at a repository, and both lead to the same place:

- **Polling.** Every `pollInterval` (default 5m) the bot re-reads its configuration document and enqueues every repository in it.
- **Webhooks.** A GitHub `package` (or legacy `registry_package`) event names a repository. If that repository is watched it is enqueued once the repository has *settled*: one image push is many package events spread over a few minutes (per-architecture manifests, buildcache tags, the manifest list, the cosign signature and attestation), so the bot waits until `webhooks.settle` (default 2m) has passed with no further delivery for that repository, capped at `webhooks.maxDelay` (default 10m) from the first, and reconciles once. A poll that falls inside the window leaves the repository to the timer. Deliveries for unwatched repositories are acknowledged and ignored. Nothing in the payload is trusted: it only decides *when* to look.

A single worker drains the queue. For each repository it lists the package's active versions from the GitHub Packages API, builds the fact document, and compares it with what the unit holds. If the facts changed it writes a new revision with a one-line description such as `registrybot observed ghcr.io/confighub/argobot: newest=main@9d0df3df21e0 semver=v0.3.1@2a9c5d3ee6ff` (each stream's tag and the first 12 characters of its digest, so a moving tag still reads as a change). If nothing changed it writes nothing, so an idle repository produces no revision churn. A fact unit that does not exist yet is created (toolchain `AppConfig/YAML`, labeled `registrybot.confighub.com/repository=<repo>`).

The bot holds no state it cannot rebuild from GitHub and ConfigHub. Restart it any time.

### The fact unit

```yaml
# Facts about ghcr.io/confighub/argobot, observed by registrybot from the GitHub Packages API.
# Do not edit: the next observation overwrites this unit. Link to it instead.
configHub:
  configSchema: registrybot.confighub.com/v1alpha3
  configName: argobot
repository: ghcr.io/confighub/argobot
registry: ghcr.io
owner: confighub
source: github-packages
url: https://github.com/orgs/confighub/packages/container/package/argobot
observedAt: "2026-09-16T15:12:03Z"
streams:
  newest:                      # most recently pushed tag of any kind
    tag: main
    digest: sha256:9f2c…
    image: ghcr.io/confighub/argobot:main
    imageByDigest: ghcr.io/confighub/argobot@sha256:9f2c…
    createdAt: "2026-09-16T14:58:41Z"
  semver:                      # highest release version (prereleases excluded)
    tag: v0.3.1
    digest: sha256:41aa…
    image: ghcr.io/confighub/argobot:v0.3.1
    imageByDigest: ghcr.io/confighub/argobot@sha256:41aa…
    createdAt: "2026-09-12T10:03:17Z"
  stable: null                 # a configured stream nothing matched yet
```

Downstream units link to `streams.<name>.tag`, `streams.<name>.digest`, or `streams.<name>.image`, so that "which tag counts as stable" is decided once, in the stream definition, and every consumer follows it. The unit deliberately carries no list of tags: the registry already has that, nothing can link into a list by key, and the unit's revision history is the history of what each stream pointed at.

### The configuration document

What to watch is itself configuration, held in a ConfigHub unit of toolchain `AppConfig/YAML` and re-read every poll cycle. Editing that unit reconfigures the running bot; no restart, no redeploy. [examples/registrybot-config.yaml](examples/registrybot-config.yaml) is a commented example. The shape:

```yaml
pollInterval: 5m
webhooks:
  settle: 2m                        # reconcile once deliveries for a repository stop for this long
  maxDelay: 10m                     # ...but never later than this after the first delivery
defaults:
  space: registry-facts             # where fact units go; default: the config unit's own space
  exclude: ["^sha-", "^pr-"]        # tags to ignore
  streams:
    stable: { semver: ">=0.0.0" }
repositories:
  - repository: ghcr.io/confighub/argobot
  - repository: ghcr.io/confighub/cubbychat
    unit: cubbychat-image           # default: <owner>-<name>
    streams:
      stable: { semver: ">=1.0.0 <2.0.0" }
      next:   { semver: ">=2.0.0-0" }    # "-0" admits prereleases
      main:   { pattern: "^main$" }
```

A stream with `semver` is the highest tag that parses as a release version (`v1.2.3`, `1.2.3`, `1.2.3-rc.1`; strict `MAJOR.MINOR.PATCH`) and satisfies the constraint. A stream with only `pattern` is the most recently pushed tag matching it. Both together narrow by pattern first. The built-in `newest` and `semver` streams are always present unless you redefine them.

### Discovery

By default the document is an allowlist: a webhook for an unlisted repository is acknowledged and ignored. With `discovery.fromWebhooks: true`, a signed `package` event for an unlisted repository whose owner the policy admits creates a fact unit for it from `defaults` and polls it from then on. This is what makes an organization-level webhook useful: every image the organization publishes gets a fact unit without anyone editing the document.

```yaml
discovery:
  fromWebhooks: true
  owners: [confighub, confighubai]          # registry namespaces allowed; empty = any
  exclude: ["^confighub/ui-preview-", "-preview$"]   # "<owner>/<name>" patterns to ignore
```

The fact units are the record. At every cycle the bot lists the units in the default space that carry its `registrybot.confighub.com/repository` label and watches those, so a restart forgets nothing and the bot never edits its own configuration. To stop watching a discovered repository, delete its fact unit or tighten the policy; an entry in `repositories` always takes precedence over a discovered one for the same repository.

## Configuration

Process configuration is environment only. Everything about *what* to watch lives in the configuration document above.

| Variable | Required | Description |
|---|---|---|
| `CONFIGHUB_URL` | yes | ConfigHub base URL, e.g. `https://hub.confighub.com` |
| `CONFIGHUB_WORKER_ID` / `CONFIGHUB_WORKER_SECRET` | one credential | Worker identity, as `cub worker get-envs` prints it |
| `CONFIGHUB_AUTH_PRIVATE_KEY` | one credential | Instead of the secret: an Ed25519 private JWK registered with `cub worker key add`, inline or as a file path |
| `CONFIGHUB_ASSERTION_AUDIENCE` | no | Only if the server sets a non-default assertion audience |
| `CONFIGHUB_TOKEN` | one credential | A static session token. Local development only; it is never refreshed |
| `REGISTRYBOT_CONFIG_SPACE` | yes* | Slug of the space holding the configuration unit |
| `REGISTRYBOT_CONFIG_UNIT` | no | Slug of the configuration unit. Default `registrybot` |
| `REGISTRYBOT_CONFIG_FILE` | yes* | Local development: read the configuration document from this file instead of a unit. *One of `_SPACE` or `_FILE` is required |
| `GITHUB_TOKEN` | yes | Classic personal access token with `read:packages`, or a GitHub App installation token with packages read. Used to list versions |
| `GITHUB_API_URL` | no | Default `https://api.github.com`; set for GitHub Enterprise Server |
| `GITHUB_WEBHOOK_SECRET` | no | The secret GitHub signs deliveries with. Absent: webhook endpoint disabled, polling only |
| `REGISTRYBOT_POLL_INTERVAL` | no | Default `5m`. The configuration document's `pollInterval` overrides it |
| `REGISTRYBOT_LISTEN_ADDR` | no | Default `:8080` |

Exactly one ConfigHub credential kind must be set; the bot refuses to guess between two.

HTTP endpoints: `POST /webhooks/github` (signature-checked), `GET /healthz`, `GET /status` (what is watched and whether it was discovered, last attempt/success/write per repository, current stream tags).

## Setup

Install the `cub` CLI (see [docs.confighub.com](https://docs.confighub.com)), then:

```sh
# 1. A space for the bot's own configuration (fact units can go here too, or elsewhere).
cub space create registrybot

# 2. The worker (bot identity). Its credential is what the bot authenticates with.
cub worker create registrybot --space registrybot
cub worker get-envs registrybot --space registrybot     # CONFIGHUB_WORKER_ID / _SECRET

# 3. The configuration document.
cub unit create --space registrybot --toolchain AppConfig/YAML \
  registrybot examples/registrybot-config.yaml
```

The worker's bot user inherits the creator's organization role, which is what lets it create and write units. Run the bot:

```sh
CONFIGHUB_URL=https://hub.confighub.com \
CONFIGHUB_WORKER_ID=... CONFIGHUB_WORKER_SECRET=... \
REGISTRYBOT_CONFIG_SPACE=registrybot \
GITHUB_TOKEN=ghp_... \
registrybot
```

Within one poll cycle each repository in the document has a fact unit. Edit the document (`cub unit update ...`) to add repositories or change streams; the bot picks it up on its next cycle.

For local development against a file instead of a unit:

```sh
REGISTRYBOT_CONFIG_FILE=examples/registrybot-config.yaml CONFIGHUB_TOKEN=$(cub auth get-token) ...
```

### Webhooks

Polling alone is complete; webhooks add immediacy, and with discovery enabled they are also how new repositories enter. Set `GITHUB_WEBHOOK_SECRET`, expose `POST /webhooks/github` to GitHub, and add a webhook on the organization (or repository) with content type `application/json`, the same secret, and only the **Packages** event selected. Organization webhooks see every package in the organization; repository webhooks see only packages linked to that repository. GitHub's ping is answered with 200; deliveries for repositories the bot does not watch and does not discover are answered with 202 and ignored.

## Deploy

`manifests/registrybot.yaml` is a complete deployment: a Namespace, a ServiceAccount (no token mounted; the bot needs no Kubernetes API access), a single-replica Deployment whose `env` list carries every supported variable, and a ClusterIP Service in front of the webhook receiver. Credentials come from a Secret named `registrybot-secrets` that the manifest deliberately does not define; supply it out of band with keys `CONFIGHUB_WORKER_ID`, `CONFIGHUB_WORKER_SECRET`, `GITHUB_TOKEN`, and optionally `GITHUB_WEBHOOK_SECRET`. How the Service is exposed to GitHub is a per-cluster decision.

### Config bundle

`.github/workflows/release.yml` packages `manifests/` into an OCI bundle at `ghcr.io/confighub/configs/registrybot:latest`. The bundle floats, but each cut pins the concrete released image: the committed manifest says `registrybot:latest` and the workflow substitutes the current release tag before publishing, failing if the pin does not take. Load it as a component base:

```sh
cub variant upload --component registrybot --variant base --granularity per-file \
  oci://ghcr.io/confighub/configs/registrybot
```

## Build and release

```sh
go test ./...
go build ./...
docker build -t ghcr.io/confighub/registrybot:dev .
```

- `.github/workflows/build.yml` runs the tests and builds development images (`main`, `pr-N`, `sha-…`) on pushes to `main` and on PRs. No semver, no `:latest`.
- `.github/workflows/release.yml` cuts a release on a `vX.Y.Z` tag: the semver image (plus `:latest`) and the config bundle pinned to it. The bundle is also republished on every `main` push, pinned to the latest existing tag, so a config-only change ships without a new image.

Every image carries its third-party notices at `/app/THIRD_PARTY_LICENSES.txt` and `/app/OS_LICENSE_NOTICE.txt`, and every released manifest has an SPDX SBOM attestation. After changing dependencies, run `./scripts/gen-third-party-licenses.sh` and commit the result; CI fails the PR if the committed file is stale or a copyleft dependency is linked in.

## Status and roadmap

This is a working prototype built to explore what a registry integration looks like as a plain ConfigHub client and what a "fact unit" should contain. Known limits and likely next steps:

- **ghcr.io only.** Observation goes through the GitHub Packages API. An OCI distribution backend (`/v2/<name>/tags/list` plus manifest HEADs) would cover any registry and public images without a token, at the cost of timestamps.
- **Discovery is webhook-only.** A repository that never fires a webhook (published before the hook existed) is not discovered; list it explicitly or push once. A `discovery.fromListing` that enumerates an organization's packages would close that gap.
- **Configuration reload is by polling.** Subscribing to ConfigHub's event log for changes to the configuration unit would make edits take effect at once.
- **Single replica.** Two instances would race on the same units. Leader election is unnecessary at this scale; a per-repository sharding key would be the way to scale out.
- **Schema.** `registrybot.confighub.com/v1alpha3` will change as consumers tell us what they need to link to. Existing fact units keep the schema annotation they were created with; `configHub.configSchema` in the data is authoritative.

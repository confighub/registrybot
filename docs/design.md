# registrybot design

## Why this exists

ConfigHub issue #5062 asks for an image updater: watch a container registry and propagate new images to every unit that deploys them. The discussion there settled the shape early — build it outside ConfigHub, like argobot and the Argo CD / Flux image updaters, and have it report back into a config unit rather than reach into deployment units directly. registrybot is that external piece, built so that we have something concrete to play with while the product-side questions (fact units versus intent units, link kinds across variants, promotion of facts) are worked out.

## The one job

registrybot keeps fact units current. It does not decide what to deploy, does not edit the units that deploy things, and does not know which units consume its facts. The moment a fact unit's data changes, the rest is ConfigHub's: links (TransformPaths, NeedsProvides), triggers, approvals, and promotion carry the new tag or digest wherever the organization has said it should go.

Keeping the bot on that side of the line is what makes it safe to run with broad credentials and cheap to reason about. Every write it performs is to a unit it created and labeled as its own, with content derived entirely from what GitHub reported a moment earlier.

## Facts, not intents

A fact unit records observed state — "this repository has these tags pointing at these digests, as of this time". Nothing in it asks ConfigHub to do anything. An intent unit says "deploy this". The two look alike (both are units, both are YAML) and ConfigHub does not yet distinguish them, but a few properties of fact units already follow from the definition:

- **The bot is the only author.** A human edit is overwritten by the next observation. The header says so.
- **Revisions are observations.** A new revision means the repository changed. An unchanged repository must not produce revisions, so the renderer is deterministic and the comparison ignores `observedAt`.
- **Selection is part of the fact.** Consumers do not want a tag list; they want *the* tag. So the document carries streams — named selections computed by the bot from a rule in the configuration — and consumers link to `streams.<name>.tag`. The rule for "stable" is written once, next to the repository, instead of in every consumer.

Whether ConfigHub should mark these units differently (a toolchain, a label convention, a unit kind), how promotion should treat a link from an intent unit to a fact unit that lives outside the variant tree (#5313), and whether "the bot is the only author" should be enforced — those are the product questions this prototype is meant to inform, not answer.

## Discovery

An allowlist is the right default for a bot that writes, but an organization-level webhook is the interesting experiment: every image the organization publishes shows up as a fact unit with no one editing anything. Discovery is therefore opt-in and policy-bound (owners, exclusion patterns), and it keeps the intent/fact split intact. The configuration document stays pure intent; the bot never appends to it. The fact units, labeled with their repository, are the record of what has been discovered, and the bot re-derives its discovered set from them every cycle. Deleting a fact unit is how an operator says "stop", and a restart loses nothing.

A webhook still carries no facts. It admits a repository into the watch set, and the reconcile that follows reads GitHub like any other.

## Configuration through ConfigHub

The bot's process configuration is environment variables, and small: how to reach ConfigHub and GitHub, and where its configuration document is. What to watch — repositories, streams, exclusions, poll interval — is a document in a ConfigHub unit. The bot is already a ConfigHub client, so the marginal cost of reading its own configuration from a unit is one list call per cycle (the listing carries `DataHash`; the body is fetched only when it changed). In return the watch list has history, review, and the same tooling as everything else.

The document is re-read every cycle. Subscribing to the event log for changes to that one unit is the obvious refinement and was left out to keep the first cut small.

## Two triggers, one path

Polling and webhooks both end in the same `reconcile(repository)`, which lists versions from the GitHub Packages API and rewrites the fact unit. Webhook payloads are never used as data: a delivery names a repository, and if the bot watches that repository it looks now instead of at the next tick. This removes a whole class of problems — payload shape drift, forged deliveries, out-of-order events, the `registry_package`/`package` duality — at the cost of one API call per delivery. Polling remains the completeness guarantee; webhooks are latency.

The queue deduplicates by repository and a single worker drains it, so two reconciles never race on one unit. Deduplication alone did not make a burst one reconcile, though: an image push is not one event. A multi-architecture build with cosign lands as five to ten package events over several minutes (each per-architecture manifest and its buildcache tag, the manifest list, the signature, the attestation), and the worker is fast enough to reconcile between them, so the first deployment wrote a revision per intermediate state. A webhook therefore opens a *settle window* for its repository instead of enqueuing it: the reconcile runs once no delivery has arrived for `webhooks.settle`, or `webhooks.maxDelay` after the first one, and the poll skips a repository whose window is open. Revisions go back to meaning "the repository changed", at the cost of the settle time in latency, which is still well under a poll interval.

One more wrinkle in what a revision says: ConfigHub treats a change description identical to the previous one as "not provided" and stores the source type instead. Two consecutive observations that differ only in a digest (a moving `latest`, a rebuilt buildcache tag) would produce the same one-liner, so the description carries a short digest per stream.

## Identity

The bot authenticates as a worker identity, the same as argobot: either the worker secret, or a private key registered against the worker's bot user (`cub worker key add`), exchanged at `/auth/worker` for a session token that carries the bot user's organization role. Session renewal is scheduled from the token's own `exp` claim (80% of the way there, five-minute floor), and a 401 mid-flight triggers one re-authentication and retry.

A static token is accepted for local development and is never refreshed.

## Fact schema `registrybot.confighub.com/v1alpha1`

| Path | Meaning |
|---|---|
| `schema` | This identifier |
| `repository`, `registry`, `owner`, `name` | The canonical repository and its parts |
| `source` | How it was observed; `github-packages` |
| `url` | The package's web page |
| `observedAt` | When this revision was observed (RFC 3339, UTC). Ignored when deciding whether facts changed |
| `streams.<name>` | The tag the stream selects: `tag`, `digest`, `image` (`repo:tag`), `imageByDigest` (`repo@digest`), `createdAt`. `null` when nothing matches |
| `tags[]` | Every tag after exclusions, newest first, capped by `limit`: `tag`, `digest`, `image`, `createdAt` |

Untagged manifests (attestations, signatures, dangling layers) are not facts anyone deploys from and are omitted. A manifest with several tags appears once per tag.

Built-in streams: `newest` (most recently pushed tag) and `semver` (highest strict `MAJOR.MINOR.PATCH` release, prereleases excluded). Both can be redefined per repository.

## What was deliberately left out

- **Other registries.** The GitHub Packages API gives digests, tags, and timestamps in one call and matches what "watch ghcr.io" means. An OCI distribution backend is the path to Docker Hub, ECR, and public images without a token; the `repoRef` and `packageVersion` shapes are meant to survive that addition.
- **Writing into consumers.** The Argo CD image updater edits the Application. Here that is a link, and doing it in the bot would duplicate ConfigHub's promotion logic badly.
- **Digest pinning as a stream option.** `imageByDigest` is present on every stream, so a consumer can link to it today. Whether a stream should be able to say "resolve to digest" as policy is a consumer question.
- **A hosted receiver.** Making ConfigHub itself the GitHub webhook endpoint would spare SaaS customers a deployment. The issue thread says to prove the stand-alone version first; this is that.

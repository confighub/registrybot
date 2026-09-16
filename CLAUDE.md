# registrybot

A ConfigHub bot that keeps one fact unit per watched container repository up to
date from the GitHub Packages API. Read `README.md` for what it does and
`docs/design.md` for why it is shaped this way. Flat `package main`, no
subpackages; one concern per file, named for it.

- The bot never writes a fact it did not just read from GitHub. Webhooks only
  schedule a reconcile; they carry no data into ConfigHub. Keep it that way.
- `observe.go` renders deterministically and `sameFacts` ignores `observedAt`;
  a change that makes an unchanged repository produce a new revision is a bug.
- Process configuration is environment only (`config.go`); everything about
  *what* to watch is the configuration document (`botconfig.go`), so it can be
  changed through ConfigHub. Do not add watch-list knobs as env vars.
- `manifests/registrybot.yaml` must list every env var, defaults filled in,
  opt-ins commented out. Adding a variable means updating it and the README table.
- After changing dependencies run `./scripts/gen-third-party-licenses.sh` (in
  the `golang:1.25` image if the local toolchain is older) and commit the result.

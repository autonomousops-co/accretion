# Contributing to Accretion

Thanks for your interest in the project. This page covers the Contributor License
Agreement, how to get a working development environment, and what the automated
checks will hold you to.

Participation is governed by our [Code of Conduct](CODE_OF_CONDUCT.md). Conduct
reports go to the same private inbox as security reports, never a public issue.

## Contributor License Agreement

**By opening a pull request against this repository, you agree to the
[Contributor License Agreement](CLA.md).**

The CLA is self-executing — there is nothing to sign and no bot to satisfy. It
takes effect when you submit a contribution, so please read it before you do.
In short: you keep every right to your own work, and you grant us a license to
distribute it under the project's licenses.

That last part is why the CLA exists rather than being decorative. Accretion is
released under [AGPL-3.0](LICENSE) *and* under a
[commercial license](LICENSE-COMMERCIAL.md) for organizations that cannot take on
the AGPL's terms. Section 3 of the CLA is what permits your contribution to be
distributed under both. Without it, a contribution could only ever ship under the
AGPL, and the dual-licensing would break at the first outside patch.

If you are contributing on behalf of an employer, make sure whoever owns the
copyright in your work is able to agree to it.

## Getting set up

You need Go 1.26, Docker, `kind`, `kubectl`, `helm`, `tilt`, `direnv`, and
`pre-commit`. `make preflight` reports on all of them, plus your git hooks and
the Go tooling, and tells you what is missing.

```bash
git clone git@github.com:autonomousops-co/accretion.git
cd accretion

make bootstrap         # creates .env, installs Go tools and git hooks, verifies
direnv allow           # loads .envrc, which pulls in .env
```

`make bootstrap` runs `env-file`, `go-tools`, `install-hooks` and `preflight` in
order. Each step is also a target on its own if you would rather go one at a
time. Fill in the keys for the providers you actually use — everything else has
a working local default.

Go itself is pinned in `.tool-versions`, which [asdf](https://asdf-vm.com) and
[mise](https://mise.jdx.dev) both read. Neither is required: any Go matching the
`go` directive in `go.mod` works, and `make preflight` tells you if yours is too
old.

`.envrc` is checked in and holds non-secret defaults; `.env` is gitignored and
holds secrets, loaded last so it wins. Neither is optional — the run targets stop
with instructions if `.env` is missing.

`make install-hooks` installs the git hooks for **both** the pre-commit and
pre-push stages. Installing only the pre-commit stage leaves the release-tag check
silently inert, so if you set hooks up by hand, use:

```bash
pre-commit install --hook-type pre-commit --hook-type pre-push
```

`make preflight` reports which stages are actually installed, so a half-wired
checkout is visible rather than silent.

## Notes for Linux

Two things bite on Linux and on no other platform:

- **Docker without `sudo`.** If `make preflight` reports the daemon as
  unreachable, the cause is usually group membership rather than a stopped
  daemon: `sudo usermod -aG docker "$USER"`, then log out and back in.
- **inotify limits and `kind`.** A cluster this size can exhaust the kernel's
  default inotify allowance, which surfaces as pods stuck in `CrashLoopBackOff`
  with `too many open files` rather than as anything mentioning inotify. Raise
  them before `make up`:

  ```bash
  sudo sysctl fs.inotify.max_user_instances=512
  sudo sysctl fs.inotify.max_user_watches=524288
  ```

  Persist them in `/etc/sysctl.d/99-kind.conf` to survive a reboot.

To run the stack: `make up` (creates the kind cluster, then starts Tilt). See the
[README](README.md#quick-start-local) for what that brings up, and
[ARCHITECTURE.md](ARCHITECTURE.md) for a map of the Go codebase.

## Making a change

Work on a branch off `main`. Commit messages follow
[Conventional Commits](https://www.conventionalcommits.org/) — the prefixes in use
are `feat`, `fix`, `refactor`, `perf`, `docs`, `style`, `test`, and `build`, with
an optional scope: `fix(discord): ...`.

Explain *why* in the commit body. The reasoning behind a change is the part that
cannot be recovered from the diff later.

## What the checks enforce

The hooks run automatically. You can run the same things by hand:

| Command | What it checks |
|---|---|
| `make test` | full suite with the race detector |
| `make lint` | `gofmt` plus `golangci-lint` — both must be clean |
| `make vet` | `go vet` |
| `make charts-check` | every chart gate the pipeline's `charts` job runs — the job calls exactly this target |
| `make ci` | the local superset: the pipeline's gates plus the ones that need local tooling (tilt, gosec, the lint self-check) |

`make lint-new` is what the pre-commit hook runs and the one to trust for "did
I introduce a finding": every changed line is held to the full `golangci-lint`
standard, while findings that predate the v2 migration are grandfathered. One
constraint, learned the hard way (#98): golangci-lint's `--new-from-rev` filter
silently drops **every** finding when the working directory is reached through
a symlink — macOS `/tmp` is one, and sprint worktrees live there. It resolves
the symlink for the working directory but not for the findings' own paths, so
nothing matches the diff, and the result is a false `0 issues` rather than an
error. `make lint-new` enters the physical path before running, and
`make lint-new-selfcheck` proves that still works. If you run
`golangci-lint run --new-from-rev=...` by hand, `cd "$(pwd -P)"` first. A full
`make lint` is unaffected: only the new-from-rev filter compares paths.

Two project-specific rules will stop you, and both fail for reasons that are
otherwise silent:

**Editing a chart requires bumping that chart's `version`.** Helm keys its chart
cache on the version, so reinstalling an edited chart under the same version
serves the old manifests with no error at all. `scripts/check-chart-version-bump.sh`
runs at commit time.

**A release tag must match the charts' `appVersion`.** `appVersion` is the only
place a deployed pod reports which release it is running, and a tag is immutable
once anyone has fetched it. `scripts/check-chart-app-version.sh` runs at push time.

The two versions are different things: `version` is the chart's own, bumped by any
chart edit; `appVersion` is the software the chart ships, and tracks release tags.
Note also that release tags carry a `v` prefix (`v1.2.3`) because Go resolves
module versions only from `v`-prefixed tags, while Helm requires bare SemVer in
both chart fields. The scripts enforce each convention on its own side.

## Opening a pull request

Target `main`. Describe what changed and why, and say how you verified it —
naming the commands you actually ran, with their results.

If you are changing behaviour, add or update a test. If you are changing operator-
facing output such as an error message or a chart value, say so explicitly: those
are what someone reads at three in the morning.

## Reporting security issues

**Please do not open a public issue for a security vulnerability**, and do not
include exploit details in a pull request.

See [SECURITY.md](SECURITY.md) for how to report one privately, what to include,
and what is in scope.

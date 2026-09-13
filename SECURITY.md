# Security Policy

## Reporting a vulnerability

Report security vulnerabilities privately, by either route:

- **Email security@autonomousops.co.** Encrypt if you prefer; say so and we will
  arrange a key.
- **GitHub private vulnerability reporting**, on this repository's
  [Security tab](https://github.com/autonomousops-co/accretion/security/advisories/new).

Both are private between you and the maintainers, and both let us prepare a fix
and an advisory before anything is public. Use whichever you prefer — email
reaches us without a GitHub account.

**Please do not** open a public issue, post details in a pull request or
discussion, or include a working exploit in any public place. If you have already
disclosed something publicly, tell us in the report so we can prioritize
accordingly.

### What to include

The more of this you can give us, the faster we can confirm and fix it:

- What the issue is and what an attacker gains from it.
- The affected component — the API server, a collector worker, the Discord bot,
  an MCP server, a Helm chart, or the Pulumi infrastructure.
- Steps to reproduce, or a proof of concept. A minimal one is ideal; please do
  not include a weaponized exploit.
- The commit, tag, or image digest you tested.
- Your configuration where it matters — LLM provider, whether auth was enabled,
  and which namespaces were in scope.

Please do not test against deployments you do not own. Accretion ingests
operational data from live clusters, so someone else's instance holds their logs,
not test data.

### What to expect

We will acknowledge your report, keep you informed as we investigate, and tell
you when a fix ships. If you would like credit in the advisory, say so and how
you want to be named; if you would rather stay anonymous, that is fine too.

We ask that you give us a reasonable opportunity to release a fix before
disclosing publicly.

## Supported versions

There is no tagged release yet. Until there is, the supported version is the
latest commit on `main`, and fixes land there.

## Scope

Accretion stores an operational record of your clusters and holds the credentials
needed to collect it, so the areas most worth your attention are:

- **Authentication and authorization** — the OAuth flow, session cookie signing
  and encryption, API key hashing and per-client scoping, and the user
  allowlists. These are written to fail closed; a path that fails *open* is a
  vulnerability, and so is one that silently widens scope.
- **The ingestion boundary** — anything by which log, event, or trace content
  could influence how it is later processed, or by which a sender could get data
  attributed to a workload that is not theirs.
- **Namespace scoping** — a selector that resolves to more namespaces than
  configured, or an exclusion that fails to exclude.
- **Secret handling** — credentials reaching logs, traces, embeddings, error
  messages, or the memory store.
- **The Helm charts and Pulumi stacks** — RBAC broader than a component needs,
  or a default that exposes something it should not.

### Out of scope

These are known, deliberate, and documented as development-only:

- Local development defaults in `.envrc` and `values-dev.yaml`, chiefly the
  `postgres`/`postgres` database credentials. They exist for a local kind cluster
  and are not intended for any real deployment.

  **No API key is among them.** Every identity in every checked-in values file
  carries a SHA-256 `hash` and nothing else; the plaintext each client sends
  lives only in a gitignored `.env`. `make values-check` refuses a plaintext key
  in any values file and proves that refusal fires against a purpose-built
  fixture rather than only reporting a pass. A working credential found checked
  into this repository is a finding, not a known exception — please report it.
- The auth-disabled path used for local development, which does not enforce the
  checks the enabled path does.
- The DKJR chaos fixture (`charts/DKJR.md`), which generates deliberately broken
  workloads and is installed on demand.
- Findings that require an attacker to already have cluster-admin or direct
  database access.
- Reports from automated scanners with no demonstrated impact on this codebase.

If you are unsure whether something is in scope, report it — we would rather read
one that turns out to be out of scope than miss one that is not.

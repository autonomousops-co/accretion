#!/usr/bin/env bash
#
# Create (or refresh) the Secrets the accretion-api chart, the MCP client and
# the Discord bot read, from your local .env.
#
# Why this exists: .env uses SHELL_CASE variable names, while the chart's
# secretKeyRefs read dashed-lowercase Secret keys. `kubectl create secret
# generic --from-env-file=.env` therefore produces a Secret whose keys the chart
# never looks at. Nothing surfaces the mismatch — the env vars are simply absent
# and the API 401s every request. This script is the one mapping, used by both
# the Tiltfile and cluster installs so dev and prod share a single path.
#
# It emits THREE Secrets, because three different consumers read three different
# things and all are sourced from the same .env:
#
#   accretion-api-secrets   the server's own key material (session key, OAuth
#                           client, LLM key, the user whitelist).
#   accretion-mcp-secrets   the MCP client's credential, under the exact keys
#                           internal/mcp/secrets resolves: ACCRETION_API_URL,
#                           ACCRETION_API_KEY and a CA pointer. The four
#                           checked-in editor configs already point at
#                           ACCRETION_MCP_SECRET_NAME=<ns>/accretion-mcp-secrets,
#                           which the chart used to create from a plaintext key
#                           in values-dev.yaml; #122 moved that plaintext into
#                           .env, so this script owns the Secret instead. One
#                           object, one owner: a Helm-created Secret and a
#                           kubectl-created one of the same name fight.
#   accretion-discord-secrets
#                           the Discord bot's OWN credential for accretion-api,
#                           under the key its chart's api.secretKey names.
#
#                           #119: the bot read api-key-accretion-discord out of
#                           the accretion-api chart's client-keys Secret, which
#                           that chart emits ONLY for an identity carrying a
#                           plaintext `key`. Every identity has been hash-only
#                           since #122, so that Secret exists on no path any
#                           more and the bot's secretKeyRef is deliberately not
#                           optional: the pod fails at rollout rather than
#                           401ing quietly. Same Secret name and same key as the
#                           one Pulumi provisions (pulumi/pkg/kubernetes/
#                           secrets.go, deploy/pulumi.yaml), so the bot is
#                           pointed at one shape locally and in a cluster rather
#                           than two.
#
#                           NOT accretion-discord-secret, SINGULAR, which is the
#                           accretion-discord chart's own Secret. The names
#                           differ by one letter and hold different things: the
#                           plural is the bot's credential FOR accretion-api and
#                           belongs to this script; the singular holds the bot's
#                           Discord token and model keys and belongs to Helm.
#                           Pulumi has only the plural because it sets
#                           secret.create=false and points the chart at its own;
#                           the Tilt stack still lets the chart create the
#                           singular from .env values.
#
# Usage:
#   scripts/make-secret.sh [--namespace NS] [--env-file PATH] [--dry-run]
#   scripts/make-secret.sh --print-keys        # accretion-api-secrets key names
#   scripts/make-secret.sh --print-identities  # the apiKeys block .env implies
#   scripts/make-secret.sh --print-identity-var NAME
#                                              # the .env variable holding one
#                                              # identity's plaintext. A NAME,
#                                              # never a value.
#
# Values are read from the env file, falling back to the process environment
# (so direnv-exported values and CI both work). Empty values are skipped.
#
# Per-client API keys used to be deliberately absent from here: the chart's
# `apiKeys` list carried each plaintext and derived both halves from it, so they
# could not drift. #122 ended that -- a working credential must not ship in a
# public repository -- and the price is that the plaintext here and the SHA-256
# `hash` in the chart's apiKeys list are now two inputs that CAN disagree, with
# a 401 and no explanation as the only symptom. `--print-identities` emits the
# apiKeys block this .env implies, and `make secrets-identity-check` is the gate
# that refuses a configured identity whose hash does not match.

set -euo pipefail

SECRET_NAME="accretion-api-secrets"
# The Secret internal/mcp/secrets resolves, and the name the four checked-in
# editor configs already carry in ACCRETION_MCP_SECRET_NAME.
MCP_SECRET_NAME="accretion-mcp-secrets"
# The Secret the accretion-discord chart's api.secretName points at. The name
# and the key below are BOTH cross-chart references: change either and change
# deploy/tilt.yaml, deploy/pulumi.yaml and the chart's api.secretName/secretKey
# defaults with it. A wrong name fails the bot's pod at rollout rather than
# 401ing every call, which is the failure this shape was chosen for.
DISCORD_SECRET_NAME="accretion-discord-secrets"
# The chart's serving-certificate Secret (accretion-api.tlsSecretName in
# templates/_helpers.tpl is "<release>-tls", and the release is accretion-api on
# every path that runs this). Published as a POINTER, never a copy: Helm cannot
# read cluster state and neither can this script, and the MCP follows the
# pointer with the kubeconfig it already used to read this Secret.
API_TLS_SECRET_NAME="accretion-api-tls"
# The in-cluster Service the MCP dials when .env names no URL — the same address
# the chart's mcpSecret template used to default to (service.port, not the
# container port: the Service targets the container port by name).
API_SERVICE_NAME="accretion-api"
API_SERVICE_PORT=80
NAMESPACE="${NAMESPACE:-accretion-core}"
ENV_FILE=".env"
MODE="apply"
# The identity --print-identity-var was asked about. Empty in every other mode.
IDENTITY_ARG=""

# "<secret-key>:<ENV_VAR>" — one row per secret key, naming the single variable
# it is read from. Keys are unique by construction, so there is no dedupe.
# There is deliberately no alias mechanism: a renamed variable is handled by
# warn_stale_vars below, which tells the operator the old name is ignored rather
# than quietly reading it (see #56).
MAPPINGS=(
  "session-key:SESSION_KEY"
  "oauth-client-id:OAUTH_CLIENT_ID"
  "oauth-client-secret:OAUTH_CLIENT_SECRET"
  "openai-api-key:ACCRETION_LLM_OPENAI_API_KEY"
  "accretion-auth-allowed-emails:ACCRETION_AUTH_ALLOWED_EMAILS"
  "accretion-auth-allowed-domains:ACCRETION_AUTH_ALLOWED_DOMAINS"
  "accretion-auth-admin-emails:ACCRETION_AUTH_ADMIN_EMAILS"
  "accretion-auth-admin-domains:ACCRETION_AUTH_ADMIN_DOMAINS"
  "accretion-auth-oauth-redirect-url:ACCRETION_AUTH_OAUTH_REDIRECT_URL"
  "accretion-auth-cookie-secure:ACCRETION_AUTH_COOKIE_SECURE"
  "accretion-auth-cookie-domain:ACCRETION_AUTH_COOKIE_DOMAIN"
  "accretion-logging-frontend-level:ACCRETION_LOGGING_FRONTEND_LEVEL"
)

# What "required" means differs by mechanism, so both are checked here.
#
# REQUIRED: the kubelet enforces these — their secretKeyRefs are not
# `optional: true`, so an absent key fails the pod at rollout rather than 401ing
# every request afterwards.
REQUIRED=(
  "session-key"
  "oauth-client-id"
  "oauth-client-secret"
)

# REQUIRED_ANY: the SERVER enforces this one — every ref below is
# `optional: true` to the kubelet (no single list can be mandatory when any one
# of the four suffices), but auth.NewAuthManager refuses to start when all four
# are empty, because an empty whitelist admits every account that completes the
# OAuth flow. Optional to the kubelet, fatal at startup: catch it here rather
# than as a crashloop.
REQUIRED_ANY=(
  "accretion-auth-allowed-emails"
  "accretion-auth-allowed-domains"
  "accretion-auth-admin-emails"
  "accretion-auth-admin-domains"
)

# Machine callers. "<identity>|<ENV_VAR>|<permissions>" — the identity name is
# the one the chart's apiKeys list uses (and becomes a Secret data key, so it is
# a DNS-1123 label), the variable holds that caller's plaintext, and the
# permissions are what the chart must grant it.
#
# Permissions are matched EXACTLY by the server, with no hierarchy: an identity
# without chat:query gets a 403 from /api/chat and spends no LLM tokens, which
# is why ci has only read:memory.
#
# `|` separates the fields because a permission contains a colon.
IDENTITIES=(
  "accretion-mcp|ACCRETION_API_KEY_MCP|read:memory,chat:query"
  "accretion-discord|ACCRETION_API_KEY_DISCORD|read:memory,chat:query"
  "ci|ACCRETION_API_KEY_CI|read:memory"
)

# The identities whose credentials the two client Secrets publish — one each,
# because one credential per caller is what makes a caller revocable on its own.
# ci is deliberately absent: it reads its key from its environment and needs no
# object in the cluster.
#
# The bot's entry used to say "the bot's own chart Secret", which named a Secret
# that has not existed since #122 removed plaintext identities; the bot now has
# a Secret of its own here (DISCORD_SECRET_NAME above).
MCP_IDENTITY="accretion-mcp"
DISCORD_IDENTITY="accretion-discord"

# SESSION_KEY is the one required value whose CONTENT, not just its presence,
# decides whether the pod stays up: the server HMACs session cookies with it and
# refuses to start on anything weaker than 32 bytes. The accepted form is hex,
# with an even character count, and AT LEAST 64 characters — a minimum, not an
# exact length, so a longer key the server would accept is never rejected here.
# `openssl rand -hex 32` emits exactly that minimum: 64 characters DECODING to
# 32 bytes. Reading that "32" as a character count is what shipped a
# half-strength key and cost a 30-minute crashloop, so the check below is
# unconditional and its message always names both counts.
#
# The authority for the rule is NewAuthManager in internal/auth/auth.go
# (#65); change one and change the other. The byte count is the single source of
# truth and the character count is derived from it, so the two cannot drift.
SESSION_KEY_MIN_BYTES=32
session_key_min_chars=$((SESSION_KEY_MIN_BYTES * 2))

# Print the header comment block (everything after the shebang up to the first
# non-comment line) as help text.
usage() {
  awk 'NR == 1 { next } !/^#/ { exit } { sub(/^# ?/, ""); print }' "$0"
}

while [ $# -gt 0 ]; do
  case "$1" in
    -n|--namespace) NAMESPACE="$2"; shift 2 ;;
    -f|--env-file)  ENV_FILE="$2"; shift 2 ;;
    --dry-run)      MODE="print"; shift ;;
    --print-keys)   MODE="keys"; shift ;;
    --print-identities) MODE="identities"; shift ;;
    --print-identity-var) MODE="identity-var"; IDENTITY_ARG="$2"; shift 2 ;;
    -h|--help)      usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ "$MODE" = "keys" ]; then
  for entry in "${MAPPINGS[@]}"; do printf '%s\n' "${entry%%:*}"; done | sort -u
  exit 0
fi

# The env file is read ONCE, here, into a newline-delimited NAME=VALUE table
# held in a single variable. Every lookup is then a shell-builtin scan of that
# table instead of a `grep | tail` pair per variable — the script asks for ~12
# variables per run and CI drives it several times per `make secrets-check`, so
# the two forks per lookup were the whole measurable cost of every guard.
# Parsed rather than sourced so a .env cannot execute shell.
#
# Spaces around `=` are accepted (`VAR = value`), because a reader who writes
# that reasonably expects it to work; the value is stripped either way.
ENV_FILE_TABLE=
load_env_file() {
  local line var value
  ENV_FILE_TABLE=$'\n'
  [ -f "$ENV_FILE" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      ''|'#'*) continue ;;
    esac
    # Split on the first `=`; reject lines without one, and lines whose name
    # is not a plausible shell variable.
    case "$line" in
      *=*) ;;
      *) continue ;;
    esac
    var="${line%%=*}"
    value="${line#*=}"
    # Trim whitespace around the name and the value.
    var="${var#"${var%%[![:space:]]*}"}"; var="${var%"${var##*[![:space:]]}"}"
    value="${value#"${value%%[![:space:]]*}"}"; value="${value%"${value##*[![:space:]]}"}"
    case "$var" in
      ''|*[!A-Za-z0-9_]*) continue ;;
    esac
    value="${value#\"}"; value="${value%\"}"
    value="${value#\'}"; value="${value%\'}"
    # Last assignment wins: append, and read back the LAST match.
    ENV_FILE_TABLE="${ENV_FILE_TABLE}${var}=${value}"$'\n'
  done < "$ENV_FILE"
}

# Read VAR out of the table loaded above. Builtin-only: no fork.
env_file_value() {
  local var="$1" rest match line
  rest="$ENV_FILE_TABLE"
  match=
  while :; do
    case "$rest" in
      *$'\n'"$var="*) ;;
      *) break ;;
    esac
    rest="${rest#*$'\n'"$var="}"
    line="${rest%%$'\n'*}"
    match="$line"
  done
  [ -n "$match" ] || return 0
  printf '%s' "$match"
}


resolve() {
  local value
  value="$(env_file_value "$1")"
  # Fall back to the process environment (direnv-exported values, CI). Indirect
  # expansion rather than `printenv` so this costs no fork: with 12 mapping rows
  # and a small .env most rows reach this line, and a fork each was the other
  # half of the per-run cost — reading the file once does not remove it.
  [ -n "$value" ] || value="${!1:-}"
  [ -n "$value" ] || return 0
  printf '%s' "$value"
}

# Sprint-6 namespaced both LLM keys. A .env still assigning the bare name gets
# that value silently dropped from both the Secret and the app config, while
# .envrc's `dotenv_if_exists .env` keeps exporting the bare name to every local
# tool — the exact collision the rename existed to remove. Advisory only: the
# outcome is an absent key rather than a wrong one, so this warns and leaves the
# exit code alone. Only the variable NAME is printed, never its value.
#
# Checked against the env FILE, not the process environment: an exported bare
# name may legitimately come from somewhere else entirely.
#
# The replacement field is prose, not necessarily one name: #44 retired a single
# shared key in favour of one credential per caller, so the honest advice names
# the three and lets the reader pick the one their caller uses.
STALE_VARS=(
  "OPENAI_API_KEY:ACCRETION_LLM_OPENAI_API_KEY"
  "ANTHROPIC_API_KEY:ACCRETION_LLM_ANTHROPIC_API_KEY"
  # #44 removed AuthConfig.APIKeys, its viper default and this binding, ending
  # the legacy shared unscoped bearer-token allowlist; #41/#42 had already
  # replaced it with one scoped identity per caller, whose plaintext #122 moved
  # into .env under the three names below. Nothing has read this name since:
  # `auth.api_keys` is not a viper key, and the list that DID replace it
  # (auth.api_key_identities) is deliberately file-only -- "viper has no env
  # binding for a struct list" (internal/config/config.go).
  #
  # It is here late because STALE_VARS did not exist at #44: the list was added
  # in sprint-6 for the LLM rename that motivated it, and the names retired
  # before that were never backfilled. #44's own message records a
  # `grep -rn "api_keys\|ACCRETION_AUTH_API_KEYS"` sweep of the repository --
  # which is exactly the sweep that cannot reach a gitignored .env. This warning
  # is the only mechanism that can.
  "ACCRETION_AUTH_API_KEYS:ACCRETION_API_KEY_MCP, ACCRETION_API_KEY_DISCORD or ACCRETION_API_KEY_CI"
)
warn_stale_vars() {
  local entry stale
  for entry in "${STALE_VARS[@]}"; do
    stale="${entry%%:*}"
    [ -n "$(env_file_value "$stale")" ] || continue
    echo "warning: ${ENV_FILE} assigns ${stale}, which nothing reads any more." >&2
    echo "         Its value is being IGNORED. Rename it to ${entry#*:}." >&2
  done
}

# SHA-256 of stdin's exact bytes, hex, no trailing newline in the input. The
# tool differs by platform and this script runs on both: shasum ships with
# macOS, sha256sum with coreutils on Linux. Both are checked rather than one
# being assumed, because a missing tool here would otherwise surface as an empty
# digest that silently matches nothing.
#
# Byte-identical to what the chart used to compute with Helm's sha256sum, and to
# the `printf '%s' "$KEY" | shasum -a 256` in env.example: no trailing newline,
# which is why printf is used and never echo.
sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    printf '%s' "$1" | shasum -a 256 | cut -d' ' -f1
  elif command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$1" | sha256sum | cut -d' ' -f1
  else
    echo "error: neither shasum nor sha256sum is available, so an API-key hash cannot be computed." >&2
    exit 1
  fi
}

# The apiKeys block this .env implies: one entry per identity whose plaintext is
# set, carrying the digest the SERVER must hold. Paste it into the file that
# declares the identities.
#
# Identities with no plaintext are skipped and named on stderr rather than
# emitted with an empty hash, which would be an entry that authenticates nobody
# and reports nothing.
print_identities() {
  local entry name var perms plaintext printed=0
  printf 'apiKeys:\n'
  for entry in "${IDENTITIES[@]}"; do
    name="${entry%%|*}"
    var="${entry#*|}"; var="${var%%|*}"
    perms="${entry##*|}"
    plaintext="$(resolve "$var")"
    if [ -z "$plaintext" ]; then
      echo "note: ${name} skipped — ${var} is not set in ${ENV_FILE}. Generate one with: openssl rand -hex 32" >&2
      continue
    fi
    printf '  - name: %s\n' "$name"
    printf '    hash: "%s"\n' "$(sha256_of "$plaintext")"
    printf '    permissions: [%s]\n' "$(printf '%s' "$perms" | sed 's/,/", "/g; s/^/"/; s/$/"/')"
    printed=$((printed + 1))
  done
  if [ "$printed" -eq 0 ]; then
    echo "note: no identity had a plaintext in ${ENV_FILE}, so the list above is empty. An empty apiKeys list is refused at render time by the chart's fail-closed guard." >&2
  fi
}

# The env variable holding one identity's plaintext, by identity name. Two
# client Secrets below each publish exactly one identity, so this is written
# once rather than as two copies of the same scan — the second copy is where a
# renamed field would have gone unnoticed. Prints nothing for an unknown name:
# the two Secret callers below treat that the same as an unset variable, and
# --print-identity-var turns it into an error instead.
identity_var() {
  local entry
  for entry in "${IDENTITIES[@]}"; do
    [ "${entry%%|*}" = "$1" ] || continue
    entry="${entry#*|}"
    printf '%s' "${entry%%|*}"
    return 0
  done
}

# --print-identity-var: the NAME of the variable holding one identity's
# plaintext, and nothing else. No value is read, so this mode never touches the
# env file and is safe to run in a shell carrying real credentials.
#
# It exists because callers OUTSIDE this script need a caller's plaintext too --
# `make test-functional` runs the suite as accretion-mcp and as ci -- and the
# alternative is those callers restating "accretion-mcp -> ACCRETION_API_KEY_MCP"
# as a literal. That is the same shape as writing `api-key-accretion-discord`
# out by hand instead of deriving it (see DISCORD_SECRET_NAME above): a renamed
# identity leaves the literal pointing at a variable nobody sets, and an unset
# variable reads as an empty credential.
#
# An unknown name is FATAL rather than empty. identity_var prints nothing for
# one, and a caller reading that as "the variable is unset" would fall straight
# into the empty-credential failure this mode exists to keep out.
if [ "$MODE" = "identity-var" ]; then
  identity_var_name="$(identity_var "$IDENTITY_ARG")"
  if [ -z "$identity_var_name" ]; then
    echo "error: no API-key identity named '${IDENTITY_ARG}'. Known identities:" >&2
    for entry in "${IDENTITIES[@]}"; do echo "  ${entry%%|*}" >&2; done
    exit 1
  fi
  printf '%s\n' "$identity_var_name"
  exit 0
fi

# The file's one membership mechanism: in_list <needle> <haystack...>. A linear
# scan rather than an associative array because macOS ships bash 3.2, which has
# no `declare -A`, and Tilt invokes this with whatever `bash` is on PATH (#54).
in_list() {
  local needle="$1" item
  shift
  for item in "$@"; do [ "$item" != "$needle" ] || return 0; done
  return 1
}

# Secret VALUES are never passed as arguments. Anything in argv lands in
# /proc/<pid>/cmdline, which is world-readable on Linux for the life of the
# call, and this script handles the session signing key, the OAuth client
# secret, and the LLM key. Each value is written to a 0600 file in a private
# temp directory and handed to kubectl as --from-file=<key>=<path>, so only the
# key NAMES (not secret) ever appear in the command line. --from-file copies the
# bytes verbatim, so `printf %s` with no trailing newline is exact.
workdir="$(mktemp -d)"
cleanup() { [ -n "${workdir:-}" ] && rm -rf -- "$workdir"; }
trap cleanup EXIT INT TERM
chmod 700 "$workdir"

# One read, before anything asks for a value.
load_env_file

warn_stale_vars

if [ "$MODE" = "identities" ]; then
  print_identities
  exit 0
fi

args=()
missing=()
have_whitelist=0
# One row per key, so one pass: whether a key resolved is decided here and
# nowhere else, with no set of already-seen keys to keep.
for entry in "${MAPPINGS[@]}"; do
  key="${entry%%:*}"
  value="$(resolve "${entry#*:}")"
  if [ -z "$value" ]; then
    ! in_list "$key" "${REQUIRED[@]}" || missing+=("$key")
    continue
  fi
  (umask 077 && printf '%s' "$value" > "$workdir/$key")
  args+=("--from-file=${key}=${workdir}/${key}")
  ! in_list "$key" "${REQUIRED_ANY[@]}" || have_whitelist=1
done

if [ "${#missing[@]}" -gt 0 ]; then
  echo "error: ${ENV_FILE} is missing values the chart requires (their secretKeyRefs are not optional, so the pod will not start):" >&2
  printf '  %s\n' "${missing[@]}" >&2
  echo "Copy env.example to ${ENV_FILE} and fill them in." >&2
  exit 1
fi

# Strength, now that presence is established. Read back from the 0600 workdir
# file rather than kept from the loop, and reported by length only — the value
# is never echoed, interpolated into a message, or passed in argv.
session_key="$(cat "$workdir/session-key")"
# The hex class is ENUMERATED, not a `a-f` range: bracket ranges are matched by
# the locale's collation order, and under a UTF-8 locale accented Latin letters
# collate inside a-f — so a range would accept "ÁBCDEF..." as hex and let exactly
# the key this guard exists to stop through to the server.
case "$session_key" in
  *[!0123456789abcdefABCDEF]*) session_key_is_hex=0 ;;
  *)                           session_key_is_hex=1 ;;
esac
session_key_chars="${#session_key}"
unset session_key

if [ "$session_key_is_hex" -ne 1 ] ||
   [ $((session_key_chars % 2)) -ne 0 ] ||
   [ "$session_key_chars" -lt "$session_key_min_chars" ]; then
  echo "error: SESSION_KEY is not a key the server will accept, so the pod would crashloop at startup:" >&2
  echo "  required: at least ${session_key_min_chars} hexadecimal characters, decoding to ${SESSION_KEY_MIN_BYTES} bytes" >&2
  echo "            (the '32' is a BYTE count, not a character count)" >&2
  if [ "$session_key_is_hex" -ne 1 ]; then
    echo "  found:    ${session_key_chars} characters, but not hexadecimal — only hex is accepted" >&2
  elif [ $((session_key_chars % 2)) -ne 0 ]; then
    echo "  found:    ${session_key_chars} hexadecimal characters — an odd count, so it decodes to no whole number of bytes" >&2
  else
    echo "  found:    ${session_key_chars} hexadecimal characters, decoding to $((session_key_chars / 2)) bytes" >&2
  fi
  echo "Generate one with:  openssl rand -hex 32" >&2
  echo "and set it as SESSION_KEY in ${ENV_FILE}." >&2
  exit 1
fi

if [ "$have_whitelist" -eq 0 ]; then
  echo "error: ${ENV_FILE} sets none of the user-whitelist variables. An empty whitelist means" >&2
  echo "'allow every authenticated user', so the server refuses to start rather than come up" >&2
  echo "silently open. Set at least one of:" >&2
  for entry in "${MAPPINGS[@]}"; do
    key="${entry%%:*}"
    canonical="${entry#*:}"
    ! in_list "$key" "${REQUIRED_ANY[@]}" || printf '  %s\n' "${canonical%%:*}" >&2
  done
  exit 1
fi

manifest="$(kubectl create secret generic "$SECRET_NAME" \
  --namespace "$NAMESPACE" \
  "${args[@]}" \
  --dry-run=client -o yaml)"

# accretion-mcp-secrets, under the exact keys internal/mcp/secrets resolves.
#
# Skipped, loudly, when the MCP identity has no plaintext: a fresh clone
# deliberately holds no working credential (that is the whole point of #122), and
# publishing a Secret with an empty ACCRETION_API_KEY would give the MCP a key
# that authenticates as nobody and a 401 with nothing explaining it. Not fatal,
# because the accretion-api Secret above is what the pod's non-optional
# secretKeyRefs need and it must still be created.
#
# ACCRETION_API_URL falls back to the in-cluster Service address the chart used
# to compute, so a cluster install with no URL in .env behaves as it did before;
# .envrc already exports the localhost port-forward URL for local development,
# and an env var overrides the Secret value at the MCP anyway.
mcp_var="$(identity_var "$MCP_IDENTITY")"
mcp_plaintext=""
[ -z "$mcp_var" ] || mcp_plaintext="$(resolve "$mcp_var")"

if [ -n "$mcp_plaintext" ]; then
  mcp_url="$(resolve ACCRETION_API_URL)"
  [ -n "$mcp_url" ] || mcp_url="https://${API_SERVICE_NAME}.${NAMESPACE}.svc.cluster.local:${API_SERVICE_PORT}"
  (umask 077 && printf '%s' "$mcp_url" > "$workdir/ACCRETION_API_URL")
  (umask 077 && printf '%s' "$mcp_plaintext" > "$workdir/ACCRETION_API_KEY")
  # Which CA pointer, and it is a PRIVILEGE choice rather than a stylistic one.
  #
  # ACCRETION_API_CA_CONFIGMAP names a ConfigMap of public CA material and is
  # least privilege. ACCRETION_API_CA_SECRET names the serving Secret, which
  # ALSO holds the API listener's private key, because Kubernetes RBAC cannot
  # narrow a read to one key of an object -- so pointing MCP users there grants
  # every one of them `get` on the private key, and the whole Secret travels to
  # their machine.
  #
  # The chart used to offer this choice as mcpSecret.caConfigMap and #122 removed
  # the template; the choice has to survive the move or the least-privilege path
  # is simply gone. Set ACCRETION_API_CA_CONFIGMAP to a CA-only ConfigMap
  # (cert-manager's trust-manager publishes exactly this) and MCP users need no
  # access to the key at all. The Secret remains the default only because it is
  # the one object that is certain to exist.
  #
  # Accepts "name" or "namespace/name": the MCP reads the variable as
  # "namespace/name", so an operator reasoning from that shape writes the
  # qualified form, and blindly prefixing it would yield "ns/ns/name".
  mcp_ca_configmap="$(resolve ACCRETION_API_CA_CONFIGMAP)"
  if [ -n "$mcp_ca_configmap" ]; then
    case "$mcp_ca_configmap" in
      */*) ;;
      *)   mcp_ca_configmap="${NAMESPACE}/${mcp_ca_configmap}" ;;
    esac
    (umask 077 && printf '%s' "$mcp_ca_configmap" > "$workdir/ACCRETION_API_CA_CONFIGMAP")
    mcp_ca_arg="--from-file=ACCRETION_API_CA_CONFIGMAP=$workdir/ACCRETION_API_CA_CONFIGMAP"
  else
    (umask 077 && printf '%s/%s' "$NAMESPACE" "$API_TLS_SECRET_NAME" > "$workdir/ACCRETION_API_CA_SECRET")
    mcp_ca_arg="--from-file=ACCRETION_API_CA_SECRET=$workdir/ACCRETION_API_CA_SECRET"
  fi
  mcp_manifest="$(kubectl create secret generic "$MCP_SECRET_NAME" \
    --namespace "$NAMESPACE" \
    --from-file="ACCRETION_API_URL=$workdir/ACCRETION_API_URL" \
    --from-file="ACCRETION_API_KEY=$workdir/ACCRETION_API_KEY" \
    "$mcp_ca_arg" \
    --dry-run=client -o yaml)"
  manifest="$manifest
---
$mcp_manifest"
else
  echo "note: ${MCP_SECRET_NAME} not created — ${mcp_var:-ACCRETION_API_KEY_MCP} is not set in ${ENV_FILE}." >&2
  echo "      The MCP client configs resolve that Secret, so accretion-mcp will have no credential." >&2
  echo "      Generate one with: openssl rand -hex 32   then: scripts/make-secret.sh --print-identities" >&2
fi

# accretion-discord-secrets — the bot's own credential for accretion-api, under
# the key the accretion-discord chart's api.secretKey names.
#
# The key name is DERIVED from the identity rather than written out, because the
# accretion-api chart builds the same name the same way (api-key-<name> in
# templates/secret.yaml) and the two must agree. Writing the literal is how a
# renamed identity would leave a Secret whose key nothing reads.
#
# Skipped, loudly, on the same terms as the MCP Secret above: a fresh clone
# holds no working credential by design, and a Secret carrying an empty key
# would authenticate as nobody and 401 with nothing explaining it. The bot's
# secretKeyRef is not optional, so the pod stays unstarted with the missing
# Secret named in its events — which is the loud failure, and better than a bot
# that connects to Discord and then fails every query.
discord_var="$(identity_var "$DISCORD_IDENTITY")"
discord_plaintext=""
[ -z "$discord_var" ] || discord_plaintext="$(resolve "$discord_var")"
discord_key="api-key-${DISCORD_IDENTITY}"

if [ -n "$discord_plaintext" ]; then
  (umask 077 && printf '%s' "$discord_plaintext" > "$workdir/$discord_key")
  discord_manifest="$(kubectl create secret generic "$DISCORD_SECRET_NAME" \
    --namespace "$NAMESPACE" \
    --from-file="${discord_key}=${workdir}/${discord_key}" \
    --dry-run=client -o yaml)"
  manifest="$manifest
---
$discord_manifest"
else
  echo "note: ${DISCORD_SECRET_NAME} not created — ${discord_var:-ACCRETION_API_KEY_DISCORD} is not set in ${ENV_FILE}." >&2
  echo "      The accretion-discord chart reads ${discord_key} from that Secret and its ref is not" >&2
  echo "      optional, so the bot's pod will not start until it exists." >&2
  echo "      Generate one with: openssl rand -hex 32   then: scripts/make-secret.sh --print-identities" >&2
fi

if [ "$MODE" = "print" ]; then
  printf '%s\n' "$manifest"
else
  printf '%s\n' "$manifest" | kubectl apply -f -
fi

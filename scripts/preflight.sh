#!/usr/bin/env bash
#
# Verify the local environment can actually run accretion before a run target
# starts anything.
#
# Why this exists: every dependency here fails late and obscurely. A missing
# Ollama model surfaces as embed errors from a worker minutes into ingestion,
# after the circuit breaker has already tripped; an embedding model of the wrong
# width surfaces as a Postgres type error on insert, because the pgvector column
# is declared vector(768) and the model silently decides the other half of that
# contract; an unreachable database surfaces as a pod that restarts until
# someone reads its logs. All of them are knowable in about a second, up front.
#
# Usage:
#   scripts/preflight.sh                 # tools + endpoints
#   scripts/preflight.sh --tools-only    # skip anything that touches the network
#   scripts/preflight.sh --quiet         # only warnings and failures
#
# Configuration is read from the environment, which .envrc/.env populate — the
# same values the binary itself reads, so a check here and a failure at runtime
# cannot disagree about what was configured.
#
# Exit codes: 0 all required checks passed (warnings allowed), 1 something
# required is missing, broken, or unreachable.

set -uo pipefail

# Minimum versions. Go tracks .tool-versions; the rest are the oldest releases
# carrying the features this repo uses (helm 3 subcharts, kubectl -o jsonpath,
# kind config v1alpha4, tilt's k8s_custom_deploy).
MIN_GO="$(awk '/^golang /{print $2}' .tool-versions 2>/dev/null || echo 1.24.5)"
MIN_HELM=3.8.0
MIN_KUBECTL=1.28.0
MIN_KIND=0.20.0
MIN_TILT=0.33.0
MIN_DOCKER=20.10.0

# The pgvector column is declared vector(768) in the schema, so the configured
# embedding model must produce exactly that width or every insert fails.
EXPECTED_EMBED_DIM=768

TOOLS_ONLY=0
QUIET=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tools-only) TOOLS_ONLY=1; shift ;;
    --quiet)      QUIET=1; shift ;;
    -h|--help)    sed -n '2,24p' "$0" | sed 's/^#//; s/^ //'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [[ -t 1 ]]; then
  RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'
  BLUE=$'\033[0;34m'; DIM=$'\033[2m'; NC=$'\033[0m'
else
  RED=""; GREEN=""; YELLOW=""; BLUE=""; DIM=""; NC=""
fi

FAILURES=0
WARNINGS=0

section() { [[ $QUIET -eq 1 ]] || printf '\n%s▶ %s%s\n' "$BLUE" "$1" "$NC"; }
ok()   { [[ $QUIET -eq 1 ]] || printf '  %s✓%s %-11s %-26s %s%s%s\n' "$GREEN" "$NC" "$1" "$2" "$DIM" "${3:-}" "$NC"; }
warn() { printf '  %s!%s %-11s %-26s %s\n' "$YELLOW" "$NC" "$1" "$2" "${3:-}"; WARNINGS=$((WARNINGS+1)); }
fail() { printf '  %s✗%s %-11s %-26s %s\n' "$RED" "$NC" "$1" "$2" "${3:-}"; FAILURES=$((FAILURES+1)); }
hint() { printf '    %s↳ %s%s\n' "$DIM" "$1" "$NC"; }

# shellcheck source=scripts/lib/semver.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib/semver.sh"

# check_tool NAME MIN VERSION_CMD REQUIRED HINT
check_tool() {
  local name="$1" min="$2" cmd="$3" required="$4" install_hint="$5"
  if ! command -v "$name" >/dev/null 2>&1; then
    if [[ "$required" == "required" ]]; then
      fail "$name" "not installed" "need >= $min"
      hint "$install_hint"
    else
      warn "$name" "not installed" "optional — $install_hint"
    fi
    return
  fi
  local raw got
  raw="$(eval "$cmd" 2>/dev/null)"
  got="$(version_of "$raw")"
  if [[ -z "$got" ]]; then
    warn "$name" "installed" "version not parseable, skipping check"
    return
  fi
  if version_ge "$got" "$min"; then
    ok "$name" "$got" ">= $min"
  elif [[ "$required" == "required" ]]; then
    fail "$name" "$got" "too old, need >= $min"
    hint "$install_hint"
  else
    warn "$name" "$got" "older than $min"
  fi
}

# http_get URL — body on stdout, and the HTTP status plus elapsed ms on fd 3.
# One curl invocation so the reported latency is the one actually measured.
#
# ACCRETION_API_CA_FILE, when set, is passed as --cacert so an https URL is
# VERIFIED rather than skipped. There is deliberately no -k anywhere in this
# script: preflight exists to catch the misconfiguration, and a check that
# skipped verification would report a healthy endpoint no real client can reach.
http_get() {
  local ca=()
  [[ -n "${ACCRETION_API_CA_FILE:-}" ]] && ca=(--cacert "$ACCRETION_API_CA_FILE")
  # ${ca[@]+"${ca[@]}"}, not "${ca[@]}": under `set -u` bash 3.2 treats an EMPTY
  # array's [@] expansion as an unbound variable and aborts the function. macOS
  # ships bash 3.2.57, so the plain form made every http_get call here fail on
  # the platform this script exists to check — and fail as "no connection",
  # which reads like a down endpoint rather than a broken script.
  curl -sS --max-time 5 ${ca[@]+"${ca[@]}"} -w '\n%{http_code} %{time_total}' "$1" 2>/dev/null
}

# tcp_open HOST PORT — true when a TCP connect succeeds within 3s. Uses bash's
# /dev/tcp so no netcat dependency is introduced.
tcp_open() {
  timeout 3 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null
}

# json_array_len JSON KEY — length of the first array under KEY, without
# requiring jq. Counts separators, which is exact for arrays of numbers (all we
# use it for: an embedding vector).
json_array_len() {
  local frag
  frag="$(printf '%s' "$1" | tr -d '\n ' | sed -n "s/.*\"$2\":\[*\[\([^]]*\)\].*/\1/p" | head -1)"
  [[ -z "$frag" ]] && { echo 0; return; }
  printf '%s' "$frag" | tr ',' '\n' | grep -c .
}

################################  tools  ################################

section "Preflight — tools"

check_tool go       "$MIN_GO"      "go version"                      required "install go $MIN_GO (see .tool-versions)"
check_tool docker   "$MIN_DOCKER"  "docker --version"                required "https://docs.docker.com/get-docker/"
check_tool kubectl  "$MIN_KUBECTL" "kubectl version --client=true -o yaml" required "https://kubernetes.io/docs/tasks/tools/"
check_tool helm     "$MIN_HELM"    "helm version --short"            required "https://helm.sh/docs/intro/install/"
check_tool kind     "$MIN_KIND"    "kind --version"                  required "go install sigs.k8s.io/kind@latest"
check_tool tilt     "$MIN_TILT"    "tilt version"                    required "https://docs.tilt.dev/install.html"
check_tool direnv   "0.0.0"        "direnv --version"                optional "brew install direnv  (loads .envrc/.env automatically)"

# air is checked by identity, not presence. `make dev` runs whatever `air` is on
# PATH, and the name collides — JetBrains Fleet ships a launcher called `air`.
# Finding the wrong one means `make dev` silently starts something else entirely,
# so a bare `command -v air` is not a useful answer here.
if ! command -v air >/dev/null 2>&1; then
  warn "air" "not installed" "optional — make dev falls back to go run"
  hint "make go-tools"
else
  air_out="$(air -v 2>&1)"
  air_ver="$(version_of "$air_out")"
  if [[ -n "$air_ver" ]] && ! printf '%s' "$air_out" | grep -qi 'fleet'; then
    ok "air" "$air_ver" "live reload for make dev"
  else
    warn "air" "wrong binary on PATH" "$(command -v air)"
    hint "this is not air-verse/air — make dev would run it instead of the server"
    hint "install the real one: make go-tools"
  fi
fi

# The go toolchain that will actually build, which can differ from `go version`
# when go.mod names a newer one and GOTOOLCHAIN fetches it.
if command -v go >/dev/null 2>&1; then
  declared="$(awk '/^toolchain /{sub(/^go/,"",$2); print $2}' go.mod 2>/dev/null)"
  running="$(version_of "$(go version 2>/dev/null)")"
  if [[ -n "$declared" && -n "$running" ]] && ! version_ge "$running" "$declared"; then
    warn "toolchain" "$running" "go.mod asks for $declared; GOTOOLCHAIN will fetch it"
  fi
fi

# Docker has to be running, not merely installed — kind and Tilt both need the
# daemon, and `docker --version` answers happily without it.
if command -v docker >/dev/null 2>&1; then
  if docker info >/dev/null 2>&1; then
    ok "docker" "daemon responding" "$(docker info --format '{{.ServerVersion}}' 2>/dev/null)"
  else
    fail "docker" "daemon not responding" "docker is installed but not reachable"
    if [[ "$(uname -s)" == "Linux" ]]; then
      hint "start it:      sudo systemctl start docker"
      hint "permission?:   sudo usermod -aG docker \"$USER\" then log out and back in"
    else
      hint "start Docker Desktop, then re-run"
    fi
  fi
fi

##############################  contributor  ##############################

# These are about the checkout rather than the running app, and they are checked
# because both have failed silently before: `pre-commit install` without
# --hook-type leaves the release-tag gate inert, and a Go tool installed under a
# previous toolchain stops resolving when the toolchain moves.

section "Preflight — contributor setup"

if [[ ! -f .env ]]; then
  warn ".env" "missing" "run: make env-file, then fill in your secrets"
else
  ok ".env" "present" "secrets loaded by .envrc"
fi

if ! command -v pre-commit >/dev/null 2>&1; then
  warn "pre-commit" "not installed" "no checks run before your commits"
  if [[ "$(uname -s)" == "Linux" ]]; then
    hint "pipx install pre-commit   (or apt/dnf install pre-commit)"
  else
    hint "brew install pre-commit"
  fi
else
  ok "pre-commit" "$(version_of "$(pre-commit --version 2>/dev/null)")" "installed"

  # Presence of the config says nothing about whether the hooks are wired in.
  # Both stages matter: the chart version check runs at commit, the release tag
  # check at push, and installing only the first leaves the second inert.
  for stage in pre-commit pre-push; do
    if [[ -f ".git/hooks/$stage" ]] && grep -q "pre-commit" ".git/hooks/$stage" 2>/dev/null; then
      ok "  $stage" "hook installed" "runs on git $stage"
    else
      warn "  $stage" "hook NOT installed" "run: make install-hooks"
    fi
  done
fi

for tool in golangci-lint gosec goimports; do
  if command -v "$tool" >/dev/null 2>&1; then
    # Not every tool reports a parseable version (goimports has no flag at all),
    # so presence is the check and the version is decoration when available.
    v="$(version_of "$("$tool" --version 2>&1 | head -1)")"
    ok "$tool" "${v:-installed}" "on PATH"
  else
    warn "$tool" "not installed" "run: make go-tools"
  fi
done

if [[ $TOOLS_ONLY -eq 1 ]]; then
  section "Preflight — endpoints"
  [[ $QUIET -eq 1 ]] || printf '  %s· skipped (--tools-only)%s\n' "$DIM" "$NC"
else

##############################  endpoints  ##############################

section "Preflight — endpoints"

PROVIDER="${ACCRETION_LLM_PROVIDER:-ollama}"
OLLAMA_URL="${ACCRETION_LLM_OLLAMA_BASE_URL:-http://localhost:11434}"
EMBED_MODEL="${ACCRETION_LLM_EMBEDDING_MODEL:-nomic-embed-text:latest}"
CHAT_MODEL="${ACCRETION_LLM_CHAT_MODEL:-}"
DB_HOST="${ACCRETION_DATABASE_HOST:-localhost}"
DB_PORT="${ACCRETION_DATABASE_PORT:-5432}"
DB_NAME="${ACCRETION_DATABASE_DATABASE:-accretion}"
DB_USER="${ACCRETION_DATABASE_USER:-postgres}"

ok "provider" "$PROVIDER" "ACCRETION_LLM_PROVIDER"

# --- Ollama. Always checked, not only when it is the chat provider: Anthropic
# has no embeddings API and delegates embedding here, so Ollama is on the
# critical path for every provider except OpenAI.
if [[ "$PROVIDER" != "openai" ]]; then
  resp="$(http_get "$OLLAMA_URL/api/tags")"
  code="$(printf '%s' "$resp" | tail -1 | awk '{print $1}')"
  secs="$(printf '%s' "$resp" | tail -1 | awk '{print $2}')"
  body="$(printf '%s' "$resp" | sed '$d')"

  if [[ "$code" != "200" ]]; then
    # curl reports 000 when the connection never completed, which is a
    # different problem from a server that answered with an error.
    if [[ "$code" == "000" || -z "$code" ]]; then
      fail "ollama" "$OLLAMA_URL" "no connection (refused or timed out)"
    else
      fail "ollama" "$OLLAMA_URL" "unhealthy (HTTP $code)"
    fi
    hint "start it with: ollama serve"
  else
    ok "ollama" "$OLLAMA_URL" "reachable in $(awk -v s="$secs" 'BEGIN{printf "%.0fms", s*1000}')"

    # Ollama reports "name:tag"; a bare configured name means the :latest tag.
    want_embed="$EMBED_MODEL"; [[ "$want_embed" == *:* ]] || want_embed="$want_embed:latest"
    if printf '%s' "$body" | grep -q "\"$want_embed\""; then
      ok "  embed" "$want_embed" "pulled"

      # The width contract with the pgvector column. Try the current /api/embed
      # first, then the older /api/embeddings, so this works across versions.
      dim=0
      emb="$(curl -sS --max-time 20 "$OLLAMA_URL/api/embed" \
             -d "{\"model\":\"$want_embed\",\"input\":\"preflight\"}" 2>/dev/null)"
      dim="$(json_array_len "$emb" "embeddings")"
      if [[ "$dim" -eq 0 ]]; then
        emb="$(curl -sS --max-time 20 "$OLLAMA_URL/api/embeddings" \
               -d "{\"model\":\"$want_embed\",\"prompt\":\"preflight\"}" 2>/dev/null)"
        dim="$(json_array_len "$emb" "embedding")"
      fi

      if [[ "$dim" -eq 0 ]]; then
        warn "  dim" "not determined" "could not embed a probe string"
      elif [[ "$dim" -eq "$EXPECTED_EMBED_DIM" ]]; then
        ok "  dim" "$dim" "matches schema vector($EXPECTED_EMBED_DIM)"
      else
        fail "  dim" "$dim" "schema declares vector($EXPECTED_EMBED_DIM)"
        hint "every insert would fail on a type error; change the model or the schema"
      fi
    else
      fail "  embed" "$want_embed" "not pulled"
      hint "ollama pull $want_embed"
    fi

    if [[ "$PROVIDER" == "ollama" && -n "$CHAT_MODEL" ]]; then
      want_chat="$CHAT_MODEL"; [[ "$want_chat" == *:* ]] || want_chat="$want_chat:latest"
      if printf '%s' "$body" | grep -q "\"$want_chat\""; then
        ok "  chat" "$want_chat" "pulled"
      else
        warn "  chat" "$want_chat" "not pulled — ollama pull $want_chat"
      fi
    fi
  fi
fi

# --- Hosted providers: presence of the key only. The key is never sent
# anywhere by this script; spending quota to prove a preflight point is not
# worth it, and an invalid key fails loudly at first use anyway.
case "$PROVIDER" in
  openai)
    if [[ -n "${ACCRETION_LLM_OPENAI_API_KEY:-}" ]]; then
      ok "openai" "key set" "${ACCRETION_LLM_OPENAI_BASE_URL:-https://api.openai.com/v1}"
    else
      fail "openai" "ACCRETION_LLM_OPENAI_API_KEY unset" "provider is openai"
      hint "set it in .env, then: direnv allow"
    fi ;;
  anthropic)
    if [[ -n "${ACCRETION_LLM_ANTHROPIC_API_KEY:-}" ]]; then
      ok "anthropic" "key set" "embeddings still delegate to ollama"
    else
      fail "anthropic" "ACCRETION_LLM_ANTHROPIC_API_KEY unset" "provider is anthropic"
      hint "set it in .env, then: direnv allow"
    fi ;;
esac

# --- Postgres. A TCP probe is the honest check before the cluster is up; when
# psql is present it also confirms the database and the pgvector extension,
# which is what the schema actually needs.
if tcp_open "$DB_HOST" "$DB_PORT"; then
  ok "postgres" "$DB_HOST:$DB_PORT" "accepting connections"
  if command -v psql >/dev/null 2>&1; then
    ext="$(PGPASSWORD="${ACCRETION_DATABASE_PASSWORD:-}" psql -h "$DB_HOST" -p "$DB_PORT" \
           -U "$DB_USER" -d "$DB_NAME" -tAc \
           "select extversion from pg_extension where extname='vector'" 2>/dev/null)"
    if [[ -n "$ext" ]]; then
      ok "  pgvector" "$ext" "extension installed in $DB_NAME"
    else
      warn "  pgvector" "not found in $DB_NAME" "db-init creates it; fine before first deploy"
    fi
  fi
else
  warn "postgres" "$DB_HOST:$DB_PORT" "not reachable — expected until the cluster is up"
fi

# --- Redis, the ingest queue. Only meaningful once something is running.
REDIS_ADDR="${ACCRETION_QUEUE_REDIS_ADDR:-}"
if [[ -n "$REDIS_ADDR" ]]; then
  if tcp_open "${REDIS_ADDR%%:*}" "${REDIS_ADDR##*:}"; then
    ok "redis" "$REDIS_ADDR" "accepting connections"
  else
    warn "redis" "$REDIS_ADDR" "not reachable — expected until the cluster is up"
  fi
fi

# --- accretion-api. Only checked when ACCRETION_API_URL names a target: the
# endpoint is reachable through a port-forward, which is not up during a
# tools-only or pre-cluster run.
if [[ -n "${ACCRETION_API_URL:-}" ]]; then
  api_url="${ACCRETION_API_URL%/}"
  resp="$(http_get "$api_url/api/health")"
  code="$(printf '%s' "$resp" | tail -1 | awk '{print $1}')"
  if [[ "$code" == "200" ]]; then
    ok "accretion-api" "$api_url" "healthy"
  elif [[ "$code" == "000" && "$api_url" == https://* ]]; then
    # curl reports 000 for a failed handshake exactly as it does for a refused
    # connection, so distinguish them: with the port open, 000 on an https URL
    # means the certificate did not verify — the single most likely TLS
    # misconfiguration, and invisible if reported as "not reachable".
    api_hostport="${api_url#https://}"
    if tcp_open "${api_hostport%%:*}" "${api_hostport##*:}"; then
      fail "accretion-api" "$api_url" "port is open but TLS verification failed"
      hint "set ACCRETION_API_CA_FILE to the CA that signed the API certificate (see README)"
    else
      warn "accretion-api" "$api_url" "not reachable — expected until the cluster is up"
    fi
  else
    warn "accretion-api" "$api_url" "returned $code — expected until the cluster is up"
  fi
fi

# --- The API certificate itself. Reachability is not the same question as
# validity: a certificate that expires in two days answers every probe today and
# takes the whole stack down on Thursday, and nothing else in this repo would say
# so first.
if [[ -n "${ACCRETION_API_CA_FILE:-}" ]]; then
  if [[ ! -f "$ACCRETION_API_CA_FILE" ]]; then
    warn "api cert" "$ACCRETION_API_CA_FILE" "CA file not found — run make up, which writes it"
  elif ! command -v openssl >/dev/null 2>&1; then
    ok "api cert" "$ACCRETION_API_CA_FILE" "present (install openssl to check expiry)"
  elif ! openssl x509 -in "$ACCRETION_API_CA_FILE" -noout >/dev/null 2>&1; then
    fail "api cert" "$ACCRETION_API_CA_FILE" "not a readable X.509 certificate"
  elif ! openssl x509 -in "$ACCRETION_API_CA_FILE" -noout -checkend 0 >/dev/null 2>&1; then
    fail "api cert" "$ACCRETION_API_CA_FILE" "EXPIRED — every client will refuse to connect"
    hint "re-issue it: kubectl delete secret accretion-api-tls -n accretion-core, then make up"
  elif ! openssl x509 -in "$ACCRETION_API_CA_FILE" -noout -checkend 604800 >/dev/null 2>&1; then
    warn "api cert" "$(openssl x509 -in "$ACCRETION_API_CA_FILE" -noout -enddate | cut -d= -f2)" "expires within 7 days"
  else
    ok "api cert" "$(openssl x509 -in "$ACCRETION_API_CA_FILE" -noout -enddate | cut -d= -f2)" "valid"
  fi
fi

# --- Tempo, only when traces are switched on.
if [[ "${ACCRETION_TEMPO_ENABLED:-false}" == "true" && -n "${ACCRETION_TEMPO_BASE_URL:-}" ]]; then
  resp="$(http_get "${ACCRETION_TEMPO_BASE_URL}/ready")"
  if [[ "$(printf '%s' "$resp" | tail -1 | awk '{print $1}')" == "200" ]]; then
    ok "tempo" "$ACCRETION_TEMPO_BASE_URL" "ready"
  else
    warn "tempo" "$ACCRETION_TEMPO_BASE_URL" "not ready — traces will not be queryable"
  fi
fi

fi  # end endpoints

################################  summary  ################################

printf '\n'
if [[ $FAILURES -gt 0 ]]; then
  printf '%s✗ preflight failed: %d problem(s), %d warning(s)%s\n' "$RED" "$FAILURES" "$WARNINGS" "$NC"
  printf '  %sfix the ✗ lines above, or run the target with SKIP_PREFLIGHT=1 to bypass%s\n' "$DIM" "$NC"
  exit 1
fi
if [[ $WARNINGS -gt 0 ]]; then
  printf '%s✓ preflight passed with %d warning(s)%s\n' "$GREEN" "$WARNINGS" "$NC"
else
  printf '%s✓ preflight passed — tools and endpoints all healthy%s\n' "$GREEN" "$NC"
fi
exit 0

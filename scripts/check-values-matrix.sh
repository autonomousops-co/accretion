#!/usr/bin/env bash
#
# Render every first-party chart under every caller's value set, and fail on any
# non-zero helm exit. Detector for #117.
#
# Why this exists: nothing verifies that the value sets our own callers build
# actually produce a valid release. Each caller assembles the full set
# independently -- `make deploy-auth STACK=x` points helm at a per-environment
# values file, the Tiltfile passed ~16 inline --set flags, and Pulumi built a
# pulumi.Map in Go -- and no gate rendered any of them. Two live breakages
# proved the gap: Pulumi's accretion-api release could not render at all (it
# supplied no `apiKeys` and loaded no values file, so the fail-closed
# empty-identities guard rejected it), and the Tilt stack stopped rendering when
# the plaintext-API-key default flipped, because the dev opt-out lived in a file
# Tilt never passed.
#
# Helm also ignores unknown value keys silently, so a renamed key lands as a
# no-op rather than an error. That is what let the Tiltfile keep passing
# mcpSecret.enabled and mcpSecret.url for a whole sprint after #122 deleted the
# block they configured. The drift check below is aimed at exactly that: a
# caller that renames or adds a value key without the matrix following is
# rejected by name.
#
# WHICH CALLERS THIS COVERS, and where the one it does not covers itself.
#
# Every caller reachable with helm alone is here: the chart defaults, the
# example file, the three `make deploy-*` paths, and Tilt. PULUMI IS NOT, and
# its absence is deliberate rather than an oversight -- #119 removed its rows.
# Since #120 that caller's static values live in deploy/pulumi.yaml and its
# deploy-time values are merged in Go, so no transcription here can render what
# Pulumi actually deploys: the accretion-api subtree alone does not render (it
# carries the identity ROSTER, while each digest arrives from stack config), and
# a row faking that up would have looked more authoritative than the one it
# replaced while testing a value set nobody deploys.
#
# Pulumi is covered by its own Go tests instead, which render the real value set
# with the real merging code, and by `UndeclaredPaths`, which compares caller
# keys against the charts at FULL DEPTH rather than at the top level this script
# can afford. Its CI job gained a helm setup step so those tests run there. So
# the claim this file makes is "every caller the charts job can reach, plus a
# named pointer to the one it cannot" -- not "every caller".
#
# Usage:
#   scripts/check-values-matrix.sh              # render the whole matrix
#   scripts/check-values-matrix.sh --selfcheck  # prove the detector fires
#
# --selfcheck drives the same engine over hand-built fixtures rather than a real
# chart, so its result never depends on which of this series' fixes have landed
# in this checkout.
#
# Dependencies are bash, awk, sed and helm, and nothing else. CI's `charts` job
# installs helm alone -- no tilt, no Go toolchain -- and this target is wired
# into `charts-check`, which that job runs. `tilt alpha tiltfile-result` would
# hand over perfectly resolved helm flags but is unavailable there, which is why
# the Tilt rows are transcribed below and paid for with the drift check instead.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

RED=$'\033[0;31m'
GREEN=$'\033[0;32m'
BLUE=$'\033[0;34m'
NC=$'\033[0m'

# A syntactically valid but unusable digest. The chart's empty-`apiKeys` guard is
# a deliberate fail-closed default (values.yaml), so a row that means "chart
# defaults and nothing else" still has to declare one identity or it fails for a
# reason that has nothing to do with the caller under test.
ZERO_DIGEST=0000000000000000000000000000000000000000000000000000000000000000
IDENTITY_STUB='--set-json apiKeys=[{"name":"ci","hash":"'"$ZERO_DIGEST"'","permissions":["read:memory"]}]'

# Stand-in for a caller value that is only known at deploy time -- a Pulumi
# stack output, an operator-supplied password. The matrix tests WHICH keys a
# caller supplies and that the resulting set renders, not what those keys
# resolve to on a particular stack.
PLACEHOLDER=values-matrix-placeholder

# The chart-defaults rows' database password, and the same kind of thing as
# IDENTITY_STUB above rather than a value under test (#134). accretion-api,
# accretion-collector and accretion-db all refuse to render with no password and
# no existingSecret -- a deliberate fail-closed default, like the empty-`apiKeys`
# one IDENTITY_STUB exists for -- so a row meaning "chart defaults and nothing
# else" still has to supply one or it fails for a reason that has nothing to do
# with the caller under test.
#
# Only the rows that render CHART DEFAULTS carry it. Every real caller below
# names a Secret instead, which is the shape #134 moved them to: the tilt rows
# get config.database.existingSecret out of deploy/tilt.yaml, the
# make-deploy-staging/prod rows pass the deploy-time password their values files
# document, and the dev and example files name accretion-api-secrets themselves.
# Adding it to a tilt row would ALSO break drift_check, correctly: the Tiltfile
# passes no such flag, and a matrix row that does is a row no longer describing
# its caller.
DB_PASSWORD_STUB='--set-string config.database.password=render-only-not-a-credential'
PG_PASSWORD_STUB='--set-string postgresql.auth.postgresPassword=render-only-not-a-credential'

# The Tiltfile's central values file (#119). Every static setting Tilt used to
# pass as an inline --set flag lives there, nested by chart name, and the
# Tiltfile materialises each chart's subtree at load.
#
# `@subtree-from:<file>:<chart>` in a row below is resolved at run time into
# `-f <a temp file holding that subtree>`, so the matrix renders the SAME values
# the Tiltfile passes rather than a transcription of them. helm is the YAML
# reader (see values_subtree_file), because it is the only parser this gate is
# allowed to depend on.
TILT_VALUES=deploy/tilt.yaml

# The Tiltfile derives accretion-api's identities from .env, through
# `scripts/make-secret.sh --print-identities`, so there is nothing here to
# borrow: the digests are per-developer credential material and a CI checkout
# holds none. The row therefore uses the same stub every other identity-needing
# row uses. What it tests is that the caller's value set RENDERS with a valid
# identity present, which is the question this matrix asks; whether a
# developer's own digests match their plaintext is `make secrets-identity-check`.
#
# This replaces an `@apiKeys-from:charts/accretion-api/values-dev.yaml`
# indirection that had gone hollow: #122 emptied that list, so the row was
# rejected for declaring no identities at all.

# ---------------------------------------------------------------------------
# The matrix. One row per (caller, chart):
#
#   caller|chart|drift-source|min-objects|helm args
#
# drift-source is the caller's own file when the row is a transcription of a
# value set that lives somewhere this script cannot execute, and `-` when the
# row cannot drift because it names the caller's real values file directly.
# min-objects is the number of rendered objects below which the row is treated
# as vacuous rather than clean; it is 0 only where rendering nothing IS the
# documented contract (accretion-discord is opt-in).
#
# The `make-deploy-*` rows are what `make deploy-auth STACK=x` and
# `make deploy-collector` run. None of the three API environments ships
# identities any more -- staging and prod never did, and dev stopped when #122
# moved the plaintext into .env -- so each of those rows adds exactly the
# deploy-time inputs its values file documents, and nothing else.
#
# The collector rows carry no values file because #123 deleted its
# per-environment ones; `make deploy-collector` now installs the chart defaults
# unless the operator passes COLLECTOR_VALUES. The second row is the scaled
# shape, which is a materially different render -- it brings up the controller,
# Redis and the Redis queue mode -- so it is worth a row of its own. It splits
# log and metric because that is what a scaled collector actually deploys; the
# unsplit form renders 25 objects against 35 and would leave sixteen ungated.
# This duplicates COLLECTOR_SCALED in the Makefile, unavoidably: this script is
# standalone bash and helm by design and cannot read a Makefile variable.
#
# values-example.yaml is a caller too, and the one a stranger starts from. It is
# not a deployment path, so nothing else here renders it -- which is how its
# output moved from 18 objects to 16 under #118 with no row watching. It calls
# itself a self-contained local install and declares no ingress block, so it had
# been silently inheriting the production GKE ingress and emitting an Ingress
# for a domain its reader does not own.
#
# The tilt rows name deploy/tilt.yaml through `@subtree-from:` rather than
# transcribing it, so the static half of that caller cannot drift at all: the
# matrix renders the very bytes the Tiltfile passes. What remains transcribed is
# only what the Tiltfile computes -- the identities, the monitored namespace list
# and, since #135, a single .env-sourced Discord value. It was three until then:
# the bot token and the Anthropic key were `--set` flags carrying live
# credentials into the Helm release, and they now arrive through a Secret named
# in the central file, so they are not transcribed here because the Tiltfile no
# longer passes them. discord.guildID stays a flag -- an ID, not a credential,
# and per-developer, so it has no place in a checked-in file either. That
# remainder is what drift_check compares. accretion-db and otel-collector take chart defaults, expressed in
# the central file as explicit empty maps, so their rows still name the subtree:
# the day one of them gains a setting, the row already covers it.
#
# Removing a caller from this matrix is a DATA change: delete its line. Nothing
# below reads the caller names except drift_check, which names only the one
# transcribed caller.
# ---------------------------------------------------------------------------
matrix_rows() {
  cat <<EOF
default|accretion-api|-|1|$IDENTITY_STUB $DB_PASSWORD_STUB
example|accretion-api|-|1|-f charts/accretion-api/values-example.yaml
default|accretion-collector|-|1|$DB_PASSWORD_STUB
default|accretion-db|-|1|$DB_PASSWORD_STUB $PG_PASSWORD_STUB
default|accretion-discord|-|0|
default|otel-collector|-|1|
make-deploy-dev|accretion-api|-|1|-f charts/accretion-api/values-dev.yaml $IDENTITY_STUB
make-deploy-staging|accretion-api|-|1|-f charts/accretion-api/values-staging.yaml $IDENTITY_STUB --set-string config.database.password=$PLACEHOLDER
make-deploy-prod|accretion-api|-|1|-f charts/accretion-api/values-prod.yaml $IDENTITY_STUB --set-string config.database.password=$PLACEHOLDER
make-deploy|accretion-collector|-|1|$DB_PASSWORD_STUB
make-deploy-scaled|accretion-collector|-|1|$DB_PASSWORD_STUB --set dataCollector.replicaCount=2 --set dataCollector.kinds.log.split=true --set dataCollector.kinds.metric.split=true --set-string redis.auth.password=render-only-not-a-credential
tilt|accretion-api|Tiltfile|1|@subtree-from:$TILT_VALUES:accretion-api $IDENTITY_STUB
tilt|accretion-collector|Tiltfile|1|@subtree-from:$TILT_VALUES:accretion-collector --set config.k8s.namespaces={default,accretion-db}
tilt|accretion-db|Tiltfile|1|@subtree-from:$TILT_VALUES:accretion-db
tilt|accretion-discord|Tiltfile|1|@subtree-from:$TILT_VALUES:accretion-discord --set discord.guildID=$PLACEHOLDER
tilt|otel-collector|Tiltfile|1|@subtree-from:$TILT_VALUES:otel-collector
EOF
}

# ---------------------------------------------------------------------------
# Engine
# ---------------------------------------------------------------------------

failures=0
rendered=0

fail() {
  printf '%b\n' "${RED}$1${NC}" >&2
  failures=$((failures + 1))
}

# helm as a YAML reader. The gate may depend on helm and nothing else, so a
# throwaway one-template chart is how it reads one chart's subtree back out of
# the central values file and writes it where helm can be pointed at it.
# $1 = top-level key (a chart name), $2 = central values file, $3 = scratch dir.
# Prints the path of the written file.
#
# The reader chart is built PER KEY rather than once: its template names the key
# it reads, so a cached chart from an earlier call would silently answer with
# the earlier key's subtree.
values_subtree_file() {
  # Declared separately, not as one `local a=.. b=$a` line: bash expands every
  # word of a `local` before it assigns any of them, so a later assignment
  # referring to an earlier one reads whatever `key` meant in the CALLER -- an
  # unbound-variable error under `set -u`, or worse, silently the wrong key.
  local key="$1" file="$2" d="$3"
  local chart="$d/subtree-$key" target="$d/subtree-$key.yaml" out
  if [ -f "$target" ]; then
    printf '%s' "$target"
    return 0
  fi
  mkdir -p "$chart/templates"
  printf 'apiVersion: v2\nname: subtreereader\nversion: 0.0.0\n' > "$chart/Chart.yaml"
  # JSON on ONE line, not a toYaml block. helm parses its own rendered output as
  # YAML before printing it, and a multi-line block under a bare marker line is
  # not valid YAML -- it fails with a parse error pointing at the reader chart,
  # which reads as a broken gate rather than as an unreadable subtree. JSON is
  # valid YAML, so the file written below is a values file helm accepts.
  printf '%s\n' 'SUBTREE: {{ toJson (index .Values "'"$key"'") }}' > "$chart/templates/dump.yaml"
  out=$(helm template subtreereader "$chart" -f "$file" 2>&1) || {
    echo "could not read the \`$key\` subtree out of $file: $out" >&2
    return 1
  }
  printf '%s\n' "$out" | sed -n 's/^SUBTREE: //p' | head -1 > "$target"
  # An absent subtree renders as `null`, which helm would accept as a values
  # file and merge as nothing -- a row that then renders chart defaults while
  # claiming to render the caller's values.
  if [ ! -s "$target" ] || grep -qx 'null' "$target"; then
    echo "the \`$key\` subtree of $file is empty or absent, so the row would render chart defaults while claiming to render this caller's values" >&2
    return 1
  fi
  printf '%s' "$target"
}

# The TOP-LEVEL KEYS of one chart's subtree, one per line. Same arguments as
# values_subtree_file.
#
# Read through helm rather than off the file that function writes, because that
# file is JSON on a single line -- a `^key:` scan of it silently finds nothing,
# which is how the unknown-key check went inert the first time this was written.
# The template emits a YAML LIST rather than repeated `KEY: x` lines: helm parses
# its own output, and repeated mapping keys are not a shape it is safe to rely
# on. An empty subtree yields an empty list, which is a legitimate answer --
# accretion-db and otel-collector take chart defaults.
values_subtree_keys() {
  local key="$1" file="$2" d="$3"
  local chart="$d/subtreekeys-$key" out
  if [ ! -d "$chart" ]; then
    mkdir -p "$chart/templates"
    printf 'apiVersion: v2\nname: subtreekeyreader\nversion: 0.0.0\n' > "$chart/Chart.yaml"
    # shellcheck disable=SC2016  # $k/$v are Go template variables, not shell
    printf '%s\n' 'SUBTREEKEYS:
{{- range $k, $v := (index .Values "'"$key"'") }}
  - {{ $k }}
{{- end }}' > "$chart/templates/dump.yaml"
  fi
  out=$(helm template subtreekeyreader "$chart" -f "$file" 2>&1) || {
    echo "could not list the keys of the \`$key\` subtree of $file: $out" >&2
    return 1
  }
  printf '%s\n' "$out" | sed -n 's/^  - //p'
}

# Render one row. The helm exit status is checked BEFORE anything is counted:
# a failed render prints no objects, and "no objects" must never be read as
# "nothing wrong here" (#106 was exactly that bug in render-check).
render_row() {
  local caller="$1" chart="$2" chartdir="$3" minobjects="$4" args="$5" d="$6"
  local label="$caller/$chart" out n
  out="$d/render-$caller-$chart"

  # Deliberately unquoted: $args is the row's flag list. Every value in the
  # matrix is space-free for exactly this reason.
  # shellcheck disable=SC2086
  if ! helm template t "$chartdir" $args > "$out.yaml" 2> "$out.err"; then
    fail "$label: helm template failed"
    sed 's/^/      /' "$out.err" >&2
    return 1
  fi
  n=$(grep -c '^kind:' "$out.yaml" || true)
  if [ "$n" -lt "$minobjects" ]; then
    fail "$label: rendered $n objects, fewer than the $minobjects this row must produce -- the render is vacuous, not clean"
    return 1
  fi
  rendered=$((rendered + 1))
  printf '  ok  %-34s %s objects\n' "$label" "$n"
}

# ---------------------------------------------------------------------------
# Drift: does the transcribed row still describe its caller?
#
# The Tilt value set is built in a language this gate cannot execute, so the
# matrix transcribes what remains of it -- and a transcription that has quietly
# fallen behind its caller renders green while the caller itself breaks, which
# is the precise failure mode this repository guards against hardest. The
# transcribed row names its source file, and the value KEYS the source mentions
# are compared against the keys the row supplies. Values are not compared; keys
# are what catch a rename, an addition or a deletion.
#
# Since #119 most of that set is not transcribed at all: the Tiltfile passes
# deploy/tilt.yaml subtrees and the rows name the same subtrees, so those cannot
# drift. What is compared is the reference itself -- a chart whose subtree the
# Tiltfile passes and the matrix does not, or the reverse -- plus the handful of
# flags the Tiltfile still computes.
# ---------------------------------------------------------------------------

# Value-path tokens in the Tiltfile: the text between a single quote and the
# first `=`, which is the shape every helm flag takes there.
#
# Scoped to the two places a helm flag can appear, because the Tiltfile is full
# of other shell text that also contains `NAME=value`: a line carrying its own
# `--set` flag, or one of the `*_set` flag lists the built-in helm() call takes
# as `set=`. Without that scoping `CGO_ENABLED=0` out of a go build command and
# `n=0` out of a port-forward retry script both read as helm values the matrix
# had failed to cover.
#
# Backslashes are dropped before comparison: `kubernetes\.io` in the source is a
# Starlark escape for a dot inside a key name, and the matrix row spells the same
# flag with the escaping helm itself needs. Only the key is being compared, so
# normalising both sides is exact rather than lossy.
#
# Third-party charts are excluded by name: cert-manager and metrics-server are
# installed from upstream repos, are not part of this matrix, and their flags
# would otherwise read as tokens the matrix has failed to cover. A new
# third-party flag fails this check loudly, which is the right outcome -- add it
# here or add the chart to the matrix.
tiltfile_tokens() {
  awk '
    function count(line, re,   n) { n = 0; while (match(line, re)) { n++; line = substr(line, RSTART + RLENGTH) } return n }
    inset && /^[A-Za-z]/ && $0 !~ /_set/ { inset = 0 }
    /^[a-z_]+_set *\+?= *\[/             { inset = 1 }
    {
      # A values file the Tiltfile materialises out of the central file is
      # EXPRESSIBLE, and is emitted as a token of its own: the Tiltfile names
      # the chart literally in `chart_values[<chart>]`, and the matrix row names
      # the same chart in `@subtree-from:<file>:<chart>`. Emitting it is what
      # keeps a chart added on one side and not the other from passing.
      if ($0 ~ /chart_values\[/) {
        line = $0
        while (match(line, /chart_values\['"'"'[a-z0-9-]+'"'"'\]/)) {
          tok = substr(line, RSTART, RLENGTH)
          line = substr(line, RSTART + RLENGTH)
          sub(/^chart_values\['"'"'/, "", tok)
          sub(/'"'"'\]$/, "", tok)
          printf "values:%s=\n", tok
        }
        # A --values flag on this line is now accounted for. Anything else on it
        # still falls through to the --set handling below.
      }
      # Any OTHER values file is a form the matrix has no way to express as a
      # token, so it is refused rather than skipped -- otherwise a caller that
      # moved its settings into a file would extract zero flags and read as
      # agreeing.
      else if ($0 ~ /'"'"'(--values|-f)'"'"'/) { printf "UNRECOGNISED %s\n", $0; next }
      if (!inset && $0 !~ /--set/ && $0 !~ /_set\.append\(/) next

      # Vacuity invariant, and the thing that makes "unrecognised is an error"
      # load-bearing rather than decorative. A --set flag and its value sit on
      # one line here, so the number of flags on a line must equal the number of
      # quoted values carrying an `=`. A flag whose value this extractor cannot
      # see -- built from a variable, spread across lines, or passed as -f --
      # makes the counts disagree, and the run FAILS instead of quietly
      # extracting one fewer token and comparing two sets that both lack it.
      # Regexes are passed as STRINGS, not /literals/: awk evaluates a regex
      # literal in an argument position as a match against $0 and passes the
      # resulting 0 or 1, which silently turns the count into a search for the
      # digit 1.
      nflags  = count($0, "'"'"'--set(-string|-json)?'"'"'")
      # The value must be the quoted literal immediately after the flag. A
      # looser "any quoted string containing =" counts Starlark keyword
      # arguments too, because the closing quote of one string and the
      # opening quote of the next bracket things like `chart=`.
      nvalues = count($0, "'"'"'--set(-string|-json)?'"'"', *'"'"'[^'"'"']*=")
      if (nflags > 0 && nflags != nvalues) {
        printf "UNRECOGNISED %s\n", $0
        next
      }

      line = $0
      while (match(line, /'"'"'\.?[A-Za-z_][A-Za-z0-9_.\\\/-]*=/)) {
        tok = substr(line, RSTART + 1, RLENGTH - 1)
        line = substr(line, RSTART + RLENGTH)
        if (tok == "installCRDs=") continue
        gsub(/\\/, "", tok)
        print tok
      }
    }
  ' "$1" | sort -u
}

# The same tokens, from the matrix rows for one caller. `--set-json` values are
# JSON, so only the path before the first `=` is a token; a `@subtree-from:`
# reference yields the `values:<chart>=` token the extractor above emits for the
# Tiltfile's own `chart_values[<chart>]`.
row_tokens() {
  local caller="$1"
  matrix_rows | awk -F'|' -v c="$caller" '$1 == c { print $5 }' |
    tr ' ' '\n' |
    sed -n -e 's/^@subtree-from:[^:]*:\(.*\)$/values:\1=/p' \
           -e 's/^\([A-Za-z_][A-Za-z0-9_.\\/-]*\)=.*/\1=/p' |
    sed 's/\\//g' |
    sort -u
}

# Compare two sorted key/token lists and report both directions by name.
compare_sets() {
  local label="$1" source_desc="$2" matrix_desc="$3" a="$4" b="$5" only_a only_b
  only_a=$(comm -23 "$a" "$b" | tr '\n' ' ')
  only_b=$(comm -13 "$a" "$b" | tr '\n' ' ')
  if [ -n "${only_a// /}" ]; then
    fail "$label: $source_desc, but the matrix does not: ${only_a% }. The transcribed row has fallen behind its caller, so this gate is rendering a value set nobody deploys."
    return 1
  fi
  if [ -n "${only_b// /}" ]; then
    fail "$label: the matrix supplies keys $source_desc no longer has: ${only_b% }. Drop them from the row, or the gate is testing a caller that no longer exists."
    return 1
  fi
  printf '  ok  %-34s %s\n' "$label" "$matrix_desc"
}

# Both extractors emit `UNRECOGNISED <line>` for anything in a caller source
# they cannot read, and this turns that into a hard failure. It is the assertion
# that keeps the whole transcription honest: an extractor that silently skipped
# what it did not understand would hand the drift check an incomplete picture of
# the caller, and the two sets would then agree only because both were missing
# the same thing -- a gate passing for the wrong reason, which is precisely what
# this file exists to prevent.
refuse_unrecognised() {
  local label="$1" source_desc="$2" file="$3"
  grep -q '^UNRECOGNISED ' "$file" || return 0
  fail "$label: $source_desc in a form this gate cannot read. It is refusing to compare rather than compare an incomplete picture of the caller — teach the extractor this form, or express it in the matrix:"
  sed -n 's/^UNRECOGNISED /      /p' "$file" >&2
  return 1
}

# Every top-level value key a chart could possibly consume: the ones its
# values.yaml declares, plus the ones its templates reference without a default.
# Both halves are needed -- `postgresql` has no entry in the API chart's
# values.yaml but _validation.tpl reads .Values.postgresql.enabled, so
# values.yaml alone would reject a key the chart genuinely uses.
chart_known_keys() {
  local simple all
  # The key pattern assumes no top-level key is quoted or contains a dot. That
  # holds across all five first-party charts today -- checked -- but it is an
  # ASSUMPTION, and the whole point of this check is catching a key a chart no
  # longer declares. A filter that can quietly miss keys is the wrong shape for
  # that job: a missed key reads as "the caller sets something undeclared"
  # (false positive) or, worse, widens the known set and produces a false clean.
  # So the assumption is asserted, not trusted: the count the pattern finds must
  # equal the count of top-level keys by any spelling.
  simple=$(helm show values "$1" 2>/dev/null | grep -cE '^[a-zA-Z][A-Za-z0-9_]*:' || true)
  all=$(helm show values "$1" 2>/dev/null | grep -cE '^[^[:space:]#-].*:' || true)
  if [ "$simple" != "$all" ]; then
    printf 'UNREADABLE %s declares %s top-level keys but only %s match the key pattern — one is quoted or dotted, so this check can no longer see the whole declared surface\n' "$1" "$all" "$simple"
    return 0
  fi
  {
    helm show values "$1" 2>/dev/null | grep -oE '^[a-zA-Z][A-Za-z0-9_]*:' | sed 's/:$//'
    grep -rhoE '\.Values\.[a-zA-Z][A-Za-z0-9_]*' "$1/templates" 2>/dev/null | sed 's/^\.Values\.//'
  } | sort -u
}

# Helm accepts and silently ignores a value key no chart reads, which is the
# whole reason #117 exists: a renamed key lands as a no-op rather than an error,
# and helm.go documents one instance in place (logWorker/metricWorker survived
# the collectors being consolidated, so the image tag and autoscaling settings
# reached nothing).
#
# The drift check above compares each caller against the MATRIX, which cannot
# see this: both sides agree on a key that neither chart has. Cycle-1 review
# found it live -- #122 deleted the chart's mcpSecret template and values block,
# the Tiltfile still passed mcpSecret.enabled and mcpSecret.url, and
# "32 value flags agree with the Tiltfile" stayed green over two dead flags.
#
# BOTH sources of a caller's keys are inspected: the --set paths in the row, and
# the top-level keys of any subtree the row names with `@subtree-from:`. Reading
# only the flags would have quietly retired this check the moment a caller moved
# its settings into a file -- which is exactly what #119 did to the Tiltfile, and
# the mcpSecret flags are what that would have stopped catching.
#
# Top-level keys only, deliberately. That is where a whole feature going away
# shows up, it needs no YAML walker, and the depth this gate can afford to
# police is bounded by having helm and awk and nothing else. A renamed LEAF is
# not caught here: `config.database.host` on the accretion-discord chart is dead
# -- that chart has no config.database at all -- and passes this check, because
# `config` exists. Full-depth comparison for this caller is a follow-up.
unknown_key_check() {
  local d="$1" caller chart src minobjects args chartdir path top rows unknown=0
  local ref file key subtree
  rows=$(matrix_rows)
  while IFS='|' read -r caller chart src minobjects args; do
    [ -n "$caller" ] || continue
    chartdir="./charts/$chart"
    [ -d "$chartdir" ] || continue
    if [ ! -f "$d/known-$chart" ]; then
      chart_known_keys "$chartdir" > "$d/known-$chart"
      if grep -q '^UNREADABLE ' "$d/known-$chart"; then
        fail "$caller/$chart: $(sed -n 's/^UNREADABLE //p' "$d/known-$chart")"
        unknown=$((unknown + 1))
        : > "$d/known-$chart"
        continue
      fi
      # Comparing against an empty set would clear every caller silently, which
      # is the same vacuity this file refuses everywhere else.
      if [ ! -s "$d/known-$chart" ]; then
        fail "$caller/$chart: no value keys could be read from the chart at all, so every caller key would compare as unknown — refusing rather than reporting a set of findings that means nothing."
        unknown=$((unknown + 1))
        continue
      fi
    fi
    [ -s "$d/known-$chart" ] || continue
    for path in $(printf '%s' "$args" | tr ' ' '\n' | sed -n 's/^\([A-Za-z_][A-Za-z0-9_.\\/-]*\)=.*/\1/p'); do
      top=${path%%.*}
      grep -qx "$top" "$d/known-$chart" && continue
      fail "$caller/$chart: sets \`$path\`, but $chart declares no \`$top\` value and no template reads one. Helm ignores it silently, so this caller believes it is configuring something and is configuring nothing."
      unknown=$((unknown + 1))
    done
    # The same question, asked of the values FILE a row names rather than of its
    # flags. `{}` is a legitimate answer -- accretion-db and otel-collector take
    # chart defaults -- so an empty key list is not a finding here; a subtree
    # that could not be materialised at all already failed above.
    case "$args" in
      *@subtree-from:*)
        ref=${args#*@subtree-from:}
        ref=${ref%% *}
        file=${ref%%:*}
        key=${ref##*:}
        subtree=$(values_subtree_keys "$key" "$file" "$d") || {
          fail "$caller/$chart: could not list the keys of the \`$key\` subtree of $file, so they cannot be checked against the chart"
          unknown=$((unknown + 1))
          continue
        }
        for top in $subtree; do
          grep -qx "$top" "$d/known-$chart" && continue
          fail "$caller/$chart: $file declares \`$key.$top\`, but $chart declares no \`$top\` value and no template reads one. Helm ignores it silently, so this caller believes it is configuring something and is configuring nothing."
          unknown=$((unknown + 1))
        done
        ;;
    esac
  done <<EOF
$rows
EOF
  [ "$unknown" -eq 0 ]
}

drift_check() {
  local d="$1"
  tiltfile_tokens Tiltfile > "$d/tilt-source"
  row_tokens tilt > "$d/tilt-matrix"
  [ -s "$d/tilt-source" ] || { fail "drift/tilt: no helm value flags or values files found in the Tiltfile -- the drift check is vacuous"; return 1; }
  refuse_unrecognised "drift/tilt" "the Tiltfile passes a helm value" "$d/tilt-source" || return 1
  compare_sets "drift/tilt" "the Tiltfile passes these helm values" \
    "$(wc -l < "$d/tilt-source" | tr -d ' ') values agree with the Tiltfile" \
    "$d/tilt-source" "$d/tilt-matrix" || return 1
}

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------

run_matrix() {
  local d caller chart src minobjects args chartdir resolved file ref key
  d=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$d'" EXIT

  printf '%b\n' "${BLUE}Checking every caller's value set renders...${NC}"
  drift_check "$d" || true
  if unknown_key_check "$d"; then
    printf '  ok  %-34s every value key reaches something\n' "unknown-keys"
  fi

  local rows
  rows=$(matrix_rows)
  [ -n "$rows" ] || { fail "the matrix is empty -- this check is vacuous"; return 1; }

  while IFS='|' read -r caller chart src minobjects args; do
    [ -n "$caller" ] || continue
    chartdir="./charts/$chart"
    [ -d "$chartdir" ] || { fail "$caller/$chart: $chartdir does not exist"; continue; }
    # A transcribed row names the caller file it was transcribed from. If that
    # file has moved or gone, the row is describing a caller that no longer
    # exists and the drift check above has nothing to compare against.
    [ "$src" = "-" ] || [ -f "$src" ] || { fail "$caller/$chart: its drift source $src does not exist, so this row can no longer be checked against its caller"; continue; }

    # Resolve the one indirection the matrix uses: a chart's subtree of a central
    # values file, materialised the way the caller materialises it rather than
    # transcribed (see TILT_VALUES).
    case "$args" in
      *@subtree-from:*)
        ref=${args#*@subtree-from:}
        ref=${ref%% *}
        file=${ref%%:*}
        key=${ref##*:}
        [ -f "$file" ] || { fail "$caller/$chart: $file does not exist, so this row cannot render the values its caller passes"; continue; }
        resolved=$(values_subtree_file "$key" "$file" "$d") || { fail "$caller/$chart: could not materialise the \`$key\` subtree of $file"; continue; }
        args=${args//@subtree-from:$ref/-f $resolved}
        ;;
    esac

    render_row "$caller" "$chart" "$chartdir" "$minobjects" "$args" "$d" || true
  done <<EOF
$rows
EOF

  [ "$rendered" -gt 0 ] || fail "no row rendered anything -- this check is vacuous"

  if [ "$failures" -gt 0 ]; then
    printf '%b\n' "${RED}$failures problem(s) across the callers' value sets — a set that does not render, or one that sets a value nothing reads. Every one is a deployment path that is wrong right now.${NC}" >&2
    return 1
  fi
  printf '%b\n' "${GREEN}Every caller's value set renders ($rendered rows)${NC}"
}

# ---------------------------------------------------------------------------
# Selfcheck: prove the engine above has teeth, against hand-built fixtures so
# the result never depends on which of this series' fixes have landed.
# ---------------------------------------------------------------------------

selfcheck() {
  local d chart
  d=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$d'" EXIT
  chart="$d/charts/fixture"
  mkdir -p "$chart/templates"
  printf 'apiVersion: v2\nname: fixture\nversion: 0.0.0\n' > "$chart/Chart.yaml"
  printf 'guarded: false\nemitObject: true\n' > "$chart/values.yaml"
  cat > "$chart/templates/all.yaml" <<'TPL'
{{- if .Values.guarded }}
{{- fail "FIXTURE-GUARD: this value set is refused by the chart" }}
{{- end }}
{{- if .Values.emitObject }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: fixture
{{- end }}
TPL

  printf '%b\n' "${BLUE}Proving the values-matrix detector fires...${NC}"

  # Every case below calls the engine in THIS shell, not a command
  # substitution: the pass/fail verdict lives in the `failures` counter, and a
  # subshell would discard exactly the thing under test.
  local out

  # 1. A clean value set passes.
  failures=0; rendered=0
  render_row selfcheck clean "$chart" 1 "--set guarded=false" "$d" >/dev/null
  [ "$failures" -eq 0 ] && [ "$rendered" -eq 1 ] || {
    printf '%b\n' "${RED}selfcheck FAILED: a clean value set was rejected -- the engine is not usable on real content${NC}" >&2
    return 1
  }
  printf '  ok  selfcheck/clean: a value set the chart accepts passes\n'

  # 2. A value set the chart refuses is caught, and helm's own message reaches
  #    the operator rather than being swallowed.
  failures=0; rendered=0
  render_row selfcheck guarded "$chart" 1 "--set guarded=true" "$d" >"$d/out" 2>&1 || true
  out=$(cat "$d/out")
  [ "$failures" -eq 1 ] || {
    printf '%b\n' "${RED}selfcheck FAILED: a value set the chart refuses was NOT caught -- the render-exit assertion is inert${NC}" >&2
    return 1
  }
  case "$out" in
    *"FIXTURE-GUARD"*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: the refused value set was caught, but the chart's own reason was swallowed: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/guarded: a refused value set is rejected, carrying the chart'"'"'s own message\n'

  # 3. A render that succeeds and emits nothing is vacuous, not clean. This is
  #    the #106 bug: helm exits 0, prints no objects, and the count reads as a
  #    pass. A row that legitimately renders nothing declares min-objects 0.
  failures=0; rendered=0
  render_row selfcheck empty "$chart" 1 "--set emitObject=false" "$d" >"$d/out" 2>&1 || true
  out=$(cat "$d/out")
  [ "$failures" -eq 1 ] || {
    printf '%b\n' "${RED}selfcheck FAILED: a render that emitted nothing passed -- the vacuity assertion is inert${NC}" >&2
    return 1
  }
  case "$out" in
    *vacuous*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: an empty render was rejected, but not for being vacuous: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/empty: a render that emits nothing is rejected as vacuous, not counted as clean\n'

  # 4. Drift. A transcription that has fallen behind its caller renders green
  #    while the caller breaks, so the comparison itself has to be proven.
  printf "%s\n" "aaa=" "bbb=" > "$d/source"
  printf "%s\n" "aaa=" > "$d/behind"
  printf "%s\n" "aaa=" "bbb=" "ccc=" > "$d/ahead"
  printf "%s\n" "aaa=" "bbb=" > "$d/agrees"

  failures=0
  compare_sets selfcheck/drift-behind "the fixture caller declares these" "n/a" "$d/source" "$d/behind" >"$d/out" 2>&1 || true
  out=$(cat "$d/out")
  [ "$failures" -eq 1 ] || { printf '%b\n' "${RED}selfcheck FAILED: a matrix missing a key its caller declares was not rejected${NC}" >&2; return 1; }
  case "$out" in
    *bbb=*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: the missing key was not named in the message: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/drift-behind: a key the caller added and the matrix lacks is rejected by name\n'

  failures=0
  compare_sets selfcheck/drift-ahead "the fixture caller declares these" "n/a" "$d/source" "$d/ahead" >"$d/out" 2>&1 || true
  out=$(cat "$d/out")
  [ "$failures" -eq 1 ] || { printf '%b\n' "${RED}selfcheck FAILED: a matrix supplying a key its caller dropped was not rejected${NC}" >&2; return 1; }
  case "$out" in
    *ccc=*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: the stale key was not named in the message: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/drift-ahead: a key the caller dropped and the matrix keeps is rejected by name\n'

  failures=0
  compare_sets selfcheck/drift-agrees "the fixture caller declares these" "in agreement" "$d/source" "$d/agrees" >/dev/null
  [ "$failures" -eq 0 ] || { printf '%b\n' "${RED}selfcheck FAILED: two identical key sets were reported as drift -- the comparison is over-broad${NC}" >&2; return 1; }
  printf '  ok  selfcheck/drift-agrees: two agreeing key sets pass\n'

  # 5. The extractors themselves. An extractor that silently returns nothing
  #    turns the drift check above into a comparison of two empty sets, which
  #    passes for the wrong reason.
  cat > "$d/fixture.Tiltfile" <<'TF'
helm_resource(
    'x',
    chart='./charts/fixture',
    flags=[
        '--set', 'alpha.beta=1',
        '--set-json', 'gamma=[]',
    ],
)
local_resource('y', cmd='kubectl get pods --dry-run=client -o yaml')
helm_resource('z', chart='jetstack/cert-manager', flags=['--set', 'installCRDs=true'])
TF
  out=$(tiltfile_tokens "$d/fixture.Tiltfile" | tr '\n' ' ')
  [ "$out" = "alpha.beta= gamma= " ] || {
    printf '%b\n' "${RED}selfcheck FAILED: the Tiltfile extractor returned [$out], expected [alpha.beta= gamma= ] -- it must find helm flags, skip kubectl command strings, and skip third-party charts${NC}" >&2
    return 1
  }
  printf '  ok  selfcheck/extract-tilt: helm flags found, a kubectl command string and a third-party flag skipped\n'

  # The load-bearing half: a caller form the extractor cannot read must FAIL the
  # run, never quietly yield one fewer token. Two shapes that would otherwise be
  # invisible -- a flag value built from a variable, and a values FILE, which the
  # matrix has no way to express as a token.
  cat > "$d/unknown.Tiltfile" <<'TF'
helm_resource(
    'x',
    chart='./charts/fixture',
    flags=[
        '--set', 'alpha.beta=1',
        '--set', some_value_built_elsewhere,
        '--values', 'charts/fixture/values-dev.yaml',
    ],
)
TF
  tiltfile_tokens "$d/unknown.Tiltfile" > "$d/unknown.tokens"
  failures=0
  refuse_unrecognised selfcheck/unknown-tilt "the fixture Tiltfile passes a helm value" "$d/unknown.tokens" >"$d/out" 2>&1 || true
  out=$(cat "$d/out")
  [ "$failures" -eq 1 ] || {
    printf '%b\n' "${RED}selfcheck FAILED: a Tiltfile flag whose value the extractor cannot read did NOT fail the run. It would have contributed no token, and the drift check would then compare two sets that agree only because both are missing it.${NC}" >&2
    return 1
  }
  case "$out" in
    *some_value_built_elsewhere*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: the unreadable flag was refused, but the offending line was not shown: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/unknown-tilt: a flag value the extractor cannot read fails the run and names the line\n'

  # 6. The central-values machinery (#119). A caller that moved its settings out
  #    of flags and into a file must still be compared against the matrix, or
  #    the move silently retires the drift check.
  cat > "$d/values.Tiltfile" <<'TF'
helm_resource(
    'x',
    chart='./charts/fixture',
    flags=[
        '--values', chart_values['fixture-one'],
        '--set', 'alpha.beta=1',
    ],
)
k8s_yaml(helm('./charts/two', values=[chart_values['fixture-two']], set=[]))
TF
  out=$(tiltfile_tokens "$d/values.Tiltfile" | tr '\n' ' ')
  [ "$out" = "alpha.beta= values:fixture-one= values:fixture-two= " ] || {
    printf '%b\n' "${RED}selfcheck FAILED: the Tiltfile extractor returned [$out], expected [alpha.beta= values:fixture-one= values:fixture-two= ] -- a chart whose subtree the Tiltfile passes must yield a token, in both the helm_resource and the built-in helm() form, or a chart added on one side and not the other would compare as agreeing${NC}" >&2
    return 1
  }
  printf '  ok  selfcheck/extract-values: a central-file subtree the Tiltfile passes yields a token in both call forms\n'

  # The reader must return the subtree it was ASKED for, not whichever one it
  # read first: its template names the key, so a cached reader chart would
  # answer every later call with the first key's contents.
  printf 'chart-a:\n  alpha: 1\nchart-b:\n  beta: 2\n' > "$d/central.yaml"
  out=$(values_subtree_file chart-a "$d/central.yaml" "$d") && out=$(cat "$out")
  [ "$out" = '{"alpha":1}' ] || {
    printf '%b\n' "${RED}selfcheck FAILED: the subtree reader returned [$out] for chart-a, expected [{\"alpha\":1}]${NC}" >&2
    return 1
  }
  out=$(values_subtree_file chart-b "$d/central.yaml" "$d") && out=$(cat "$out")
  [ "$out" = '{"beta":2}' ] || {
    printf '%b\n' "${RED}selfcheck FAILED: the subtree reader returned [$out] for chart-b, expected [{\"beta\":2}] -- it is answering with an earlier key's subtree${NC}" >&2
    return 1
  }
  printf '  ok  selfcheck/subtree-reads: each chart'"'"'s subtree is read back, and a second key is not served the first one\n'

  # An absent subtree yields `null`, which helm accepts as a values file and
  # merges as nothing -- so the row would render chart defaults while claiming
  # to render its caller's values. That must be refused, not merged.
  if values_subtree_file chart-missing "$d/central.yaml" "$d" >"$d/out" 2>&1; then
    printf '%b\n' "${RED}selfcheck FAILED: a subtree the central file does not declare was accepted. helm merges it as nothing, so the row would render chart defaults and report them as the caller's values.${NC}" >&2
    return 1
  fi
  printf '  ok  selfcheck/subtree-absent: a subtree the central file does not declare is refused, not merged as nothing\n'

  printf '%b\n' "${GREEN}The values-matrix detector fires: refused, empty and drifted value sets are all rejected by name, and a caller's values file is read rather than assumed${NC}"
}

# --print-subtree <central-file> <chart>: one chart's subtree from the central
# values file, as JSON on stdout, ready to hand to `helm -f -`.
#
# It exists so that OTHER gates can read a subtree without growing a second copy
# of values_subtree_file. #135's secrets-keys-check grew exactly that copy --
# the same synthetic chart, the same toJson template, the same sed -- and lost
# what this one had learned (the per-key cache, the `local` expansion gotcha at
# the top of that function) in the process. Two implementations of "read one
# chart's subtree out of deploy/tilt.yaml using helm as the parser" is the drift
# these gates exist to prevent, and the copy lived in a Makefile recipe, the
# harder of the two to exercise.
#
# The selfcheck above covers this path already: it drives values_subtree_file
# directly, including the absent-subtree refusal, so this mode inherits a tested
# reader rather than needing its own proof.
print_subtree() {
  local file="$1" chart="$2" d out
  if [ -z "$file" ] || [ -z "$chart" ]; then
    echo "usage: $0 --print-subtree <central-values-file> <chart>" >&2
    return 2
  fi
  d=$(mktemp -d)
  # shellcheck disable=SC2064  # $d is expanded now, deliberately
  trap "rm -rf '$d'" EXIT
  out=$(values_subtree_file "$chart" "$file" "$d") || return 1
  cat "$out"
}

case "${1:-}" in
  --selfcheck)     selfcheck ;;
  --print-subtree) print_subtree "${2:-}" "${3:-}" ;;
  "")              run_matrix ;;
  *) echo "usage: $0 [--selfcheck | --print-subtree <central-values-file> <chart>]" >&2; exit 2 ;;
esac

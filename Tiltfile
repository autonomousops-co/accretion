# -*- mode: Python -*-
# Tiltfile for Accretion local development with kind

# Load Tilt extensions


load('ext://helm_resource', 'helm_resource')
load('ext://restart_process', 'docker_build_with_restart')

# Give Helm enough time for PostgreSQL to start + schema job to complete
update_settings(k8s_upsert_timeout_secs=300)

# Parse .env file
def read_env():
    env_vars = {}
    if os.path.exists(".env"):
        env_content = str(read_file(".env"))
        for line in env_content.splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if "=" in line:
                key, value = line.split("=", 1)
                env_vars[key.strip()] = value.strip().strip("'").strip('"')
    return env_vars

env_vars = read_env()

anthropic_api_key = os.getenv("ACCRETION_LLM_ANTHROPIC_API_KEY") or env_vars.get("ACCRETION_LLM_ANTHROPIC_API_KEY", "")
discord_bot_token = os.getenv("DISCORD_BOT_TOKEN") or env_vars.get("DISCORD_BOT_TOKEN", "")
discord_guild_id = os.getenv("DISCORD_GUILD_ID") or env_vars.get("DISCORD_GUILD_ID", "")

# Settings
settings = {
    'namespace': 'accretion-core',
    'ns_monitors': ['default'],
    'enable_collector': True,
    'tilt_values': 'deploy/tilt.yaml',
}

# Declare allowed settings. They come from an optional, gitignored
# tilt_config.json or from the command line (`tilt up -- --enable_collector=false`,
# which is what make tilt-server-only does). The file used to be tracked and
# rewritten by make, so one `make tilt-server-only` left every later `make tilt-up`
# server-only until someone reverted it by hand (#101). The defaults above are
# what a fresh checkout runs with.
config.define_string('namespace')
config.define_bool('enable_collector')

config.define_string_list('ns_monitors')

# Which central values file to read.
#
# THIS SETTING EXISTS FOR TESTABILITY, and that is a real cost: one more knob in
# a supported interface, settable to something nobody intended, and needed by
# nothing the stack does. It is worth paying because the alternative is worse.
# The guards below refuse a mis-keyed central file, and the only way to WATCH
# them refuse is to hand the load a broken copy — which is what
# scripts/check-tiltfile.sh does, through here. A Tiltfile resolves relative
# paths against its own directory, so a fixture Tiltfile anywhere but the
# repository root would not see charts/ or deploy/; driving the perturbation
# through the values file is what avoids needing one at all. Without this knob
# those guards could never be proven to fire, and a guard nobody has watched
# fail is a comment.
#
# It is a usable override as well -- an operator can run the stack from an
# alternative central file without editing a tracked one -- but that is a side
# effect, not the reason it is here.
config.define_string('tilt_values')

cfg = config.parse()
namespace = cfg.get('namespace', settings['namespace'])
enable_collector = cfg.get('enable_collector', settings['enable_collector'])
ns_monitors = cfg.get('ns_monitors', settings['ns_monitors'])
tilt_values_file = cfg.get('tilt_values', settings['tilt_values'])

# ---------------------------------------------------------------------------
# Central values (#119)
#
# Every static setting these charts need lives in deploy/tilt.yaml, nested by
# chart name, instead of being rebuilt here as inline --set flags. Sixteen for
# accretion-api, twelve for accretion-collector and nine for accretion-discord
# used to be Starlark string concatenation, which no gate could read: two of
# them were configuring a chart block #122 had deleted, and a third was setting
# a database host for a bot that has no database connection. None of the three
# was detectable from here.
#
# Each chart's subtree is written out at LOAD time and passed to that chart's
# release with --values. The split files are ephemeral and never committed.
# ---------------------------------------------------------------------------

# Where the split files go, and why there rather than a directory of their own.
# build/ already carries two of the three properties these files need, and the
# third comes from somewhere it is worth naming exactly:
#
#   gitignored          .gitignore line 2. The split files are ephemeral and
#                       must never be committed.
#   not watched         .tiltignore lists build/, so files rewritten on every
#                       load cannot trigger a reload loop.
#   not in the image    NOT from .dockerignore, which says nothing about
#                       build/ -- its `*.yaml` line matches root-level files
#                       only, not build/tilt-values/*.yaml. Two other things
#                       carry it: the docker_build call below names 'build/' in
#                       its own `ignore` list, which is what keeps writing these
#                       files from triggering an image rebuild; and the
#                       Dockerfile COPYs explicit paths (go.mod, vendor, cmd,
#                       internal, web, prompts) rather than the whole context,
#                       so nothing under build/ can reach an image whatever any
#                       ignore file says.
#
# The distinction is not pedantry. A reader who believes .dockerignore covers
# this directory would go looking for a rule that is not there, and would be
# wrong about the protection the day anything secret is written here -- which is
# a plausible extension of this very materialiser, since the values it splits
# are one policy change away from carrying a digest.
tilt_values_dir = 'build/tilt-values'

central_values = read_yaml(tilt_values_file)

def materialise_chart_values(chart):
    """Write one chart's subtree of the central file and return its path."""
    subtree = central_values.get(chart)
    # A key present but empty parses as None, which would silently deploy chart
    # defaults while looking configured. `{}` says "defaults, deliberately".
    if type(subtree) != 'dict':
        fail('%s: `%s` must be a mapping (use `{}` for chart defaults), got %s' %
             (tilt_values_file, chart, type(subtree)))
    path = '%s/%s.yaml' % (tilt_values_dir, chart)
    local('mkdir -p %s && cat > %s' % (tilt_values_dir, path),
          stdin=encode_yaml(subtree), quiet=True, echo_off=True)
    return path

# THE GUARD. The set of top-level keys must equal the set of charts deployed
# from this repository, in both directions.
#
# Helm ignores a value key no chart reads, so a typo'd or renamed chart name
# here would hand a release an empty map and say nothing — the release would
# come up on chart defaults, which for accretion-api means no ingress, no TLS
# and a fail-closed empty identity list. Compared as sorted lists so a key
# naming no chart and a chart with no key are BOTH named, rather than the check
# passing on a count.
#
# cert-manager and metrics-server are absent by design: they are installed from
# upstream chart repositories, not from charts/, and take no values from here.
tilt_charts = [
    'accretion-api',
    'accretion-collector',
    'accretion-db',
    'accretion-discord',
    'otel-collector',
]

declared_charts = sorted(central_values.keys())
if declared_charts != sorted(tilt_charts):
    fail(('%s declares top-level keys %s, but Tilt deploys charts %s. ' +
          'Every chart needs a key and every key needs a chart: helm ignores a ' +
          'value key no chart reads, so a mismatch here configures nothing and ' +
          'reports nothing.') % (tilt_values_file, declared_charts, sorted(tilt_charts)))

# A chart named here must also EXIST. Without this the guard above only proves
# the two lists agree with each other, which they would continue to do after a
# chart was renamed in both places and in neither directory.
for chart in tilt_charts:
    if not os.path.exists('charts/%s/Chart.yaml' % chart):
        fail(('%s names chart `%s`, but charts/%s/Chart.yaml does not exist. ' +
              'Renaming a chart directory without following it here leaves a ' +
              'release configured by a subtree nothing reads.') % (tilt_values_file, chart, chart))

chart_values = {}
for chart in tilt_charts:
    chart_values[chart] = materialise_chart_values(chart)

# API-key identities, derived from .env rather than declared anywhere.
#
# Until #122 these were three checked-in plaintext `key` entries in
# charts/accretion-api/values-dev.yaml, which the Tiltfile read directly. That
# borrowed HALF a coupled pair: the plaintext came from that file but
# security.forbidPlaintextAPIKeys=false did not, so when the chart default
# flipped the Tilt stack stopped rendering. The plaintext now lives in .env and
# the list there is empty.
#
# scripts/make-secret.sh is the single mapping from .env to both halves: it
# emits the Secrets the clients read AND, here, the digests the server compares
# against. One input, so the two cannot drift — which is stronger than the gate
# that exists for the paths where they are two inputs.
#
# NOT written into deploy/tilt.yaml: a digest is credential material derived
# from a plaintext that lives only in your .env, and that file is checked in.
identities_yaml = local('bash scripts/make-secret.sh --print-identities',
                        quiet=True, echo_off=True)
dev_identities = decode_yaml(str(identities_yaml)).get('apiKeys') or []
if not dev_identities:
    # A warning, not a failure. A fresh clone holds no credential by design, and
    # `tilt alpha tiltfile-result` must still evaluate on one — that is what
    # `make tiltfile-check` runs, and a check that needs a developer's secrets
    # is a check that gets skipped. The accretion-api release then fails at helm
    # with the chart's own fail-closed message naming the empty list.
    warn('No API-key identities: .env sets none of ACCRETION_API_KEY_MCP, ' +
         'ACCRETION_API_KEY_DISCORD or ACCRETION_API_KEY_CI, so accretion-api ' +
         'has no caller it would authenticate and its install will refuse to ' +
         'render. Generate one per caller with `openssl rand -hex 32`, put them ' +
         'in .env, and see `scripts/make-secret.sh --print-identities`.')

# The database lives in its own dedicated namespace (separate from accretion's own
# components in `namespace`). Because it is NOT accretion's own namespace, the
# collectors can safely ingest its telemetry — no log-amplification loop — so it
# is added to the monitored set.
db_namespace = 'accretion-db'
if db_namespace not in ns_monitors:
    ns_monitors = ns_monitors + [db_namespace]

# Create namespaces
local_resource(
    'accretion-core.namespace',
    cmd='kubectl create namespace ' + namespace + ' --dry-run=client -o yaml | kubectl apply -f -',
    labels=['services'],
)

local_resource(
    'accretion-db.namespace',
    cmd='kubectl create namespace ' + db_namespace + ' --dry-run=client -o yaml | kubectl apply -f -',
    labels=['infra'],
)


# accretion-api-secrets, accretion-mcp-secrets, accretion-discord-secrets and
# accretion-db-secrets, via the one mapping from .env's SHELL_CASE names to the
# charts' dashed Secret keys (scripts/make-secret.sh). `--from-env-file=.env`
# used to be inlined here and produced keys the chart never reads, which showed
# up only as a 401 on every request. Runs before accretion-api, whose
# session-key / OAuth secretKeyRefs are not optional -- and before
# accretion-discord, whose own api-key secretKeyRef is not optional either.
#
# It also runs before accretion-db now (#134). That release used to need no
# secret at all because the chart carried `postgres` in its own values.yaml; it
# now reads the password from accretion-db-secrets, and the schema Job is a
# post-install hook, so the Secret has to exist in db_namespace before the
# release is installed rather than merely before the pods settle. TWO
# namespaces, because a Secret does not cross one: --namespace takes the app
# Secrets, --db-namespace takes the database's copy of the same password.
local_resource(
    'sync-secrets',
    cmd='bash scripts/make-secret.sh --namespace ' + namespace +
        ' --db-namespace ' + db_namespace,
    deps=['.env', 'scripts/make-secret.sh'],
    resource_deps=['accretion-core.namespace', 'accretion-db.namespace'],
    labels=['services'],
)

# Build binary locally for live update.
# TRIGGER_MODE_MANUAL: Tilt still watches the Go tree and marks this resource "pending"
# when code changes, but does NOT rebuild automatically — it (and the image
# live-update it feeds) only runs when triggered from the Tilt UI or
# `tilt trigger accretion-api-binary`. The initial build on `tilt up` still runs.
local_resource(
    'accretion-api-binary',
    cmd='CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ./build/accretion-api ./cmd/accretion-api',
    deps=['cmd', 'internal', 'go.mod', 'go.sum'],
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['build'],
)

# Build Docker image with live update for fast development
docker_build_with_restart(
    'accretion-api',
    context='.',
    dockerfile='Dockerfile',
    entrypoint=['/app/accretion-api'],
    # Live update: sync locally-built binary
    live_update=[
        sync('./build/accretion-api', '/app/accretion-api'),
        sync('./web', '/app/web'),
        sync('./prompts', '/app/prompts'),
    ],
    # Only rebuild image when Dockerfile or go.mod changes
    ignore=['charts/', 'pulumi/', 'scripts/', 'README.md', '.git/', 'build/'],
)

# Metrics Server (required for kubectl top / Kubernetes Metrics API)
# kind clusters don't ship with metrics-server; --kubelet-insecure-tls is
# needed because kubelet certs are self-signed in kind.
helm_resource(
    'k8s-metrics-server',
    chart='metrics-server/metrics-server',
    namespace='kube-system',
    flags=[
        '--set', 'args[0]=--kubelet-insecure-tls',
    ],
    labels=['infra'],
    pod_readiness='wait',
)

# PostgreSQL database — runs in its own `accretion-db` namespace (consumed cross-
# namespace by accretion-api and accretion-collector).
helm_resource(
    'accretion-db',
    chart='./charts/accretion-db',
    namespace=db_namespace,
    # Its subtree in deploy/tilt.yaml, which since #134 names the Secret the
    # chart reads the database password from. It was an empty map until then --
    # every chart Tilt deploys has a key there, so the guard that the keys equal
    # the charts needs no exception -- and this is the setting it gained.
    flags=['--values', chart_values['accretion-db']],
    # sync-secrets, because the chart no longer carries a password of its own
    # (#134): the Postgres superuser password and the schema Job's PGPASSWORD
    # both come from accretion-db-secrets, and the schema Job is a post-install
    # hook that runs as part of THIS install.
    resource_deps=['accretion-db.namespace', 'sync-secrets'],
    labels=['infra'],
    pod_readiness='wait',
)
k8s_resource(
    'accretion-db',
    extra_pod_selectors=[{'app.kubernetes.io/name': 'postgresql'}],
)

# cert-manager — issues the API listener's TLS certificate.
#
# Installed here rather than left as a documented prerequisite so that `make up`
# on a fresh clone produces working TLS with no manual certificate steps. TLS
# that has to be set up by hand is TLS that gets turned off.
#
# installCRDs is required: the chart's Certificate and Issuer objects are CRDs,
# and without them accretion-api's install fails on unknown kinds rather than on
# anything that names cert-manager.
# The chart repo is added here rather than assumed, so a fresh clone needs no
# `helm repo add` typed by hand. --force-update makes it idempotent.
local_resource(
    'cert-manager.repo',
    cmd='helm repo add jetstack https://charts.jetstack.io --force-update && helm repo update jetstack',
    labels=['infra'],
)

helm_resource(
    'cert-manager',
    chart='jetstack/cert-manager',
    namespace='cert-manager',
    flags=[
        '--create-namespace',
        '--set', 'installCRDs=true',
    ],
    resource_deps=['cert-manager.repo'],
    labels=['infra'],
    pod_readiness='wait',
)

# The CA the local certificate is signed by, written where every local client can
# find it. cert-manager creates this Secret asynchronously, so this retries
# rather than assuming it exists the moment the release is installed.
#
# This is what makes "no manual steps" true: `direnv`/.envrc exports
# ACCRETION_API_CA_FILE pointing here, so the MCP, the functional tests, curl and
# preflight all verify without anyone copying a certificate around.
local_resource(
    'api-ca-cert',
    cmd='kubectl wait --for=condition=ready certificate/accretion-api-tls -n ' + namespace + ' --timeout=120s && ' +
        'mkdir -p .tls && ' +
        'kubectl get secret accretion-api-tls -n ' + namespace +
        ' -o jsonpath="{.data.ca\\.crt}" | base64 -d > .tls/ca.crt && ' +
        'echo "CA written to .tls/ca.crt"',
    resource_deps=['accretion-api'],
    labels=['services'],
)

# Main accretion-api application
helm_resource(
    'accretion-api',
    chart='./charts/accretion-api',
    namespace=namespace,
    flags=[
        # Every static setting is in deploy/tilt.yaml under `accretion-api`.
        '--values', chart_values['accretion-api'],
        # Auth is ON locally, with hash-only identities derived from .env. A
        # wrong or under-scoped key gets a real 401/403 here rather than only in
        # a cluster. Passed as a flag rather than written into the central file
        # because a digest is credential material.
        '--set-json', 'apiKeys=' + encode_json(dev_identities),
    ],
    image_deps=['accretion-api'],
    image_keys=[('image.repository', 'image.tag')],
    resource_deps=['accretion-core.namespace', 'sync-secrets', 'db-init', 'cert-manager'],
    labels=['services'],
    # One forward per enabled listener. The API port is what accretion-mcp dials;
    # the UI port only exists when the UI is on, which it is not by default.
    port_forwards=['41337:41337'],
)

# Discord bot.
#
# Off by default in the chart since #94 (the MCP is the primary front end, and
# the bot needs a token the operator may not have), so the dev environment opts
# in explicitly -- and only when a token is actually present. Without a token
# the bot cannot connect, so deploying it would only add a CrashLoopBackOff to
# `tilt up`.
#
# The bot's credential FOR accretion-api is a second, separate key, and since
# #119 it comes from the accretion-discord-secrets Secret sync-secrets creates
# (api.secretName/secretKey in deploy/tilt.yaml). It used to come from the
# accretion-api chart's client-keys Secret, which that chart emits only for a
# PLAINTEXT identity -- so it stopped existing when #122 made every identity
# hash-only, and the bot's pod could not start at all.
if discord_bot_token:
    helm_resource(
        'accretion-discord',
        chart='./charts/accretion-discord',
        namespace=namespace,
        flags=[
            # Every static setting is in deploy/tilt.yaml under
            # `accretion-discord`, including where the bot reads its own API key
            # from. `config.database.host` used to be set here and was dead: the
            # bot has no database connection, every cluster query goes through
            # accretion-api, and that chart declares no config.database at all.
            '--values', chart_values['accretion-discord'],
            '--set', 'secret.botToken=' + discord_bot_token,
        ] + ([
            '--set', 'secret.anthropicApiKey=' + anthropic_api_key,
        ] if anthropic_api_key else []) + ([
            '--set', 'discord.guildID=' + discord_guild_id,
        ] if discord_guild_id else []),
        image_deps=['accretion-api'],
        image_keys=[('image.repository', 'image.tag')],
        # sync-secrets is named explicitly rather than left to arrive through
        # api-ca-cert: the bot's api-key secretKeyRef is not optional, so the
        # Secret has to exist before the pod is created, and a dependency that
        # holds only by transitivity is one refactor from not holding.
        resource_deps=['accretion-core.namespace', 'sync-secrets', 'db-init', 'api-ca-cert'],
        labels=['services'],
    )

# Accretion Collector — deployed with the built-in helm() (not the helm_resource
# extension) so Tilt assembles a resource per workload, which is what lets the
# data-collector and status-collector carry different dependencies (see the
# metrics-server note further down: since #92 the metric collector shares a
# process with log, event and trace, so that dependency is the whole
# data-collector's). Tilt auto-injects the freshly built
# `accretion-api` image wherever the chart references it (the chart's default
# image repository is already `accretion-api`), so image_deps/image_keys are no
# longer needed.
#
# Every static setting — the three workers' images, the single-replica sizing,
# the database host and the Ollama URL — is in deploy/tilt.yaml under
# `accretion-collector`. Only the monitored namespace list is built here,
# because it comes from `ns_monitors`, which an operator overrides on the
# command line or in a local tilt_config.json.
collector_set = [
    # Watch the namespaces where workloads actually run ({...} => a Helm list)
    'config.k8s.namespaces={' + ','.join(ns_monitors) + '}',
]
# Server-only (`make tilt-server-only`, or enable_collector=false in a local
# tilt_config.json) skips the collector chart entirely. Rendering it with
# dataCollector.enabled=false and statusCollector.enabled=false leaves only the
# ConfigMap and two Secrets, which nothing but the collectors reads -- while the
# k8s_resource calls below name ServiceAccounts, ClusterRoles and Deployments
# that then do not exist, and Tilt fails at load on the first fragment it cannot
# resolve, so NOTHING deploys (#101). Guarded the same way as the Discord block.
if enable_collector:
    k8s_yaml(helm(
        './charts/accretion-collector',
        name='accretion-collector',
        namespace=namespace,
        values=[chart_values['accretion-collector']],
        set=collector_set,
    ))

    # The shared/cluster-scoped objects (per-worker ServiceAccounts + RBAC, the
    # config ConfigMap, the postgres Secret, PDBs) can't be auto-attached to a
    # single workload, so pin them to one `accretion-collector-shared` resource
    # rather than leaving Tilt to bucket them into `uncategorized`. Every worker
    # depends on it, which also guarantees the ServiceAccounts exist before their
    # pods are created.
    #
    # This list and the chart's render must name the SAME set of objects, in both
    # directions, and each direction fails differently. A name here the chart does
    # not render fails the LOAD, so nothing deploys at all -- that is what
    # `accretion-collector-redis-secret` did once #123 gated the Redis Secret on
    # scale-out, which the values above deliberately do not enable. An object the
    # chart renders and this list does not name fails NOTHING: Tilt buckets it
    # into an `uncategorized` resource and carries on. `make tiltfile-check`
    # asserts both directions.
    collector_rbac_names = [
        'accretion-collector-data-collector',
        'accretion-collector-status-collector',
        # trace is split out by default for isolation (#100), so it has its
        # own identity.
        'accretion-collector-trace-collector',
    ]
    # accretion-collector-postgresql-secret is deliberately ABSENT since #134,
    # for exactly the reason accretion-collector-redis-secret is: the chart no
    # longer renders it on this path. deploy/tilt.yaml points the collector at
    # config.database.existingSecret (accretion-api-secrets, built by
    # sync-secrets from .env), and templates/secrets.yaml renders a Secret of its
    # own ONLY when no existingSecret is set. Naming an object the chart does not
    # render fails the LOAD, so nothing would deploy at all.
    collector_shared_objects = [
        'accretion-collector-config:configmap',
    ]
    for nm in collector_rbac_names:
        collector_shared_objects.append(nm + ':serviceaccount')
        collector_shared_objects.append(nm + ':clusterrole')
        collector_shared_objects.append(nm + ':clusterrolebinding')
    k8s_resource(
        new_name='accretion-collector-shared',
        objects=collector_shared_objects,
        resource_deps=['db-init'],
        labels=['services'],
    )

    # Tilt assembles one resource per collector workload. These only need the
    # database + shared objects to be ready — unlike the data-collector below.
    # The controller and Redis are absent from a default install since #93 -- they
    # shard work across scaled collectors, and nothing here is scaled -- so they are
    # not listed. Scale a collector (a replicaCount or autoscaling --set in
    # collector_set above) and they come back, at which point Tilt buckets them
    # into uncategorized until they are named here.
    collector_bulk = [
        'accretion-collector-status-collector',
        'accretion-collector-trace-collector',
    ]
    for r in collector_bulk:
        k8s_resource(r, resource_deps=['accretion-collector-shared'], labels=['services'])

    # The metric collector is the only consumer of the Kubernetes Metrics API, and
    # since #92 it shares a process with the log and event collectors — so
    # that dependency is now the whole data-collector's. This is a real cost of
    # consolidation: metrics-server being slow to come up delays log and event
    # collection too, where before it delayed only the metric worker. An operator
    # who needs that isolation back splits the metric collector into its own
    # Deployment (dataCollector.kinds.metric.split=true).
    k8s_resource(
        'accretion-collector-data-collector',
        resource_deps=['accretion-collector-shared', 'k8s-metrics-server'],
        labels=['services'],
    )

# Create obs namespace
local_resource(
    'obs-namespace',
    cmd='kubectl create namespace obs --dry-run=client -o yaml | kubectl apply -f -',
    labels=['obs'],
)

# OTel Collector
helm_resource(
    'otel-collector',
    chart='./charts/otel-collector',
    namespace='obs',
    # Chart defaults; see the accretion-db release above for why the empty
    # values file is passed rather than omitted.
    flags=['--values', chart_values['otel-collector']],
    resource_deps=['obs-namespace'],
    labels=['obs'],
    port_forwards=[],
)

# Port-forwards into the obs stack.
#
# `resource_deps=['otel-collector']` gates on the Helm release being applied, not
# on the pods behind it being ready -- the collector reports ready seconds before
# Grafana and Loki finish starting. A bare `kubectl port-forward` exits non-zero
# when no ready pod backs the Service, and Tilt does not restart a serve_cmd that
# exits, so the resource went red and stayed red while the pod behind it was
# perfectly healthy. Grafana and Loki lost that race every time; Prometheus and
# Tempo won it by a couple of seconds, which is luck, not correctness.
#
# Retrying also reconnects the forward after a pod restart, which otherwise drops
# it silently for the rest of the session.
def obs_port_forward(name, service, port):
    # Retry, but not forever, and not blindly: a forward that survived at least
    # 5s was genuinely up and merely dropped (pod restart), so the failure count
    # resets. Consecutive fast failures mean something that will not fix itself
    # -- the local port already bound, a Service that never appears -- and after
    # ~90s the resource exits non-zero so Tilt shows it red. An unbounded retry
    # would leave it green while forwarding nothing.
    script = '\n'.join([
        'n=0',
        'while true; do',
        '  t0=$(date +%s)',
        '  kubectl port-forward -n obs svc/{svc} {port}:{port}',
        '  if [ $(($(date +%s) - $t0)) -ge 5 ]; then n=0; else n=$((n + 1)); fi',
        '  if [ $n -ge 30 ]; then',
        '    echo "[{name}] :{port} failed 30 times in a row - giving up"',
        '    exit 1',
        '  fi',
        '  echo "[{name}] :{port} not forwarding (attempt $n/30) - retrying in 2s"',
        '  sleep 2',
        'done',
    ]).format(svc=service, port=port, name=name)

    local_resource(
        name,
        serve_cmd=script,
        resource_deps=['otel-collector'],
        labels=['obs'],
        links=['http://localhost:%d' % port],
    )

obs_port_forward('grafana', 'otel-collector-grafana', 3000)
obs_port_forward('loki', 'otel-collector-loki', 3100)
obs_port_forward('prometheus', 'otel-collector-prometheus', 9090)
obs_port_forward('tempo', 'otel-collector-tempo', 3200)

# Wait for PostgreSQL to be ready and accretion database to exist
local_resource(
    'db-init',
    cmd='kubectl wait --for=condition=complete job/accretion-db-schema -n ' + db_namespace + ' --timeout=120s && ' +
        'kubectl exec -n ' + db_namespace + ' $(kubectl get pod -l app.kubernetes.io/name=postgresql -n ' + db_namespace + ' -o jsonpath="{.items[0].metadata.name}") -- ' +
        'psql -U postgres -d accretion -c "SELECT 1"',
    resource_deps=['accretion-db'],
    labels=['infra'],
)

# Show schema job output (tables and indexes)
local_resource(
    'db-schema',
    cmd='kubectl wait --for=condition=complete job/accretion-db-schema -n ' + db_namespace + ' --timeout=120s && ' +
        'kubectl logs -n ' + db_namespace + ' job/accretion-db-schema -c schema',
    resource_deps=['accretion-db'],
    labels=['infra'],
)

# Manual triggers for common tasks

local_resource(
    'reset-database',
    cmd='kubectl scale statefulset -l app.kubernetes.io/name=postgresql -n ' + db_namespace + ' --replicas=0 && kubectl wait --for=delete pod -l app.kubernetes.io/name=postgresql -n ' + db_namespace + ' --timeout=60s; kubectl delete pvc -l app.kubernetes.io/name=postgresql -n ' + db_namespace + ' && kubectl scale statefulset -l app.kubernetes.io/name=postgresql -n ' + db_namespace + ' --replicas=1 && kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=postgresql -n ' + db_namespace + ' --timeout=120s && kubectl delete job accretion-db-schema -n ' + db_namespace + ' --ignore-not-found && helm upgrade --install accretion-db ./charts/accretion-db --namespace ' + db_namespace + ' && kubectl wait --for=condition=complete job/accretion-db-schema -n ' + db_namespace + ' --timeout=120s',
    labels=['tools'],
    auto_init=False,
    trigger_mode=TRIGGER_MODE_MANUAL,
    resource_deps=['accretion-db'],
)

# Welcome message
print("""
╔══════════════════════════════════════════════════════════════╗
║                    Accretion Development                     ║
╠══════════════════════════════════════════════════════════════╣
║  API:     https://localhost:41337/api/health                 ║
║           (verify with --cacert .tls/ca.crt)                 ║
║  Tilt UI: http://localhost:10350                             ║
║  Web UI:  off by default (config.ui.enabled)                 ║
║                                                              ║
║  Groups:                                                     ║
║    app   — accretion-api (API listener on 41337)             ║
║    infra — accretion-collector (Postgres + Redis + workers)  ║
║    tools — reset-database                                    ║
╚══════════════════════════════════════════════════════════════╝
""")
print("  collector: " + str(enable_collector) + "\n")

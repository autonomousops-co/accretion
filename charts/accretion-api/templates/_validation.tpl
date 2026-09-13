{{/*
Chart-level security guards. These are named templates; they are executed by
the rendered stub in validate-security.yaml so that `helm template` fails fast
on insecure configuration.
*/}}

{{/*
Refuse to render when a strong database password is required but the configured
value is empty or the insecure default "postgres". Operators must supply a real
secret via `--set-string config.database.password=...` or an existingSecret.
The guard is opt-in via `.Values.security.enforceStrongDbPassword` — values.yaml
sets it `false` explicitly, so dev/staging values (which do not set it
themselves) continue to template with their local defaults; values-prod.yaml
sets it `true`.

House idiom, also used below for forbidPlaintextAPIKeys in
validateAPIKeyIdentities: `--set security=null`, or a leaf null like `--set
security.enforceStrongDbPassword=null`, deletes the key from the merged
Values entirely instead of falling back to whatever a -f values file set —
confirmed by dumping .Values.security, the key comes back simply absent, not
present as false. A naive check that reads absence as "off" is wrong whenever
a values file meant it to be on. Comparing the formatted value instead of the
raw one closes it: only an explicit `false` is the off switch; `true`,
missing, and `null` all leave the guard active.

Polarity trap specific to this flag: its own chart default is `false`
(forbidPlaintextAPIKeys's is `true`), so "active only when explicitly true" —
`eq (printf "%v" $sec.enforceStrongDbPassword) "true"` — looks like the
correct inverse, and it is for values.yaml's own default. But absent is
neither `true` nor `false`, so that form reads a nulled flag as inactive too,
same as never configuring it — verified concretely against values-prod.yaml,
which sets the flag true: with the insecure default password, `--set
security=null` rendered successfully under that form, defeating the one
environment that turns the guard on. The idiom's actual comparison, `ne
(printf "%v" $sec.enforceStrongDbPassword) "false"`, closes it: values.yaml's
real, explicit `false` is unaffected (it is not ambiguous), so a bare `helm
install` keeps rendering with the checked-in "postgres" password. Only a
deliberate `security=null` — already a malformed, adversarial override —
makes the flag stricter than the values file asked for, the direction it is
safe to fail toward.
*/}}
{{- define "accretion-api.validateDatabasePassword" -}}
{{- $sec := .Values.security | default dict }}
{{- if ne (printf "%v" $sec.enforceStrongDbPassword) "false" }}
{{- /*
When the operator supplies config.database.existingSecret, the chart references
that external secret for the db-password and never consults
config.database.password — so the empty/"postgres" check does not apply. Skip it
entirely in that path (matches the guidance in values.yaml / values-prod.yaml).
*/}}
{{- if eq (.Values.config.database.existingSecret | default "") "" }}
{{- $pw := .Values.config.database.password | default "" }}
{{- if or (eq $pw "") (eq $pw "postgres") }}
{{- fail "SECURITY: config.database.password is empty or the insecure default \"postgres\". Supply a real password via --set-string config.database.password=... or an existingSecret (see charts/accretion-api/values-prod.yaml)." }}
{{- end }}
{{- end }}
{{- if .Values.postgresql.enabled }}
{{- $adminPw := .Values.postgresql.auth.postgresPassword | default "" }}
{{- $existing := .Values.postgresql.auth.existingSecret | default "" }}
{{- if and (eq $existing "") (or (eq $adminPw "") (eq $adminPw "postgres")) }}
{{- fail "SECURITY: postgresql.auth.postgresPassword is empty or the insecure default \"postgres\" and no existingSecret is set. Supply postgresql.auth.existingSecret or --set-string postgresql.auth.postgresPassword=..." }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Refuse to render an authenticated deployment that has no API-key identity, and
refuse any identity that could not authorize anything.

Fail-closed on an empty list is the point of the guard: with no identities the
server accepts no machine caller at all, and the previous shared unscoped key —
the mechanism this replaces — deliberately does not satisfy it.

An entry must carry exactly one of `hash` (hex SHA-256, plaintext stays out of
the release) or `key` (plaintext, dev convenience — the chart derives the hash).
`permissions` must be non-empty: HasPermission is an exact match with no
hierarchy, so an entry without permissions authenticates and then 403s on every
route. `name` becomes a Secret data key (api-key-<name>), so it is constrained
to a DNS-1123 label.

`security.forbidPlaintextAPIKeys` additionally rejects any `key` entry. Set it
wherever plaintext must not enter the release — staging and prod do, and CI
renders the checked-in values files with it so an example file cannot regress
into shipping a working credential (which is what it did before it was caught).
It defaults to true (values.yaml). `$forbidPlaintext` below uses the same
null-safe idiom as validateDatabasePassword above (see that comment for the
mechanism): only an explicit `false` disables it, so a missing or nulled
security.forbidPlaintextAPIKeys stays active rather than silently readmitting
plaintext. Here that coincides with the flag's own default, so there is no
polarity trap to warn about — unlike enforceStrongDbPassword, "active unless
false" and "true by default" agree.

There is no longer a mutual-exclusion case to check first. The chart used to
publish an identity's plaintext `key` to clients through an `mcpSecret` Secret,
which no identity could ever satisfy alongside this guard — so every deployment
that wanted a local MCP credential had to turn the guard OFF, which is why the
local stack ran with it disabled. #122 removed that template:
scripts/make-secret.sh creates accretion-mcp-secrets from .env instead, the
plaintext never enters the Helm release at all, and the guard stays on
everywhere including locally.
*/}}
{{- define "accretion-api.validateAPIKeyIdentities" -}}
{{- $sec := .Values.security | default dict }}
{{- $forbidPlaintext := ne (printf "%v" $sec.forbidPlaintextAPIKeys) "false" }}
{{- $identities := .Values.apiKeys | default list }}
{{- if and (not .Values.disableAuthentication) (not $identities) }}
{{- fail "SECURITY: authentication is enabled but `apiKeys` is empty, so no client could authenticate. Declare one entry per caller (see charts/accretion-api/values.yaml)." }}
{{- end }}
{{- $seen := dict }}
{{- range $i, $id := $identities }}
{{- $name := $id.name | default "" }}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $name) }}
{{- fail (printf "apiKeys[%v]: name %q must be a DNS-1123 label (it becomes the Secret data key api-key-<name>)." $i $name) }}
{{- end }}
{{- if hasKey $seen $name }}
{{- fail (printf "apiKeys[%v]: duplicate name %q — each client needs its own identity." $i $name) }}
{{- end }}
{{- $_ := set $seen $name true }}
{{- $hash := $id.hash | default "" }}
{{- $key := $id.key | default "" }}
{{- if and (eq $hash "") (eq $key "") }}
{{- fail (printf "apiKeys[%v] (%s): set exactly one of `hash` (hex SHA-256) or `key` (plaintext, dev only)." $i $name) }}
{{- end }}
{{- if and (ne $hash "") (ne $key "") }}
{{- fail (printf "apiKeys[%v] (%s): set exactly one of `hash` or `key`, not both." $i $name) }}
{{- end }}
{{- if and (ne $hash "") (not (regexMatch "^[0-9a-fA-F]{64}$" $hash)) }}
{{- fail (printf "apiKeys[%v] (%s): hash must be 64 hex characters — the output of `printf '%%s' \"$KEY\" | shasum -a 256`." $i $name) }}
{{- end }}
{{- if and (ne $key "") $forbidPlaintext }}
{{- fail (printf "SECURITY: apiKeys[%v] (%s) carries a plaintext `key`, but security.forbidPlaintextAPIKeys is set. Supply `hash` instead so the plaintext never enters the Helm release: printf '%%s' \"$KEY\" | shasum -a 256" $i $name) }}
{{- end }}
{{- if not $id.permissions }}
{{- fail (printf "apiKeys[%v] (%s): permissions must be non-empty. Permissions are matched exactly (no hierarchy), so an entry with none authenticates and then 403s everywhere. Routes require read:memory and/or chat:query." $i $name) }}
{{- end }}
{{- range $j, $perm := $id.permissions }}
{{- if not (trim $perm) }}
{{- fail (printf "apiKeys[%v] (%s): permissions[%v] is empty. A blank permission matches no route, so it silently contributes nothing to the grant." $i $name $j) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Both listeners disabled means the pod comes up healthy and serves no one, and
identical ports mean the second listener fails to bind at runtime — a crash loop
whose cause is not in any manifest. Both are cheaper to catch here.
*/}}
{{- define "accretion-api.validateListeners" -}}
{{- if and (not .Values.config.api.enabled) (not .Values.config.ui.enabled) }}
{{- fail "config.api.enabled and config.ui.enabled are both false: nothing would listen. Enable at least one." }}
{{- end }}
{{- if and .Values.config.api.enabled .Values.config.ui.enabled }}
{{- if eq (int .Values.config.api.port) (int .Values.config.ui.port) }}
{{- fail (printf "config.api.port and config.ui.port are both %v. The second listener would fail to bind; give them distinct ports." .Values.config.api.port) }}
{{- end }}
{{- if eq (int .Values.service.port) (int .Values.service.uiPort) }}
{{- fail (printf "service.port and service.uiPort are both %v. Kubernetes keys Service ports on the port number, so two entries sharing one render an invalid Service." .Values.service.port) }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
TLS enabled with no certificate source would render a Deployment that mounts a
Secret nobody creates: the pod stays Pending on a missing volume, or — worse, if
the Secret exists but is empty — the server aborts at startup on an unloadable
keypair. Both are diagnosable only from inside the cluster, so refuse at render
time instead, consistent with the chart's other fail-closed guards.
*/}}
{{- define "accretion-api.validateTLS" -}}
{{- $tls := .Values.config.api.tls }}
{{- if $tls.enabled }}
{{- if and (not $tls.certManager.enabled) (eq ($tls.existingSecret | default "") "") }}
{{- fail "config.api.tls.enabled is true but no certificate source is configured. Set config.api.tls.certManager.enabled=true (cert-manager must be installed) or config.api.tls.existingSecret to a Secret holding tls.crt and tls.key." }}
{{- end }}
{{- if not (has ($tls.minVersion | default "1.2") (list "1.2" "1.3")) }}
{{- fail (printf "config.api.tls.minVersion must be \"1.2\" or \"1.3\", got %q. TLS 1.0 and 1.1 are deprecated (RFC 8996) and the server refuses them." $tls.minVersion) }}
{{- end }}
{{- if not $tls.mountPath }}
{{- fail "config.api.tls.mountPath is empty; the certificate paths are derived from it." }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Tempo backs /api/traces and /api/traces/{traceID}. config.tempo.enabled with no
base_url is the shape of the bug that made trace read-back inert: the endpoints
answer, the reads go nowhere, and nothing says so. The same for telemetry, which
pays the full cost of tracing and needs somewhere to send it (#94).

This chart cannot see whether the Tempo Deployment exists -- that lives in the
otel-collector chart, which has its own guard on the same combination
(charts/otel-collector/templates/_validation.tpl). What it can refuse is a
configuration that cannot possibly work.
*/}}
{{- define "accretion-api.validateObservability" -}}
{{- if .Values.config.tempo.enabled }}
{{- if eq (.Values.config.tempo.base_url | default "") "" }}
{{- fail "config.tempo.enabled is true but config.tempo.base_url is empty. /api/traces reads from Tempo's query API; without a URL those endpoints answer with nothing and never say why. Set config.tempo.base_url, or set config.tempo.enabled=false." }}
{{- end }}
{{- end }}
{{- if .Values.config.telemetry.enabled }}
{{- if eq (.Values.config.telemetry.endpoint | default "") "" }}
{{- fail "config.telemetry.enabled is true but config.telemetry.endpoint is empty. Tracing would be instrumented and then dropped. Set config.telemetry.endpoint to the OTLP collector, or set config.telemetry.enabled=false." }}
{{- end }}
{{- end }}
{{- end -}}

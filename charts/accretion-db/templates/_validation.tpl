{{/*
Fail-closed guards for the accretion-db chart, in the same spirit as the
accretion-api and accretion-collector charts'. These are named templates;
templates/validate.yaml executes them, so an insecure or incoherent
configuration is a failed `helm template`/`helm upgrade` rather than a database
that comes up quietly wrong.
*/}}

{{/*
Refuse to render when no database password has been supplied (#134).

THE SUBCHART IS WHY THIS HAS TO BE A HARD FAILURE. Bitnami's postgresql chart
does not refuse an absent password: with auth.postgresPassword empty and no
auth.existingSecret it GENERATES a random one into a Secret of its own. That
renders green, installs green, and produces a database whose password no client
holds -- not accretion-api, not the collectors, not this chart's own schema job,
each of which gets the password from .env or an operator Secret. The symptom is
authentication failures inside the cluster with nothing in any manifest saying
why. So the guard fires before the subchart can, and it names the variable to
set rather than inventing a value that happens to let the render finish.

"postgres" is rejected alongside empty on both sides. It is not a password; it
is the literal this chart shipped until #134, and accepting it would let a copy
of the old values file walk straight back in.
*/}}
{{- define "accretion-db.validateDatabasePassword" -}}
{{- $pg := .Values.postgresql | default dict }}
{{- if $pg.enabled }}
{{- $auth := $pg.auth | default dict }}
{{- $adminPw := $auth.postgresPassword | default "" }}
{{- $adminExisting := $auth.existingSecret | default "" }}
{{- if and (eq $adminExisting "") (or (eq $adminPw "") (eq $adminPw "postgres")) }}
{{- fail "SECURITY: no PostgreSQL superuser password. postgresql.auth.postgresPassword is empty or the insecure default \"postgres\" and postgresql.auth.existingSecret is not set, and the Bitnami subchart would GENERATE a random one that none of this platform's clients hold. Set postgresql.auth.existingSecret to a Secret carrying the password under postgres-password -- scripts/make-secret.sh publishes ACCRETION_DATABASE_PASSWORD from your gitignored .env into accretion-db-secrets under exactly that key (see env.example). charts/README.md documents the --set-string escape hatch and why a password passed as a Helm value is readable afterwards with `helm get values`." }}
{{- end }}
{{- end }}
{{- $existing := .Values.config.database.existingSecret | default "" }}
{{- if eq $existing "" }}
{{- $pw := .Values.config.database.password | default "" }}
{{- if or (eq $pw "") (eq $pw "postgres") }}
{{- fail "SECURITY: no database password for the schema job and the partition-maintenance cronjob. config.database.password is empty or the insecure default \"postgres\", and config.database.existingSecret is not set. Set config.database.existingSecret to a Secret carrying the password (config.database.existingSecretKey names the key, default db-password) -- scripts/make-secret.sh publishes ACCRETION_DATABASE_PASSWORD from your gitignored .env into accretion-db-secrets under that key. charts/README.md documents the --set-string escape hatch and why a password passed as a Helm value is readable afterwards with `helm get values`. The chart will not invent one." }}
{{- end }}
{{- else }}
{{/*
existingSecret set: this chart renders no Secret of its own and never sees
config.database.password. It can still assert the key NAME it will reference is
non-empty -- an empty one renders a secretKeyRef with no key, which fails at
each job's rollout instead, separately and with nothing pointing back here. It
cannot go further: confirming the Secret actually carries that key needs cluster
access, which `helm template` and `helm lint` never have. Same assertion, same
limit, as accretion-collector's existingSecretKey guard.
*/}}
{{- if not (.Values.config.database.existingSecretKey | default "") }}
{{- fail "config.database.existingSecret is set but config.database.existingSecretKey is empty -- the schema job and the partition-maintenance cronjob read the password from this key via secretKeyRef and need a name. Set it (default \"db-password\"), or clear config.database.existingSecret to let the chart manage its own Secret." }}
{{- end }}
{{- end }}
{{- end -}}

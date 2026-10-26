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
{{/*
Single-source cross-check (#143). values.yaml promises "one variable feeds both
sides and they cannot disagree" -- the Postgres superuser (postgresql.auth) and
the schema job + partition cronjob (config.database). The two guards above each
check only their OWN side, so a MIXED shape -- an existingSecret on one side and a
literal on the other -- satisfies both independently while the two halves point at
different passwords. The server then comes up on one password and the jobs
authenticate with another, and nothing in any rendered manifest says why: each
value is individually valid, only their disagreement is wrong. Refuse it here, by
name. The two coherent shapes -- BOTH an existingSecret, or BOTH a literal -- pass;
this rejects only the disagreement between them. (Two different existingSecret
NAMES cannot be compared without cluster access, the same limit the existingSecret
guards note.)
*/}}
{{- $jobsExisting := .Values.config.database.existingSecret | default "" }}
{{- $adminUsesSecret := ne $adminExisting "" }}
{{- $jobsUsesSecret := ne $jobsExisting "" }}
{{- if ne $adminUsesSecret $jobsUsesSecret }}
{{- if $adminUsesSecret }}
{{- fail "SECURITY: mixed database password sources (#143). postgresql.auth.existingSecret is set, so the Postgres superuser is read from that Secret, while config.database.password is a literal that the schema job and the partition-maintenance cronjob use -- so the server and the jobs can hold DIFFERENT passwords, which values.yaml promises is impossible and the schema job fails authentication on at rollout. Point config.database.existingSecret at the SAME Secret (scripts/make-secret.sh publishes ACCRETION_DATABASE_PASSWORD under both postgres-password and db-password so one value feeds both), or supply both sides as literals -- do not mix a Secret with a literal." }}
{{- else }}
{{- fail "SECURITY: mixed database password sources (#143). config.database.existingSecret is set, so the schema job and the partition-maintenance cronjob read the password from that Secret, while postgresql.auth.postgresPassword is a literal that the Postgres superuser is initialised from -- so the server and the jobs can hold DIFFERENT passwords, which values.yaml promises is impossible and the schema job fails authentication on at rollout. Point postgresql.auth.existingSecret at the SAME Secret, or supply both sides as literals -- do not mix a Secret with a literal." }}
{{- end }}
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

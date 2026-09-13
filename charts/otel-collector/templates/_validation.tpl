{{/*
Fail-closed guards for the otel-collector chart.

The bug shape these exist for: a pipeline kept exporting to a backend whose
Deployment had been turned off. Nothing failed -- the collector started, the
export errored into its own logs, and the feature that read from that backend
was quietly inert. Trace read-back was broken that way. A render-time failure
turns "silently collecting nothing" into "your values are wrong, here is which
line".

STRUCTURE, because getting this wrong is how the last hole appeared: only the
pipeline-exporter guards may sit inside `if .Values.otelCollector.enabled`,
because a pipeline that is not rendered cannot export to anything. Every other
guard is at top level. A guard nested under a values key is a guard with an off
switch, and a component omitted from a guard's list is a hole -- an earlier
revision had both, and `--set otelCollector.enabled=false --set
grafana.enabled=true` rendered Grafana with dashboards querying a Prometheus
that was not deployed.
*/}}

{{- define "otel-collector.validate" -}}

{{/*
Nothing in this chart feeds the backends except the collector, so the collector
being off while any of them is on is the same "quietly inert" failure as the
pipeline guards below, from the other direction. Grafana counts: it reads from
the other three, and with the collector gone there is nothing for them to hold
even when they are deployed.
*/}}
{{- if not .Values.otelCollector.enabled -}}
{{- $on := list -}}
{{- if .Values.tempo.enabled -}}{{- $on = append $on "tempo" -}}{{- end -}}
{{- if .Values.loki.enabled -}}{{- $on = append $on "loki" -}}{{- end -}}
{{- if .Values.prometheus.enabled -}}{{- $on = append $on "prometheus" -}}{{- end -}}
{{- if .Values.grafana.enabled -}}{{- $on = append $on "grafana" -}}{{- end -}}
{{- if $on -}}
{{- fail (printf "otel-collector: otelCollector.enabled is false but %s %s still enabled. Nothing in this chart feeds them, so they would run and stay empty. Disable them too, or re-enable the collector." (join ", " $on) (ternary "is" "are" (eq (len $on) 1))) -}}
{{- end -}}
{{- end -}}

{{/*
Grafana is a reader of the other three. On its own it renders dashboards whose
panels query datasources that do not exist -- confusing rather than broken, but
confusing in exactly the way this file exists to prevent. Top level, so it holds
whether or not the collector is enabled.
*/}}
{{- if and .Values.grafana.enabled (not (or .Values.tempo.enabled .Values.loki.enabled .Values.prometheus.enabled)) -}}
{{- fail "otel-collector: grafana.enabled is true but Tempo, Loki and Prometheus are all disabled, so Grafana would have no data source to read. Enable at least one backend, or set grafana.enabled=false." -}}
{{- end -}}

{{- if .Values.otelCollector.enabled -}}

{{/*
Collect every exporter named by any configured pipeline, then require the
backend behind it to be deployed. Only the three local backends are checked;
debug and the Grafana Cloud / accretion exporters have no Deployment here.

These are the only guards that belong under otelCollector.enabled: with the
collector off there are no pipelines to be inconsistent with.
*/}}
{{- $used := dict -}}
{{- range $pipeline, $spec := .Values.config.service.pipelines -}}
{{- range $exporter := $spec.exporters -}}
{{- $_ := set $used $exporter $pipeline -}}
{{- end -}}
{{- end -}}

{{- if and (hasKey $used "otlp/tempo") (not .Values.tempo.enabled) -}}
{{- fail (printf "otel-collector: pipeline %q exports to otlp/tempo but tempo.enabled is false. Traces would be accepted and dropped, and accretion's /api/traces would return nothing. Set tempo.enabled=true, or remove otlp/tempo from that pipeline's exporters." (index $used "otlp/tempo")) -}}
{{- end -}}

{{- if and (hasKey $used "otlphttp/loki") (not .Values.loki.enabled) -}}
{{- fail (printf "otel-collector: pipeline %q exports to otlphttp/loki but loki.enabled is false. Set loki.enabled=true, or remove otlphttp/loki from that pipeline's exporters." (index $used "otlphttp/loki")) -}}
{{- end -}}

{{- if and (hasKey $used "prometheus") (not .Values.prometheus.enabled) -}}
{{- fail (printf "otel-collector: pipeline %q exports to the prometheus exporter but prometheus.enabled is false. Set prometheus.enabled=true, or remove prometheus from that pipeline's exporters." (index $used "prometheus")) -}}
{{- end -}}

{{/*
The mirror direction: a backend deployed with nothing exporting to it.

Enabling a backend is two changes, not one -- the Deployment and the pipeline
that feeds it -- and the minimal profile's logs and metrics pipelines export to
`debug` only. Without this, `-f values-minimal.yaml --set loki.enabled=true`
rendered a Loki that received nothing and said so nowhere, which is the same
quietly-inert failure from the opposite side. Failing here is what keeps the
guard set complete in both directions instead of only catching the half that
happened to bite first.
*/}}
{{- if and .Values.tempo.enabled (not (hasKey $used "otlp/tempo")) -}}
{{- fail "otel-collector: tempo.enabled is true but no pipeline exports to otlp/tempo, so Tempo would be deployed and receive nothing. Add otlp/tempo to config.service.pipelines.traces.exporters, or set tempo.enabled=false." -}}
{{- end -}}

{{- if and .Values.loki.enabled (not (hasKey $used "otlphttp/loki")) -}}
{{- fail "otel-collector: loki.enabled is true but no pipeline exports to otlphttp/loki, so Loki would be deployed and receive nothing. Add otlphttp/loki to config.service.pipelines.logs.exporters, or set loki.enabled=false." -}}
{{- end -}}

{{- if and .Values.prometheus.enabled (not (hasKey $used "prometheus")) -}}
{{- fail "otel-collector: prometheus.enabled is true but no pipeline exports to the prometheus exporter, so Prometheus would be deployed and scrape nothing. Add prometheus to config.service.pipelines.metrics.exporters, or set prometheus.enabled=false." -}}
{{- end -}}

{{/*
grafanaCloud defines the otlp/grafana exporter in configmap.yaml but no shipped
pipeline lists it, so enabling it and filling in credentials exported nothing
and reported nothing. Same rule as the three above: a backend configured with
nothing feeding it is a misconfiguration, not a no-op.
*/}}
{{- if and .Values.grafanaCloud.enabled (not (hasKey $used "otlp/grafana")) -}}
{{- fail "otel-collector: grafanaCloud.enabled is true but no pipeline exports to otlp/grafana, so nothing would be sent to Grafana Cloud. Add otlp/grafana to the exporters of the pipelines you want forwarded (traces and/or metrics), or set grafanaCloud.enabled=false." -}}
{{- end -}}

{{- end -}}
{{- end -}}

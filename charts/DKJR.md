# DKJR — the chaos fixture (dev team, on demand)

DKJR (*Disruptive Kubernetes Jeopardy Randomizer*) is a synthetic misbehaving
workload: it burns CPU, leaks memory, stalls the network, and returns errors on
a 1–10 intensity/frequency/consistency scale, so the collectors have something
interesting to record. It is a **development fixture, not part of Accretion**.

It used to ship inside the `accretion-api` chart and be brought up by `make up`
from a sibling `../dkjr` clone. It no longer is: Accretion's default bring-up
deploys Accretion only, and DKJR is installed on demand with a plain
`helm install` from its own repository, which owns the chart, the image, and
the documentation of every knob.

> **That repository is not public (#130).** `autonomousops-co/dkjr` is an
> internal development fixture, so the `git clone` and `helm install` lines
> below resolve only for someone who already has access to it. They are kept
> here because they are what the dev team runs -- not as instructions a reader
> of the public repository can follow. Nothing in Accretion needs DKJR: it
> exists to give the collectors something interesting to record, and every
> instruction outside this page stands without it.

## Install

```bash
git clone git@github.com:autonomousops-co/dkjr.git
cd dkjr

kubectl create namespace dkjr

# Create the Secret yourself, from a file, then let the chart reference it.
# The chart's defaults (secret.create=false, secret.name=dkjr-secret) already
# expect this.
kubectl create secret generic dkjr-secret -n dkjr \
  --from-file=openai-key=<path-to-key-file>

helm upgrade --install dkjr ./helm-chart --namespace dkjr
```

**Do not pass the key to Helm** — not `--set secret.openaiKey=...`, not
`--set secret.create=true`. Helm persists user-supplied values verbatim in the
release Secret, so the key comes back in cleartext from `helm get values dkjr -n
dkjr` and from anything with `get secrets` in that namespace, and `--set` puts it
in argv where it is readable from `/proc/<pid>/cmdline` for the life of the
process. The integration this replaced never made a second plaintext copy — it
pointed at the existing `accretion-api-secrets` — and
[`scripts/make-secret.sh`](../scripts/make-secret.sh) goes to `0600` +
`--from-file` lengths for exactly this reason. Keep it that way.

Against a local kind cluster, build and load the image first (the chart's
default `image.repository` is the bare name `dkjr`), then add
`--set image.pullPolicy=IfNotPresent`. Note that the repo's `deploy-local.sh` is
the shorter path but takes its OpenAI key from the environment and installs
with `secret.create=true` — convenient for a throwaway kind cluster, not for a
shared one.

Tear it down with `helm uninstall dkjr -n dkjr && kubectl delete namespace dkjr`.

## What Accretion expects

**Add the namespace to Accretion's watched list yourself.** `dkjr` used to be
pre-listed so this install needed no reconfiguration; it was removed from every
list because a name that resolves to nothing is hard to tell from a mistake, and
it read as one. Accretion collects nothing from DKJR until you add it back.

- **Namespace.** Add `dkjr` (or whichever namespace you install into) to
  whichever of these your deployment reads:
  `config.k8s.namespaces` in [`accretion-collector/values.yaml`](accretion-collector/values.yaml),
  `ns_monitors` in the [Tiltfile](../Tiltfile), `ACCRETION_K8S_NAMESPACES` in
  [`.envrc`](../.envrc), or `k8s.namespaces` in
  [`config.example.yaml`](../config.example.yaml). The status collector opens
  about nine informers per namespace listed, so add it while you are using the
  fixture and drop it when you are done.
- **Release name.** Install into the `dkjr` namespace, or add whichever
  namespace you choose to those lists.

DKJR emits **no OpenTelemetry spans** — it has no otel dependency at all; it is a
logs/metrics/status fixture. Do not expect it to populate Tempo or the trace
worker. Trace ingestion is scoped independently of the lists above, by
`filter/accretion` in
[`otel-collector/templates/configmap.yaml`](otel-collector/templates/configmap.yaml)
and `config.namespaces.traces` in
[`accretion-collector/values.yaml`](accretion-collector/values.yaml).

## Configuring the chaos

Every knob lives in the DKJR repo's `helm-chart/values.yaml` under `dkjr:` —
four simulators (`cpu`, `memory`, `network`, `service`), each with 1–10
`intensity` / `frequency` / `consistency` dials, plus `dkjr.serviceName`,
`dkjr.llmProvider` (point it at a host Ollama to avoid OpenAI spend), and
`resources`. That repo owns them; they are not reproduced here, because nothing
in this repo can test that a copy stays true.

```bash
helm upgrade --install dkjr ./helm-chart --namespace dkjr \
  --set dkjr.cpu.intensity=8
```

## Several instances

The old integration deployed N numbered instances from one release. With the
standalone chart, install it more than once — one release per personality:

```bash
helm upgrade --install chaos-cpu ./helm-chart -n dkjr --set dkjr.cpu.intensity=8
helm upgrade --install chaos-mem ./helm-chart -n dkjr --set dkjr.memory.intensity=9
```

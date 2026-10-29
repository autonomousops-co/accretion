#!/usr/bin/env bash
set -euo pipefail

# Based on: https://kind.sigs.k8s.io/docs/user/local-registry/

CLUSTER_NAME="accretion-dev"
REG_NAME="kind-registry"
REG_PORT="5001"
KIND_CONFIG="kind-config.yaml"

# 1. Ensure a local registry is available on REG_PORT. Reuse whatever is already
#    there instead of failing on a name clash (stopped container) or a port clash
#    (a registry someone else already started on this port).
if [ "$(docker inspect -f '{{.State.Running}}' "${REG_NAME}" 2>/dev/null || true)" = 'true' ]; then
  echo "Registry ${REG_NAME} already running."
elif docker inspect "${REG_NAME}" >/dev/null 2>&1; then
  # Container exists but is stopped: start it rather than recreating (a fresh
  # `docker run --name ${REG_NAME}` would fail with "name already in use").
  echo "Starting existing registry container ${REG_NAME}..."
  docker start "${REG_NAME}" >/dev/null
elif [ -n "$(docker ps --quiet --filter "publish=${REG_PORT}")" ]; then
  # A different container already publishes REG_PORT. Only adopt it if it is a
  # registry image -- otherwise fail loudly rather than wiring a non-registry
  # container into containerd as if it could serve images.
  found_name="$(docker ps --format '{{.Names}}' --filter "publish=${REG_PORT}" | head -n1)"
  found_image="$(docker inspect -f '{{.Config.Image}}' "${found_name}" 2>/dev/null || true)"
  if printf '%s' "${found_image}" | grep -qiE '(^|/)registry(:|$)'; then
    REG_NAME="${found_name}"
    echo "Reusing existing registry '${REG_NAME}' (${found_image}) already bound to port ${REG_PORT}."
  else
    echo "ERROR: port ${REG_PORT} is already in use by container '${found_name}'" >&2
    echo "       (image '${found_image}'), which is not a Docker registry." >&2
    echo "       Free the port or start a registry there, then re-run." >&2
    exit 1
  fi
else
  echo "Starting local registry on port ${REG_PORT}..."
  docker run -d --restart=always -p "127.0.0.1:${REG_PORT}:5000" --network bridge --name "${REG_NAME}" registry:2
fi

# 2. Create the Kind cluster
if kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  echo "Cluster ${CLUSTER_NAME} already exists."
else
  echo "Creating Kind cluster ${CLUSTER_NAME}..."
  kind create cluster --config "${KIND_CONFIG}"
fi

# 3. Configure containerd on each node to use the local registry
REGISTRY_DIR="/etc/containerd/certs.d/localhost:${REG_PORT}"
for node in $(kind get nodes --name "${CLUSTER_NAME}"); do
  docker exec "${node}" mkdir -p "${REGISTRY_DIR}"
  cat <<EOF | docker exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml"
[host."http://${REG_NAME}:5000"]
EOF
done

# 4. Connect the registry to the Kind network (if not already connected)
if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${REG_NAME}")" = 'null' ]; then
  echo "Connecting registry to Kind network..."
  docker network connect "kind" "${REG_NAME}"
fi

# 5. Publish the registry location via ConfigMap so Tilt auto-detects it
# https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${REG_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

echo "Kind cluster '${CLUSTER_NAME}' is ready with local registry at localhost:${REG_PORT}"

#!/usr/bin/env bash
set -euo pipefail

# Based on: https://kind.sigs.k8s.io/docs/user/local-registry/

CLUSTER_NAME="accretion-dev"
REG_NAME="kind-registry"
REG_PORT="5001"
KIND_CONFIG="kind-config.yaml"

# 1. Start a local registry container (if not already running)
if [ "$(docker inspect -f '{{.State.Running}}' "${REG_NAME}" 2>/dev/null || true)" != 'true' ]; then
  echo "Starting local registry on port ${REG_PORT}..."
  docker run -d --restart=always -p "127.0.0.1:${REG_PORT}:5000" --network bridge --name "${REG_NAME}" registry:2
else
  echo "Registry ${REG_NAME} already running."
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

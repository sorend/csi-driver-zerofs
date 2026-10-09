#!/bin/sh
#
# Run the Kubernetes external storage e2e suite (test/e2e/storage/external)
# against the ZeroFS CSI driver.
#
# e2e.test is the generic Kubernetes test binary: it has no knowledge of the
# driver and only defines the storage test suites when it is handed a driver
# definition through -storage.testdriver.  test/e2e/driverinfo.yaml is that
# definition; it names the driver, points at the StorageClass to provision from
# and declares which capabilities to test.
#
# Unlike csi-sanity (test/sanity), these tests drive the driver purely through
# the Kubernetes API - PVCs, pods, PVs - so the driver runs as it does in
# production: controller Deployment, node DaemonSet and a per-volume ZeroFS
# server backed by RustFS.
#
# Usage:
#   make e2e-test
#
# Environment:
#   KIND_CLUSTER      kind cluster name (default: zerofs-e2e)
#   KIND_NODE_IMAGE   kind node image (default: kindest/node:v1.37.0)
#   K8S_VERSION       version of e2e.test to download (default: v1.37.0)
#   E2E_BIN_DIR       where e2e.test is cached (default: /tmp/csi-e2e)
#   E2E_FOCUS         replaces the default ginkgo focus
#   E2E_SKIP          replaces the default ginkgo skip
#   E2E_ARGS          extra flags handed to e2e.test
#   DRIVER_IMAGE      driver image tag to build and load (default: ghcr.io/sorend/csi-driver-zerofs:latest)
#   ZEROFS_IMAGE      ZeroFS server image (default: ghcr.io/barre/zerofs:1.0.4)
#   BUCKET            S3 bucket backing the volumes (default: zerofs-data)
#   WORK_DIR          scratch dir for the test artifacts (default: /tmp/csi-e2e-run)
#   KEEP_CLUSTER      set to 1 to keep the kind cluster on exit

set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)

KIND_CLUSTER="${KIND_CLUSTER:-zerofs-e2e}"
KUBE_CONTEXT="kind-${KIND_CLUSTER}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
NAMESPACE="zerofs-csi"
STORAGE_CLASS="zerofs"

# e2e.test has to match the cluster version: it embeds the API machinery of the
# release it was built from and uses feature gates that moved between releases.
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.37.0}"
K8S_VERSION="${K8S_VERSION:-v1.37.0}"
E2E_BIN_DIR="${E2E_BIN_DIR:-/tmp/csi-e2e}"
E2E_TEST_BIN="${E2E_BIN_DIR}/e2e.test"

DRIVER_IMAGE="${DRIVER_IMAGE:-ghcr.io/sorend/csi-driver-zerofs:latest}"
ZEROFS_IMAGE="${ZEROFS_IMAGE:-ghcr.io/barre/zerofs:1.0.4}"
BUCKET="${BUCKET:-zerofs-data}"
WORK_DIR="${WORK_DIR:-/tmp/csi-e2e-run}"
JUNIT_REPORT="${WORK_DIR}/junit_e2e.xml"

E2E_ARGS="${E2E_ARGS:-}"

# The driver name has to be escaped for the ginkgo regex, so it is matched
# literally here rather than relying on the dots in the FQDN.
E2E_FOCUS="${E2E_FOCUS:-External Storage \[Driver: zerofs\.csi\.sorend\.github\.com\]}"

# Alpha and beta feature tests need feature gates and CRDs that the driver does
# not enable, and the Disruptive tests stop the kubelet, which is disruptive in
# a throwaway cluster. Both are run separately by upstream CI, not here.
E2E_SKIP="${E2E_SKIP:-\[Feature:|\[Disruptive\]}"

log() {
    echo "==> $*"
}

kubectl_cmd() {
    kubectl --context "$KUBE_CONTEXT" "$@"
}

teardown() {
    if [ "$KEEP_CLUSTER" = "1" ]; then
        log "Keeping kind cluster ${KIND_CLUSTER} (context ${KUBE_CONTEXT})"
    else
        log "Deleting kind cluster ${KIND_CLUSTER}"
        # The throwaway kubeconfig is not the host's, but drop it anyway so a
        # kept WORK_DIR does not accumulate a stale one.
        rm -f "${WORK_DIR}/kubeconfig"
        kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
    fi
}

dump_debug() {
    log "CSI controller logs"
    kubectl_cmd logs -n "$NAMESPACE" deployment/zerofs-csi-controller -c zerofs-plugin --tail=200 || true
    log "CSI provisioner logs"
    kubectl_cmd logs -n "$NAMESPACE" deployment/zerofs-csi-controller -c csi-provisioner --tail=100 || true
    log "Pods in ${NAMESPACE}"
    kubectl_cmd get pods -n "$NAMESPACE" -o wide || true
    log "Persistent volumes"
    kubectl_cmd get pv -o wide || true
    log "Persistent volume claims"
    kubectl_cmd get pvc -A || true
}

# download_e2e_test fetches e2e.test from the kubernetes-test tarball, which is
# published per release at dl.k8s.io. The tarball is self-contained, so there is
# nothing to build against the checked-out driver.
download_e2e_test() {
    if [ -x "$E2E_TEST_BIN" ] && [ "$("$E2E_TEST_BIN" --version 2>&1 | grep -c "$K8S_VERSION")" -ge 1 ]; then
        log "Reusing cached e2e.test ($K8S_VERSION)"
        return
    fi

    log "Downloading e2e.test ${K8S_VERSION}"
    mkdir -p "$E2E_BIN_DIR"
    TARBALL="${E2E_BIN_DIR}/kubernetes-test.tar.gz"
    curl -fsSL -o "$TARBALL" \
        "https://dl.k8s.io/release/${K8S_VERSION}/kubernetes-test-linux-amd64.tar.gz"
    tar -C "$E2E_BIN_DIR" -xzf "$TARBALL" kubernetes/test/bin/e2e.test
    mv "${E2E_BIN_DIR}/kubernetes/test/bin/e2e.test" "$E2E_TEST_BIN"
    rm -rf "${E2E_BIN_DIR}/kubernetes" "$TARBALL"
}

trap teardown EXIT

mkdir -p "$WORK_DIR"

download_e2e_test

kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true

log "Creating kind cluster ${KIND_CLUSTER} (${KIND_NODE_IMAGE})"
kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE" \
    --config "${ROOT_DIR}/test/e2e/kind-config.yaml" --wait 180s

log "Building driver image ${DRIVER_IMAGE}"
docker build -t "$DRIVER_IMAGE" "$ROOT_DIR"

log "Loading driver image into kind"
kind load docker-image "$DRIVER_IMAGE" --name "$KIND_CLUSTER"

log "Pulling ZeroFS server image ${ZEROFS_IMAGE} into kind"
docker exec "${KIND_CLUSTER}-control-plane" \
    ctr --namespace=k8s.io images pull "$ZEROFS_IMAGE"

log "Deploying CSI driver"
kubectl_cmd apply -f "${ROOT_DIR}/deploy/install.yaml"
kubectl_cmd patch deployment zerofs-csi-controller -n "$NAMESPACE" \
    --type=strategic \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"zerofs-plugin","imagePullPolicy":"Never"}]}}}}'
kubectl_cmd patch daemonset zerofs-csi-node -n "$NAMESPACE" \
    --type=strategic \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"zerofs-plugin","imagePullPolicy":"Never"}]}}}}'
kubectl_cmd rollout status deployment/zerofs-csi-controller -n "$NAMESPACE" --timeout=180s
kubectl_cmd rollout status daemonset/zerofs-csi-node -n "$NAMESPACE" --timeout=180s

log "Deploying RustFS"
kubectl_cmd apply -f "${ROOT_DIR}/test/rustfs.yaml"
kubectl_cmd rollout status deployment/rustfs -n "$NAMESPACE" --timeout=180s

log "Creating bucket ${BUCKET}"
kubectl_cmd delete pod s3-init -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
kubectl_cmd run s3-init \
    --image=amazon/aws-cli:latest \
    --restart=Never \
    -n "$NAMESPACE" \
    --env=AWS_ACCESS_KEY_ID=minioadmin \
    --env=AWS_SECRET_ACCESS_KEY=minioadmin123 \
    --env=AWS_DEFAULT_REGION=us-east-1 \
    --command -- /bin/sh -c \
    "aws --endpoint-url http://rustfs.${NAMESPACE}.svc.cluster.local:9000 s3api head-bucket --bucket ${BUCKET} >/dev/null 2>&1 || aws --endpoint-url http://rustfs.${NAMESPACE}.svc.cluster.local:9000 s3 mb s3://${BUCKET}"
kubectl_cmd wait pod/s3-init -n "$NAMESPACE" \
    --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s

log "Applying StorageClasses"
kubectl_cmd apply -f "${ROOT_DIR}/deploy/storageclasses.yaml"
kubectl_cmd get storageclass "$STORAGE_CLASS"

log "Running external storage e2e suite (this takes a while)"
# e2e.test has no --context flag: it reads the current context out of the
# kubeconfig.  Point it at a kubeconfig written for this cluster so it does not
# depend on whatever context happens to be selected on the host.
KUBECONFIG_FILE="${WORK_DIR}/kubeconfig"
kind get kubeconfig --name "$KIND_CLUSTER" >"$KUBECONFIG_FILE"
set +e
# shellcheck disable=SC2086
"$E2E_TEST_BIN" \
    -ginkgo.v \
    -ginkgo.no-color \
    -ginkgo.junit-report "$JUNIT_REPORT" \
    -ginkgo.focus "$E2E_FOCUS" \
    -ginkgo.skip "$E2E_SKIP" \
    -storage.testdriver "${ROOT_DIR}/test/e2e/driverinfo.yaml" \
    -kubeconfig "$KUBECONFIG_FILE" \
    -node-os-arch amd64 \
    -node-os-distro debian \
    $E2E_ARGS
RESULT=$?
set -e

if [ "$RESULT" -ne 0 ]; then
    dump_debug
fi

exit "$RESULT"
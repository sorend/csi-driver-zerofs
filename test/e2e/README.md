# Kubernetes external storage e2e

Runs the upstream Kubernetes [external storage e2e
suite](https://github.com/kubernetes/kubernetes/tree/master/test/e2e/storage/external)
against the ZeroFS CSI driver.

```bash
make e2e-test
```

This is a different test from `make sanity-test`. [csi-sanity](https://github.com/kubernetes-csi/csi-test)
drives the driver over gRPC the way a container orchestrator would and checks it
against the CSI specification. The suite here goes through the Kubernetes API
instead - PersistentVolumeClaims, pods, PersistentVolumes - and checks that the
driver behaves correctly once kubelet, the external-provisioner and the
scheduler are in the loop.

## How it is wired up

`e2e.test` is the generic Kubernetes test binary and knows nothing about the
driver. It only instantiates the storage test suites when handed a *driver
definition* through `-storage.testdriver`:

* `driverinfo.yaml` is that definition. It names the driver, points at the
  StorageClass to provision from (`FromExistingClassName: zerofs`, the NFS
  ReadWriteMany class) and declares which capabilities to exercise. Suites for
  capabilities that are absent are skipped rather than failed, so the file is
  where the driver's supported feature set is recorded. Each entry is annotated
  with the driver source that justifies it.
* `run-e2e.sh` builds the driver image, loads it into a throwaway kind cluster,
  deploys the controller, node DaemonSet and a RustFS backend, creates the S3
  bucket, and then runs the suite against `dns`-free local kubeconfig.

The cluster uses three workers on top of the control plane
(`kind-config.yaml`). The multi-node tests (`multiVolume`,
`volume-lifecycle-performance`, and the ones that recreate a pod pinned to a
different node) need somewhere to schedule onto; a single-node cluster would
skip the interesting half of the suite.

`e2e.test` is downloaded rather than built. For each Kubernetes release a
`kubernetes-test-linux-amd64.tarball` is published on `dl.k8s.io` that contains
a prebuilt `e2e.test`. The version has to match the kind node image, because the
binary embeds the feature gates and API machinery of the release it was built
from - `KIND_NODE_IMAGE` and `K8S_VERSION` exist for that and should be bumped
together. The download is cached in `E2E_BIN_DIR`.

## Skipped tests

The default `-ginkgo.skip` drops two classes of spec:

* `[Feature:...]` - alpha and beta feature tests. They need feature gates and
  CRDs the driver does not enable (VolumeAttributesClass, VolumeSnapshot,
  SELinux, VolumeSourceXFS). Upstream CI runs them in separate jobs.
* `[Disruptive]` - these stop the kubelet and would leave the throwaway cluster
  in a state where the remaining specs are meaningless.

Capabilities that are not claimed in `driverinfo.yaml` also cause their suites
to skip: block volumes, topology, volume limits, snapshots, single-node volumes,
ReadWriteOncePod and volume expansion. The last one is worth calling out:
`ControllerExpandVolume` is implemented and the `zerofs` StorageClass sets
`allowVolumeExpansion: true`, but the controller Deployment has no
`external-resizer` sidecar, so nothing ever calls it. The capability stays off
in the driver definition until the resizer is deployed.

## Requirements

The host kernel needs NFS client support (`nfs` module or built in). Almost
every spec in the suite mounts a real NFS export from a pod. Without it they
fail with:

```
mount.nfs: failed to prepare mount: No such device
```

GitHub Actions runners and most desktop kernels have it; minimal kernels without
loadable modules do not. This is the same constraint documented in
[test/sanity/README.md](../sanity/README.md).

## Knobs

| Variable | Purpose |
|----------|---------|
| `KIND_CLUSTER` | kind cluster name (default `zerofs-e2e`) |
| `KIND_NODE_IMAGE` | kind node image (default `kindest/node:v1.37.0`) |
| `K8S_VERSION` | `e2e.test` version to download (default `v1.37.0`) |
| `E2E_BIN_DIR` | where `e2e.test` is cached (default `/tmp/csi-e2e`) |
| `E2E_FOCUS` | replaces the default ginkgo focus |
| `E2E_SKIP` | replaces the default `--ginkgo.skip` pattern |
| `E2E_ARGS` | extra flags handed to `e2e.test` |
| `DRIVER_IMAGE` | driver image tag to build and load |
| `ZEROFS_IMAGE` | ZeroFS server image |
| `BUCKET` | S3 bucket backing the volumes |
| `WORK_DIR` | scratch dir for the junit report and kubeconfig |
| `KEEP_CLUSTER` | `1` keeps the cluster for debugging |

To run a single suite, narrow the focus. For example, everything under the
`subPath` suite:

```bash
E2E_FOCUS='External Storage \[Driver: zerofs\.csi\.sorend\.github\.com\] \[Testpattern: Dynamic PV \(default fs\)\] subPath' \
  make e2e-test
```

## Results

The junit report is written to `$WORK_DIR/junit_e2e.xml` and uploaded by CI.
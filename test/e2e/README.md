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
ReadWriteOncePod, fsGroup and volume expansion. The last two are worth calling
out:

* **fsGroup** - `CSIDriver` sets `fsGroupPolicy: File`, so kubelet does the work
  itself: it chowns the volume root to `root:<fsGroup>` and sets the setgid bit.
  That part works. The failure is in the ZeroFS server, which does not implement
  setgid gid inheritance when creating a *file*, so a new file lands `root:root`
  instead of inheriting the directory's gid. Confirmed directly against a
  mounted export:

  ```console
  # chown and setgid both work - the directory ends up drwxr-sr-x root:1000
  chown root:1000 dir && chmod g+s dir && ls -land dir
  # explicit chown works, so it is not a permission problem
  touch dir/explicit && chown 0:1000 dir/explicit && ls -land dir/explicit
  # inheritance is the broken part - should be gid 1000
  touch dir/inherited && ls -land dir/inherited
  ```

  Upstream ZeroFS implements the inheritance for directories but not for files,
  so this cannot be worked around from the driver. The fix is written and
  verified - all seven specs pass in 199s against a patched build, against 3
  failures on a stock image - see `FINDINGS.md` for the branch and the
  unit-level proof. It is still not in an upstream release as of ZeroFS 2.3.5,
  so it is not claimed here either.
* **Volume expansion** - `ControllerExpandVolume` is implemented and the
  `zerofs` StorageClass sets `allowVolumeExpansion: true`, but the controller
  Deployment has no `external-resizer` sidecar, so nothing ever calls it. Not
  claimed until the resizer is deployed.

## Runtime

A full run provisions a ZeroFS Deployment, Service and S3 prefix per volume and
waits for PV deletion after every spec, which lands somewhere between 20 and 40
minutes depending on the machine. `E2E_TIMEOUT` raises ginkgo's suite timeout
from its 1h default to 60m.

That headroom matters. A failing spec can also burn 20 minutes in cleanup: the
suite waits for the PV to be deleted while tearing the namespace down, and that
wait is the suite's own 20m limit rather than something the driver controls. PV
deletion itself is fast - a create/delete cycle against a running cluster takes
about ten seconds - so several stuck specs will burn the whole budget and the
run ends as a suite timeout rather than as the actual failures. When that
happens, read the individual `[FAILED]` specs above the timeout line rather than
the summary.

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
| `E2E_TIMEOUT` | ginkgo suite timeout (default `60m`; ginkgo's own default is 1h) |
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
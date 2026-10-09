# csi-sanity

Runs the [csi-sanity](https://github.com/kubernetes-csi/csi-test) suite against
the ZeroFS CSI driver.

## Running

```bash
make sanity-test
```

The target installs `csi-sanity` (`github.com/kubernetes-csi/csi-test/v5`) and
runs `test/sanity/run-sanity.sh`, which creates a throwaway kind cluster,
deploys the driver plus a RustFS backend, and then runs the suite against the
driver.  The cluster is deleted again unless `KEEP_CLUSTER=1` is set.

## How it is wired up

The driver needs a Kubernetes API to provision volumes, so it runs inside the
cluster, but `csi-sanity` runs on the host.  Three things bridge the gap:

* `plugin.yaml` runs a `zerofs-csi-sanity` pod that serves the identity,
  controller **and** node services on a single TCP endpoint
  (`csi-driver-zerofs sanity --endpoint=tcp://0.0.0.0:10000`).  The regular
  controller Deployment and node DaemonSet expose separate unix sockets that
  the host cannot reach.  The pod uses the node network so the endpoint is
  reachable without a hostPort, and reuses the `zerofs-csi-controller`
  ServiceAccount because it has to create the per-volume objects.
* The suite talks to that endpoint over `dns:///<node-ip>:10000`.
* Staging and target paths have to exist inside the plugin pod, so the path
  management flags (`--csi.createmountpathcmd` and friends) are pointed at
  generated `kubectl exec` shims.  This is the same technique the upstream csi
  prow jobs use.

`params.yaml` holds the `CreateVolume` parameters; they mirror the `zerofs`
StorageClass in `deploy/storageclasses.yaml`.

## Requirements

`NodeStageVolume` and `NodePublishVolume` tests mount a real NFS export from the
plugin pod, so the **host kernel needs NFS client support** (`nfs` module or it
built in).  Without it those tests fail with:

```
mount.nfs: failed to prepare mount: No such device
```

GitHub Actions runners and most desktop kernels have it; minimal kernels without
loadable modules do not.

## Knobs

| Variable | Purpose |
|----------|---------|
| `KIND_CLUSTER` | kind cluster name (default `zerofs-sanity`) |
| `KEEP_CLUSTER` | `1` keeps the cluster for debugging |
| `CSI_SANITY_BIN` | csi-sanity binary to run |
| `CSI_SANITY_ARGS` | extra csi-sanity flags, e.g. `--ginkgo.focus=CreateVolume` |
| `CSI_SANITY_SKIP` | replaces the default `--ginkgo.skip` pattern |

## Skipped tests

`NodeExpandVolume should fail when volume is not found` is skipped: the suite
sends the call without a capacity range and expects `NOT_FOUND`, while the CSI
specification asks for `INVALID_ARGUMENT` when the capacity range is missing,
and the stateless node service cannot know whether a volume exists.

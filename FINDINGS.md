# Findings

Issues found while running the Kubernetes external storage e2e suite against the
driver and investigating live-cluster failures. Not all of these are this
repository's to fix.

## 9P I/O errors with SQLite WAL (this repository)

**Root cause confirmed on 2026-11-27:** driver v0.2.0, ZeroFS 2.3.7, Linux
7.0.0-43-generic. This is a client mount configuration problem, not an observed
S3 failure or a ZeroFS server crash.

The node plugin mounted 9P without a `cache` option. Linux defaults to
`cache=none`, which disables shared writable memory mappings. SQLite WAL maps
its `-shm` file this way and reports the generic `disk I/O error` when mapping
fails; the extended error is `SQLITE_IOERR_SHMMAP` (5386). Plain file writes and
SQLite rollback journals can still work, masking the problem in smoke tests.

A second bug prevented configuring this through a StorageClass: the plugin
read only `volumeContext["mountOptions"]`, whereas Kubernetes supplies PV mount
options through `volumeCapability.mount.mountFlags`. Those flags were ignored
for both 9P and NFS.

### Controlled reproduction

Created a separate PVC using the installed `zerofs-ninep` class and mounted it
in a dedicated diagnostic pod. The original PostgreSQL and SQLite workloads
and their volumes were not modified. The same test server and backend were
used for both cases; only the client cache option changed.

| Check | Existing defaults | `cache=mmap` |
| --- | --- | --- |
| Shared writable 32 KiB mmap | `EINVAL` | PASS |
| SQLite WAL create/insert/commit | `SQLITE_IOERR_SHMMAP` | PASS |
| SQLite rollback-journal create/insert/commit | PASS | PASS |

The cluster's existing concurrent stress script (4 writers, 2 readers, 60s per
mode, `synchronous=FULL`, batches of 20 rows with 1 KiB payloads) passed on the
corrected mount:

- WAL: 204 commits, 4,080 rows, 18,779 reads, no busy retries.
- Rollback journal: 72 commits, 1,440 rows, 205 reads, 5 busy retries.
- Both passed the script's integrity, row-count and payload-checksum checks.

These are functional tests, not evidence of crash durability under node/server
failure. `cache=mmap` enables writeback data caching without opting into loose
metadata caching. Applications still need `fsync`, and independent client
mounts must not be treated as a coherent shared cache.

### Fix and regression coverage

- Default 9P mounts and the example StorageClass to `cache=mmap`.
- Merge defaults, legacy context options, and CSI mount flags, with the latter
  taking precedence by option key. Unspecified defaults such as the nonstandard
  9P port are preserved.
- Exercise actual `NodeStageVolume` mount arguments with fake-mounter tests.
- Add `make ninep-io-test` and use the same mmap/concurrent SQLite probe in the
  9P integration lifecycle tests. The probe uses and removes a unique directory.

An isolated copy of the patched node plugin was also run inside the diagnostic
pod and called over its CSI Unix socket, without registering it with kubelet:

- `NodeStageVolume` with no mount flags produced `cache=0x5` (`mmap`) and port
  5564 in `/proc/mounts`.
- CSI flags `msize=131072,cache=mmap` changed the actual mounted message size;
  mmap and SQLite WAL passed.
- An explicit CSI flag `cache=none` reproduced `EINVAL` and
  `SQLITE_IOERR_SHMMAP`, confirming that overrides now reach the kernel.
- Repeating each stage request succeeded without mounting it again.

All four checks in `make ninep-io-test` passed on the patched plugin's default
mount, including concurrent WAL and rollback-journal writes. `make fmt`,
`make test` (109 specs), `make vet`, and `make docker-build TAG=ninep-io-fix`
also passed. The full kind suites were not run locally.

The Linux cache modes are documented in
[v9fs mount options](https://docs.kernel.org/filesystems/9p.html#options).

### Rollout requirements and separate observations

Existing mounts keep their options when the node plugin is upgraded. Stop all
consumers of an affected PVC, let kubelet unpublish and unstage it, and restart
them after deploying the fixed driver. StorageClass edits alone do not update
existing PVs, and an explicit PV `cache=none` must be removed or changed.

The PostgreSQL pod also showed AppArmor-denied inter-process signals and
repeated startup/recovery cycles. This is separate evidence, not proof that
its restarts have the same cause as SQLite WAL; PostgreSQL was not changed as
part of this storage investigation.

All diagnostic mounts were unmounted, the test pod/PVC/PV and per-volume server
resources were deleted, and the diagnostic namespace was removed. The installed
driver and original workload resources were left unchanged; deploying the fix
is a separate rollout.

---

## 1. setgid gid inheritance is not applied when creating a file (upstream: ZeroFS)

**Status:** Fixed in ZeroFS 2.3.7 ([Barre/ZeroFS#646](https://github.com/Barre/ZeroFS/pull/646)).
The e2e suite enables `fsGroup` with this image. The results below document the
failure on 1.0.4 and the original verification against the patched fork.

### The fix

`mkdir` already had the right logic. The same check was missing from three
sibling paths in the same crate, so the fix is to apply the identical rule:

- `create.rs`, `create` - regular files
- `create.rs`, `mknod` - fifos, sockets, devices
- `link.rs`, `symlink`

Hard links are deliberately untouched: they share the source inode, so Linux
keeps the source's group rather than inheriting from the new parent. That is
covered by a regression test.

Regular files still do **not** inherit the setgid bit itself, only directories
do, matching Linux and the existing `mkdir` behaviour. An explicitly requested
gid still wins over inheritance.

### Verification

Unit level, in the fork:

- 8 new tests across `create.rs` and `link.rs`; full suite `866 passed, 0 failed`.
- Each inheritance test fails when its hunk is reverted (`left: 1000,
  right: 2000` - the caller's group instead of the parent's) and passes with it.
  They use a parent group of 2000 against a caller group of 1000 so inheritance
  is observable; had both been 1000 the tests would pass with or without the fix.

End to end, through real NFS and real kubelet `fsGroup` handling:

| ZeroFS image                    | fsGroup suite result                    |
| ------------------------------- | --------------------------------------- |
| `ghcr.io/barre/zerofs:1.0.4`    | 3 specs FAIL, ~20 min each in cleanup   |
| fork build with the fix         | `7 Passed, 0 Failed` in 199 seconds     |

The unpatched run does not merely fail the assertions, it burns 20 minutes per
spec in namespace teardown before reporting anything.

Note that the fork build is from `main` (v2.3.5), not the 1.0.4 release - those
are different builds and 1.0.4 has a different default cache path. The fix is
made against `main` since that is what an upstream change has to target;
backporting to 1.0.4 is separate work and was not done.

### What happened on 1.0.4

A pod with an `fsGroup` writes a file into the volume. The file comes out
`root:root` instead of inheriting the pod's fsGroup, so
`fsgroupchangepolicy` fails on the ownership assertion:

```
stdout: -rw-r--r-- 1 root root 0 /mnt/volume1/file1
expected gid: 1000
```

### Root cause

Not a driver or kubelet problem. With `fsGroupPolicy: File` kubelet does the work
itself and its half works correctly. The gap is in the ZeroFS server: it does
not implement setgid gid inheritance on file create.

Verified against a mounted export:

```console
# kubelet's half: chown and setgid both land on the directory
chown root:1000 dir && chmod g+s dir
ls -land dir
# drwxr-sr-x 2 0 1000 ... dir        <- correct

# explicit chown by root works, so this is not a permissions problem
touch dir/explicit && chown 0:1000 dir/explicit && ls -land dir/explicit
# -rw-r--r-- 1 0 1000 ... dir/explicit   <- correct

# inheritance is the broken part
touch dir/inherited && ls -land dir/inherited
# -rw-r--r-- 1 0    0 ... dir/inherited   <- should be gid 1000
```

The same gap applies to a subdirectory: a directory created under the setgid
parent does inherit `gid 1000`, but a file created inside that subdirectory does
not.

### Why it is inconsistent inside ZeroFS

`zerofs/src/fs/ops/create.rs` implements the inheritance in the `mkdir` path:

```rust
// create.rs, mkdir path
let parent_mode = dir.mode;
if parent_mode & 0o2000 != 0 {
    new_mode |= 0o2000;
}

let new_gid = match &attr.gid {
    SetGid::Set(g) => *g,
    SetGid::NoChange => {
        if parent_mode & 0o2000 != 0 {
            dir.gid          // <- inherits the parent's gid
        } else {
            creds.gid
        }
    }
};
```

The `create_file` path in the same file has no equivalent check and falls
straight through to the caller's credentials:

```rust
// create.rs, create_file path
gid: match &attr.gid {
    SetGid::Set(g) => *g,
    SetGid::NoChange => creds.gid,   // <- parent setgid ignored
},
```

For reference, `zerofs/src/fs/ops/setattr.rs` is not involved: it handles
`chmod`/`chown`/`truncate`, all of which behave correctly here. `chown` as uid 0
works, which is what rules out an authorization problem.

### Suggested fix

Mirror the `mkdir` logic in the `create_file` path: when the caller did not ask
for a specific gid and the parent directory has `0o2000` set, inherit
`dir.gid` and propagate the setgid bit into `final_mode`. This is what Linux and
POSIX do.

### Impact on users

Any workload relying on `fsGroup` on a ZeroFS volume. A pod declaring
`runAsGroup`/`fsGroup` gets files owned by `root:root`, so group-based
permissions do not apply. This is a fairly common Kubernetes pattern, so it is
worth reporting upstream.

### How it was found

The Kubernetes external storage e2e suite, added in
[#8](https://github.com/sorend/csi-driver-zerofs/pull/8). Reproduced both on CI
runners and locally once the host kernel had NFS support. Recorded in
`test/e2e/driverinfo.yaml` and `test/e2e/README.md` so it is not silently
skipped.

---

## 2. `controllerExpansion` cannot pass - no `external-resizer` sidecar (this repository)

**Severity:** Medium - silently disables a working code path.

**Status:** Open, fixable here.

`ControllerExpandVolume` is implemented and the `zerofs` StorageClass sets
`allowVolumeExpansion: true`, but the controller Deployment in
`deploy/install.yaml` runs only `csi-provisioner` and `liveness-probe` beside the
plugin. Nothing ever calls `ControllerExpandVolume`, so a PVC resize is accepted
by the API and then never acted on. The volume-expansion specs would pass or
fail misleadingly, so `controllerExpansion` is left `false` in
`test/e2e/driverinfo.yaml`.

**Fix:** add an `external-resizer` sidecar to the controller Deployment in
`deploy/install.yaml`, then set `controllerExpansion: true`.

---

## 3. A failing spec can consume the whole suite timeout (harness behaviour)

**Severity:** Low - affects debugging, not correctness.

**Status:** Documented in `test/e2e/README.md`.

While chasing finding 1, a full run reported `24 Passed | 5 Failed` *and* a suite
timeout at 105 minutes. The timeout was not a separate problem: all five
failures were the same fsGroup assertion, and each then burned 20 minutes in
namespace teardown, because the suite waits up to 20m for the PV to be deleted
before it gives up. That is roughly 80 of the 105 minutes.

PV deletion itself is healthy - a create/delete cycle against a live cluster
completes in about ten seconds:

```console
$ kubectl delete pvc deltest --wait=false
23:16:43 1: Released
23:16:53 2: GONE
```

`E2E_TIMEOUT` was lowered from 105m to 60m for this reason. The individual
`[FAILED]` specs above the timeout line are the real signal; the summary line at
the bottom is misleading.

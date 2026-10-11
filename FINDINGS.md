# Findings

Issues found while running the Kubernetes external storage e2e suite against the
driver. Not all of these are this repository's to fix.

---

## 1. setgid gid inheritance is not applied when creating a file (upstream: ZeroFS)

**Severity:** High - blocks the `fsGroup` capability entirely.

**Status:** Fixed in a fork ([sorend/ZeroFS](https://github.com/sorend/ZeroFS),
branch `fix/setgid-inheritance-on-create`) and verified end to end. Not yet in an
upstream release, so `test/e2e/driverinfo.yaml` still has `fsGroup: false`
against the default `ghcr.io/barre/zerofs:2.3.5` image.

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
| `ghcr.io/barre/zerofs:2.3.5`    | still unfixed, same 3 specs FAIL        |
| fork build with the fix         | `7 Passed, 0 Failed` in 199 seconds     |

The unpatched run does not merely fail the assertions, it burns 20 minutes per
spec in namespace teardown before reporting anything.

The fix is still not in an upstream release as of ZeroFS 2.3.5: the pull request
([barre/ZeroFS#646](https://github.com/Barre/ZeroFS/pull/646)) is open and
unmerged, and `zerofs/src/fs/ops/create.rs` is byte-identical between `main` and
the `v2.3.5` tag, so both still only honour the parent setgid in `mkdir`.

**Affects:** `test/e2e/driverinfo.yaml` (`fsGroup` is therefore `false`), which
makes the seven `fsgroupchangepolicy` specs skip.

### What happens

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
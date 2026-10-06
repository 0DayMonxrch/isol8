# isol8

A minimal Linux container runtime written from scratch in Go.

The goal of this project is to understand what tools like Docker and `runc` actually do under the hood. A container is not a lightweight VM; it is simply a standard Linux process isolated using kernel primitives: **namespaces**, **mount tables (`pivot_root`)**, and **cgroups**.

This repo documents my incremental progress building `isol8` step-by-step.

---

## The Mental Model (For Docker Users)

When you run:
```bash
docker run --rm -it alpine /bin/sh
```

Docker doesn't boot an OS. It sets up three main layers of isolation around a process:

| Docker Concept | Linux Kernel Primitive | What `isol8` Does |
| :--- | :--- | :--- |
| **Container Hostname** | UTS Namespace (`CLONE_NEWUTS`) | Assigns an isolated hostname (`container-root`) without touching the host. |
| **Container Rootfs (`/`)** | Mount Namespace (`CLONE_NEWNS`) + `pivot_root` | Unshares mount propagation (`MS_PRIVATE`), bind-mounts the rootfs, and pivots root to Alpine so the host filesystem is inaccessible. |
| **Container PID 1** | PID Namespace (`CLONE_NEWPID`) + fresh `/proc` | Isolates process IDs. The container command runs as PID 1, and `/proc` only displays container processes. |
| **Resource Limits** | cgroups v2 (`cpu`, `memory`, `pids`) | Subtree delegation, bounds fork count (`pids.max`), memory usage (`memory.max`), and CPU quota (`cpu.max`) before `execve`. |

---

## What's Built So Far

### Milestone 1: The Re-exec Skeleton
* **The Go Multithreading Problem**: Linux namespaces attach per OS thread. Go's runtime spins up multiple OS threads before `main()` starts. Calling `unshare()` directly inside a running Go program is non-deterministic because goroutines migrate across OS threads.
* **The Solution**: Re-executing `/proc/self/exe child ...` with `SysProcAttr.Cloneflags`. This ensures the child process and its fresh Go runtime are born directly inside the target namespaces from birth.

### Milestone 2: UTS & Mount Isolation (`pivot_root`)
* **Private Mount Propagation**: Made `/` recursively private (`MS_REC | MS_PRIVATE`) so mounts and unmounts do not leak to the host.
* **Self Bind-Mount**: Bind-mounted the rootfs onto itself to turn it into an official mount point (a strict prerequisite for `pivot_root`).
* **Atomic Pivot & Detach**: Called `pivot_root`, switched working directory to `/`, and lazily detached the old host root (`MNT_DETACH`).

### Milestone 3: PID Namespace & Process Model
* **PID 1 via `syscall.Exec`**: The child bootstrap process sets up isolation and replaces itself via `syscall.Exec` with the user payload. The user command becomes true **PID 1**, gaining native kernel signal immunity and automatic orphan child reaping.
* **Fresh `/proc`**: Mounted a fresh instance of `proc` to `rootfs/proc` so tools like `ps` only show processes inside the container.

### Milestone 4: Cgroups v2 Resource Isolation
* **Subtree Delegation**: Reads supported controllers from `cgroup.controllers` and delegates them sequentially through `cgroup.subtree_control` down to `/sys/fs/cgroup/isol8/<container_id>`.
* **Zero-Window Process Attachment**: Attaches the child process via `cgroup.procs` using PID `0` right before `pivot_root` and `syscall.Exec`, eliminating any unconstrained execution window at startup.
* **Configurable Limits & Bypass**: Supports `--pids-max`, `--memory-max`, `--cpu-max`, and `--insecure-no-pids-limit`.
* **Lifecycle Cleanup**: The parent process tracks container cgroups and cleanly executes `rmdir` upon child termination.

---

## Real Bugs & Gotchas I Hit While Building

Building this exposed several Linux kernel nuances that aren't obvious until you hit them:

1. **Mounting `/proc` after `pivot_root` gave `EPERM`**:
   * *What happened*: In user namespaces, mounting `proc` after pivoting and detaching the old root threw `Operation not permitted`.
   * *Why*: The Linux kernel security check (`mnt_already_visible` in `fs/namespace.c`) requires that existing proc mounts remain visible when mounting a new procfs in an unprivileged namespace.
   * *Fix*: Mounted `proc` to `rootfs/proc` *before* pivoting. When `pivot_root` runs, `rootfs/proc` cleanly transitions to `/proc`.

2. **Leaked Host File Descriptors**:
   * *What happened*: Inspecting `/proc/self/fd` inside the container showed 30+ open file descriptors from the parent terminal and editor.
   * *Why*: File descriptors without `O_CLOEXEC` remain open across `clone()` and `execve()`.
   * *Fix*: Added `closeNonStandardFds()` before `syscall.Exec` to close all descriptors `> 2`.

3. **Concurrency Race Condition on `.old_root`**:
   * *What happened*: Spawning 10+ containers concurrently caused race condition errors removing `.old_root`.
   * *Why*: All containers shared the same rootfs path and collided on the temporary directory name.
   * *Fix*: Made temporary old root paths unique per container (`.old_root_<pid>_<timestamp>`).

---

## Getting Started

### 1. Prerequisites
* Linux (kernel 5.x or newer)
* Go 1.22+
* `curl` and `tar`

### 2. Download and Unpack Alpine Rootfs
`isol8` needs a root filesystem layout (`/bin`, `/etc`, `/lib`, etc.) to pivot into. Download the minimal Alpine rootfs:

```bash
mkdir -p app/rootfs
curl -fsSL https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.0-x86_64.tar.gz | tar -xz -C app/rootfs
```

Ensure standard device placeholders exist:
```bash
touch app/rootfs/dev/null app/rootfs/dev/zero app/rootfs/dev/random app/rootfs/dev/urandom
```

### 3. Build the Runtime
```bash
go build -o isol8 main.go
```

### 4. Run a Container
Creating namespaces requires root privileges (`CAP_SYS_ADMIN`). You can run with `sudo` or unprivileged via user namespace mapping (`unshare -r`):

```bash
# Run an interactive shell
sudo ./isol8 run /bin/sh

# Or test directly with unshare -r:
unshare -r ./isol8 run /bin/sh
```

Inside the container:
```sh
# Hostname is isolated
hostname
# -> container-root

# Host files are gone; only Alpine rootfs is visible
cat /etc/os-release
# -> Alpine Linux v3.20

# Only container processes exist
ps aux
# -> PID 1 is /bin/sh
```

---

## Stress Testing & Validation

The project includes an automated test suite verifying namespace boundaries, mount leaks, and concurrency:

```bash
./test/run_stress_tests.sh
```

What the suite tests:
* **Scenario 1A**: Process visibility (`/proc` contains only local container PIDs).
* **Scenario 1B**: Zombie process accumulation (verifies orphaned children are reaped).
* **Scenario 1C**: Signal resistance (verifies kernel blocks unhandled `SIGTERM` sent to PID 1).
* **Scenario 2A**: Mount table isolation (tmpfs mount inside container does not leak to host).
* **Scenario 2B**: Relative path traversal containment (`cd ../../../..` cannot escape root).
* **Scenario 2C**: File descriptor leak check (`/proc/self/fd` contains no host descriptors).
* **Scenario 3A**: UTS hostname mutation (modifying hostname inside container does not mutate host).
* **Scenario 4**: Concurrency stress test (spawns 50 concurrent containers writing data simultaneously with 0 errors and 0 leftover mounts).

---

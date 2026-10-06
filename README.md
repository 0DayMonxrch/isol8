# isol8

A minimal, subtractive Linux container runtime written from scratch in Go.

`isol8` demonstrates how container runtimes (like Docker and `runc`) construct isolation using Linux kernel primitives: **namespaces**, **mount tables (`pivot_root`)**, and **cgroups v2**. A process begins with full access; `isol8` subtracts privileges, visibility, and resources until only the isolated container workload remains.

---

## Completed Architecture & Features

| Layer | Kernel Primitive | Implementation in `isol8` |
| :--- | :--- | :--- |
| **Re-exec Skeleton** | `/proc/self/exe` | Solves Go's multi-threaded runtime scheduler conflict by re-executing with `SysProcAttr.Cloneflags`. |
| **Hostname** | UTS Namespace (`CLONE_NEWUTS`) | Isolates hostname (`container-root`) without mutating the host. |
| **Mount & Rootfs** | Mount Namespace (`CLONE_NEWNS`) + `pivot_root` | Unshares mount propagation (`MS_PRIVATE`), bind-mounts rootfs, pivots to Alpine rootfs, and unmounts old host root with `MNT_DETACH`. |
| **Process Model** | PID Namespace (`CLONE_NEWPID`) + fresh `/proc` | User payload runs as true **PID 1** via `syscall.Exec` with automatic zombie reaping, kernel signal resistance, and private `/proc`. |
| **Resource Limits** | Cgroups v2 (`pids`, `memory`, `cpu`) + `CLONE_NEWCGROUP` | Full subtree controller delegation. Binds process using PID `0` before `execve` to eliminate startup race conditions. |
| **Hardening** | FD Cleanup | Automatically sets `FD_CLOEXEC` on all file descriptors `> 2` to prevent host descriptor inheritance. |
| **Lifecycle** | Automatic Cleanup | Automatically catches process exit and removes instance cgroups via `rmdir`. |

---

## Installation & Setup

### 1. Prerequisites
* Linux (Kernel 5.8+ with cgroups v2 enabled)
* Go 1.22+
* `curl` and `tar`

### 2. Download and Unpack Alpine Rootfs
```bash
mkdir -p app/rootfs
curl -fsSL https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.0-x86_64.tar.gz | tar -xz -C app/rootfs

# Ensure device nodes exist
touch app/rootfs/dev/null app/rootfs/dev/zero app/rootfs/dev/random app/rootfs/dev/urandom
```

### 3. Build isol8
```bash
go build -o isol8 main.go
```

---

## Usage

### Run an Isolated Shell
```bash
# With root privileges
sudo ./isol8 run /bin/sh

# Or unprivileged via user namespace mapping
unshare -r ./isol8 run /bin/sh
```

### Enforce Custom Resource Limits
```bash
sudo ./isol8 run \
  --pids-max 64 \
  --memory-max 524288000 \
  --cpu-max "50000 100000" \
  /bin/sh
```

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--pids-max <n>` | `64` | Maximum allowable processes in the container. |
| `--memory-max <bytes>` | `524288000` (500MB) | Hard memory limit; triggers OOM killer if exceeded. |
| `--cpu-max "<quota> <period>"` | `50000 100000` | CFS CPU bandwidth quota (microseconds). |
| `--insecure-no-pids-limit` | `max` | Disables PID bounding for threat modeling demos. |

---

## Verification & Test Suite

### 1. Run Automated Stress Tests
Run the comprehensive 11-scenario test suite verifying namespace boundaries, mount leaks, concurrency, and cgroup limits:
```bash
./test/run_stress_tests.sh
```

### 2. Fork Bomb Containment Test
Execute the fork bomb test script to verify that `pids.max` bounds process creation to 63 child processes with `EAGAIN` (`Resource temporarily unavailable`):
```bash
sudo ./isol8 run /test/exploits/fork_bomb.sh
```

---
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: isol8 run <command> [args...]\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		parent()
	case "child":
		child()
	default:
		fmt.Fprintf(os.Stderr, "isol8: unknown command %q (supported: run)\n", os.Args[1])
		os.Exit(1)
	}
}

func parent() {
	// Re-exec pattern:
	// Go's runtime creates multiple OS threads before main() runs.
	// Linux namespaces are attached per-thread, so calling unshare() in a multi-threaded
	// Go process causes non-deterministic thread/namespace mismatches.
	// Re-executing /proc/self/exe with Cloneflags ensures the child process and its Go runtime
	// are born cleanly inside the new namespaces from process creation (PID 1).
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, os.Args[2:]...)...)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Namespace isolation flags:
	// - CLONE_NEWUTS: isolate hostname and domain
	// - CLONE_NEWNS:  isolate mount namespace (private mount table)
	// - CLONE_NEWPID: isolate process IDs (child becomes PID 1)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
	}

	absRootfs, err := filepath.Abs("./app/rootfs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "isol8: error resolving rootfs path: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.Stat(absRootfs); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "isol8: rootfs directory not found at %s\n", absRootfs)
		fmt.Fprintf(os.Stderr, "Please unpack an Alpine minirootfs to ./app/rootfs first (see README.md)\n")
		os.Exit(1)
	}

	cmd.Env = append(os.Environ(), "ISOL8_ROOTFS="+absRootfs)

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "isol8: %v\n", err)
		os.Exit(1)
	}
}

func child() {
	rootfs := os.Getenv("ISOL8_ROOTFS")
	if rootfs == "" {
		fmt.Fprintf(os.Stderr, "isol8: missing ISOL8_ROOTFS configuration\n")
		os.Exit(1)
	}

	// 1. UTS isolation: Set container hostname
	hostname := os.Getenv("ISOL8_HOSTNAME")
	if hostname == "" {
		hostname = "container-root"
	}
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to set hostname: %v\n", err)
		os.Exit(1)
	}

	// 2. Prevent mount propagation back to the host (host systemd default is MS_SHARED)
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to make root private: %v\n", err)
		os.Exit(1)
	}

	// 3. Self bind-mount rootfs to satisfy pivot_root's mountpoint requirement
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to bind-mount rootfs: %v\n", err)
		os.Exit(1)
	}

	// 4. Mount fresh procfs instance in rootfs/proc before pivot_root
	// Linux requires parent /proc to be visible when mounting procfs in a user/PID namespace
	procDir := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(procDir, 0555); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to create /proc directory: %v\n", err)
		os.Exit(1)
	}
	if err := syscall.Mount("proc", procDir, "proc", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to mount /proc: %v\n", err)
		os.Exit(1)
	}

	// 5. Create unique temporary directory for the old host root to avoid collisions
	oldRootName := fmt.Sprintf(".old_root_%d_%d", os.Getpid(), time.Now().UnixNano())
	oldRoot := filepath.Join(rootfs, oldRootName)
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to create temporary old root directory: %v\n", err)
		os.Exit(1)
	}

	// 6. Pivot root filesystem
	if err := syscall.PivotRoot(rootfs, oldRoot); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: pivot_root failed: %v\n", err)
		os.Exit(1)
	}

	// 7. Change working directory into the new root
	if err := os.Chdir("/"); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to chdir to /: %v\n", err)
		os.Exit(1)
	}

	// 8. Detach and remove old host root mount
	oldRootPivoted := "/" + oldRootName
	if err := syscall.Unmount(oldRootPivoted, syscall.MNT_DETACH); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to unmount old root: %v\n", err)
		os.Exit(1)
	}
	if err := os.Remove(oldRootPivoted); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "isol8: failed to remove old root directory: %v\n", err)
		os.Exit(1)
	}

	// Resolve target binary path inside isolated container filesystem
	binPath, err := exec.LookPath(os.Args[2])
	if err != nil {
		binPath = os.Args[2]
	}

	// Set clean default environment for container processes
	env := []string{
		"PATH=/bin:/usr/bin:/sbin:/usr/sbin",
		"TERM=xterm",
		"HOME=/root",
	}

	// Close all inherited non-standard file descriptors (> 2) to eliminate host file leaks
	closeNonStandardFds()

	// Replace the bootstrap process with user command as true PID 1
	if err := syscall.Exec(binPath, os.Args[2:], env); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: execution of %s failed: %v\n", binPath, err)
		os.Exit(1)
	}
}

// closeNonStandardFds closes all file descriptors > 2 to prevent host fd leakage across execve.
func closeNonStandardFds() {
	fds, err := os.ReadDir("/proc/self/fd")
	if err == nil {
		for _, f := range fds {
			var fd int
			if _, err := fmt.Sscanf(f.Name(), "%d", &fd); err == nil {
				if fd > 2 {
					_ = syscall.Close(fd)
				}
			}
		}
	} else {
		for fd := 3; fd < 1024; fd++ {
			_ = syscall.Close(fd)
		}
	}
}

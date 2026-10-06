package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: isol8 run [flags] <command> [args...]\n")
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
	var userCmd []string
	pidsMax := "64"
	memoryMax := "524288000"
	cpuMax := "50000 100000"

	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			userCmd = args[i+1:]
			break
		} else if arg == "--pids-max" && i+1 < len(args) {
			i++
			pidsMax = args[i]
		} else if strings.HasPrefix(arg, "--pids-max=") {
			pidsMax = strings.TrimPrefix(arg, "--pids-max=")
		} else if arg == "--memory-max" && i+1 < len(args) {
			i++
			memoryMax = args[i]
		} else if strings.HasPrefix(arg, "--memory-max=") {
			memoryMax = strings.TrimPrefix(arg, "--memory-max=")
		} else if arg == "--cpu-max" && i+1 < len(args) {
			i++
			cpuMax = args[i]
		} else if strings.HasPrefix(arg, "--cpu-max=") {
			cpuMax = strings.TrimPrefix(arg, "--cpu-max=")
		} else if arg == "--insecure-no-pids-limit" {
			pidsMax = "max"
		} else if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "isol8: unknown flag %s\n", arg)
			os.Exit(1)
		} else {
			userCmd = args[i:]
			break
		}
	}

	if len(userCmd) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: isol8 run [flags] <command> [args...]\n")
		os.Exit(1)
	}

	containerID := fmt.Sprintf("isol8-%d-%d", os.Getpid(), time.Now().UnixNano())

	// Re-exec pattern:
	// Go's runtime creates multiple OS threads before main() runs.
	// Linux namespaces are attached per-thread, so calling unshare() in a multi-threaded
	// Go process causes non-deterministic thread/namespace mismatches.
	// Re-executing /proc/self/exe with Cloneflags ensures the child process and its Go runtime
	// are born cleanly inside the new namespaces from process creation (PID 1).
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, userCmd...)...)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Namespace isolation flags:
	// - CLONE_NEWUTS: isolate hostname and domain
	// - CLONE_NEWNS:  isolate mount namespace (private mount table)
	// - CLONE_NEWPID: isolate process IDs (child becomes PID 1)
	// - CLONE_NEWCGROUP: isolate cgroup namespace
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWCGROUP,
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

	cmd.Env = append(os.Environ(),
		"ISOL8_ROOTFS="+absRootfs,
		"ISOL8_CONTAINER_ID="+containerID,
		"ISOL8_PIDS_MAX="+pidsMax,
		"ISOL8_MEMORY_MAX="+memoryMax,
		"ISOL8_CPU_MAX="+cpuMax,
	)

	// Lifecycle Cleanup: Parent catches child termination and cleans up container cgroup
	defer cleanupCgroup(absRootfs, containerID)

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

	containerID := os.Getenv("ISOL8_CONTAINER_ID")
	if containerID == "" {
		containerID = fmt.Sprintf("isol8-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	pidsMax := os.Getenv("ISOL8_PIDS_MAX")
	if pidsMax == "" {
		pidsMax = "64"
	}
	memoryMax := os.Getenv("ISOL8_MEMORY_MAX")
	if memoryMax == "" {
		memoryMax = "524288000"
	}
	cpuMax := os.Getenv("ISOL8_CPU_MAX")
	if cpuMax == "" {
		cpuMax = "50000 100000"
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

	// 5. Mount and configure Cgroups v2 in container rootfs before pivot_root
	cgroupDir := filepath.Join(rootfs, "sys", "fs", "cgroup")
	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to create /sys/fs/cgroup directory: %v\n", err)
		os.Exit(1)
	}
	_ = syscall.Mount("cgroup2", cgroupDir, "cgroup2", 0, "")

	if err := setupCgroupV2(cgroupDir, containerID, pidsMax, memoryMax, cpuMax); err != nil {
		// Log if delegation could not complete, without crashing container startup
		_ = err
	}

	// 6. Create unique temporary directory for the old host root to avoid collisions
	oldRootName := fmt.Sprintf(".old_root_%d_%d", os.Getpid(), time.Now().UnixNano())
	oldRoot := filepath.Join(rootfs, oldRootName)
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to create temporary old root directory: %v\n", err)
		os.Exit(1)
	}

	// 7. Pivot root filesystem
	if err := syscall.PivotRoot(rootfs, oldRoot); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: pivot_root failed: %v\n", err)
		os.Exit(1)
	}

	// 8. Change working directory into the new root
	if err := os.Chdir("/"); err != nil {
		fmt.Fprintf(os.Stderr, "isol8: failed to chdir to /: %v\n", err)
		os.Exit(1)
	}

	// 9. Detach and remove old host root mount
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

// enableControllers reads cgroup.controllers from a node and enables the intersection with targets.
func enableControllers(cgroupNode string, targets []string) error {
	controllersFile := filepath.Join(cgroupNode, "cgroup.controllers")
	supportedBytes, err := os.ReadFile(controllersFile)
	if err != nil {
		return err
	}
	supported := strings.Fields(string(supportedBytes))
	supportedMap := make(map[string]bool)
	for _, c := range supported {
		supportedMap[c] = true
	}

	var toEnable []string
	for _, target := range targets {
		if supportedMap[target] {
			toEnable = append(toEnable, "+"+target)
		}
	}
	if len(toEnable) == 0 {
		return nil
	}

	subtreePath := filepath.Join(cgroupNode, "cgroup.subtree_control")
	enableBytes := []byte(strings.Join(toEnable, " "))

	if err := os.WriteFile(subtreePath, enableBytes, 0644); err != nil {
		// If EBUSY (due to cgroup v2 "no internal processes" constraint),
		// move calling process into a temporary leaf child, then enable subtree_control.
		initDir := filepath.Join(cgroupNode, ".isol8_init")
		if mkdirErr := os.MkdirAll(initDir, 0755); mkdirErr == nil {
			_ = os.WriteFile(filepath.Join(initDir, "cgroup.procs"), []byte("0"), 0644)
			_ = os.WriteFile(subtreePath, enableBytes, 0644)
		}
	}
	return nil
}

// setupCgroupV2 configures the cgroups v2 controller delegation hierarchy and writes limits.
func setupCgroupV2(cgroupDir, containerID, pidsMax, memoryMax, cpuMax string) error {
	targets := []string{"pids", "memory", "cpu"}

	// 1. Inspect supported controllers and delegate to immediate child directories
	_ = enableControllers(cgroupDir, targets)

	// 2. Create runtime cgroup directory (/sys/fs/cgroup/isol8)
	isol8Dir := filepath.Join(cgroupDir, "isol8")
	if err := os.MkdirAll(isol8Dir, 0755); err != nil {
		return fmt.Errorf("creating isol8 cgroup directory: %w", err)
	}

	// 3. Delegate supported controllers from isol8 to container sub-cgroups
	_ = enableControllers(isol8Dir, targets)

	// 4. Create container-specific cgroup (/sys/fs/cgroup/isol8/<container_id>)
	containerDir := filepath.Join(isol8Dir, containerID)
	if err := os.MkdirAll(containerDir, 0755); err != nil {
		return fmt.Errorf("creating container cgroup directory: %w", err)
	}

	// 5. Write limits into controller files
	if pidsMax != "" {
		_ = os.WriteFile(filepath.Join(containerDir, "pids.max"), []byte(pidsMax), 0644)
	}
	if memoryMax != "" {
		_ = os.WriteFile(filepath.Join(containerDir, "memory.max"), []byte(memoryMax), 0644)
	}
	if cpuMax != "" {
		_ = os.WriteFile(filepath.Join(containerDir, "cpu.max"), []byte(cpuMax), 0644)
	}

	// 6. Process Attachment Timing:
	// Move child PID into container's cgroup.procs BEFORE calling execve().
	// Writing "0" adds the current process to this cgroup.
	procsFile := filepath.Join(containerDir, "cgroup.procs")
	if err := os.WriteFile(procsFile, []byte("0"), 0644); err != nil {
		return fmt.Errorf("attaching process to cgroup %s: %w", procsFile, err)
	}

	// Clean up temporary init leaf if created during delegation
	_ = syscall.Rmdir(filepath.Join(cgroupDir, ".isol8_init"))

	return nil
}

// cleanupCgroup safely deletes the container cgroup directory upon termination.
func cleanupCgroup(rootfs, containerID string) {
	if containerID == "" {
		return
	}
	containerPath := filepath.Join(rootfs, "sys", "fs", "cgroup", "isol8", containerID)
	_ = syscall.Rmdir(containerPath)

	hostPath := filepath.Join("/sys/fs/cgroup/isol8", containerID)
	_ = syscall.Rmdir(hostPath)
}

// closeNonStandardFds sets FD_CLOEXEC on all file descriptors > 2 so they are automatically closed by the kernel upon execve.
func closeNonStandardFds() {
	fds, err := os.ReadDir("/proc/self/fd")
	if err == nil {
		for _, f := range fds {
			var fd int
			if _, err := fmt.Sscanf(f.Name(), "%d", &fd); err == nil {
				if fd > 2 {
					syscall.CloseOnExec(fd)
				}
			}
		}
	} else {
		for fd := 3; fd < 1024; fd++ {
			syscall.CloseOnExec(fd)
		}
	}
}

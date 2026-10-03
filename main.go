package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func main() {
	if len(os.Args) < 3 {
		panic("Not enough arguments. Usage: ./main run <cmd>")
	}

	switch os.Args[1] {
	case "run":
		parent()
	case "child":
		child()
	default:
		panic("Bad command")
	}
}

func parent() {
	// re-exec pattern
	// we pass "child" as the first argument so the new process knows its role.
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, os.Args[2:]...)...)

	// wire up std streams so we can see output
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// CLONE_NEWUTS: isolate hostname
	// CLONE_NEWNS: isolate mount namespace
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS,
	}

	absRootfs, err := filepath.Abs("./app/rootfs")
	if err != nil {
		fmt.Printf("Error resolving rootfs path: %v\n", err)
		os.Exit(1)
	}

	// INJECTION: Pass the absolute rootfs config to the child via Env vars
	cmd.Env = append(os.Environ(), "ISOL8_ROOTFS="+absRootfs)

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func child() {
	// EXTRACTION: Read the config passed from the parent
	rootfs := os.Getenv("ISOL8_ROOTFS")
	fmt.Printf("[Child] Bootstrapping container using rootfs: %s\n", rootfs)

	// Set container-specific hostname in isolated UTS namespace
	if err := syscall.Sethostname([]byte("container-root")); err != nil {
		fmt.Printf("Error setting hostname: %v\n", err)
		os.Exit(1)
	}

	// 1. Prevent mount propagation back to the host by making root private
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fmt.Printf("Error setting mount propagation to private: %v\n", err)
		os.Exit(1)
	}

	// 2. Bind-mount the rootfs onto itself so it becomes a valid mountpoint for pivot_root
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		fmt.Printf("Error bind mounting rootfs: %v\n", err)
		os.Exit(1)
	}

	// 3. Create temporary directory to hold the old host root
	oldRoot := filepath.Join(rootfs, ".old_root")
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		fmt.Printf("Error creating .old_root directory: %v\n", err)
		os.Exit(1)
	}

	// 4. Pivot the root filesystem
	if err := syscall.PivotRoot(rootfs, oldRoot); err != nil {
		fmt.Printf("Error pivot_root: %v\n", err)
		os.Exit(1)
	}

	// 5. Change working directory to the new root
	if err := os.Chdir("/"); err != nil {
		fmt.Printf("Error chdir to /: %v\n", err)
		os.Exit(1)
	}

	// 6. Unmount old root with MNT_DETACH and remove temporary directory
	if err := syscall.Unmount("/.old_root", syscall.MNT_DETACH); err != nil {
		fmt.Printf("Error unmounting .old_root: %v\n", err)
		os.Exit(1)
	}
	if err := os.Remove("/.old_root"); err != nil {
		fmt.Printf("Error removing .old_root directory: %v\n", err)
		os.Exit(1)
	}

	// os.Args[2] is the actual command the user wants to run (e.g., "/bin/sh")
	// os.Args[3:] are the arguments to that command
	cmd := exec.Command(os.Args[2], os.Args[3:]...)

	// wire up std streams so we can see output
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

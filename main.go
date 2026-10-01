package main

import (
	"fmt"
	"os"
	"os/exec"
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

	// INJECTION: Pass the rootfs config to the child via Env vars
	cmd.Env = append(os.Environ(), "ISOL8_ROOTFS=./app/rootfs")

	if err := cmd.Run(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func child() {
	// EXTRACTION: Read the config passed from the parent
	rootfs := os.Getenv("MINICON_ROOTFS")
	fmt.Printf("[Child] Bootstrapping container using rootfs: %s\n", rootfs)

	// os.Args[2] is the actual command the user wants to run (e.g., "echo")
	// os.Args[3:] are the arguments to that command (e.g., "hello")
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

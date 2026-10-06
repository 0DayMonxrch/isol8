package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

func main() {
	var mu sync.Mutex

	// background goroutine to simulate time window
	go func() {
		fmt.Println("[Background Goroutine] Locking the mutex...")
		mu.Lock()

		time.Sleep(10 * time.Second)

		mu.Unlock()
		fmt.Println("[Background Goroutine] Unlocked the mutex.")
	}()

	time.Sleep(1 * time.Second)

	fmt.Println("[Main Thread] calling SYS_FORK...")

	// r1 will hold the PID of the child. In the child process, r1 will be 0.
	r1, _, err := syscall.RawSyscall(syscall.SYS_FORK, 0, 0, 0)
	if err != 0 {
		fmt.Printf("Fork failed: %v\n", err)
		os.Exit(1)
	}

	if r1 == 0 {
		// child
		fmt.Println("[Child] I am the child. Attempting to acquire the lock...")

		// Deadlock
		// The child inherited a memory state where 'mu' is currently LOCKED.
		// But the goroutine that was supposed to unlock it was destroyed by the fork.
		mu.Lock()

		fmt.Println("[Child] I got the lock! (You will never see this line)")
		os.Exit(0)
	} else {
		// parent
		fmt.Printf("[Parent] Spawned child PID %d.\n", r1)
		time.Sleep(3 * time.Second)
		fmt.Printf("[Parent] Notice how the child (PID %d) is permanently hung?\n", r1)
	}
}

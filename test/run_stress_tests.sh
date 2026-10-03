#!/usr/bin/env bash
set -euo pipefail

# isol8 stress test suite
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "=== Building isol8 binary ==="
go build -o isol8 main.go
echo "Build complete."

PASSED=0
FAILED=0

assert_success() {
  local name="$1"
  shift
  echo -n "[TEST] $name ... "
  if "$@"; then
    echo "PASSED"
    PASSED=$((PASSED + 1))
  else
    echo "FAILED"
    FAILED=$((FAILED + 1))
  fi
}

echo ""
echo "=== 1. PID Namespace & Procfs Stress Testing ==="

# Scenario 1A: /proc leak & visibility test
test_1a() {
  local output
  output=$(unshare -r ./isol8 run /bin/sh -c 'ls -d /proc/[0-9]*' | grep -v '\[Child\]')
  # Should only have PID 1
  local pids
  pids=$(echo "$output" | tr -s ' ' '\n' | grep -v '^$' | sed 's#/proc/##')
  for p in $pids; do
    if [ "$p" -gt 20 ]; then
      echo "Host PID $p leaked into container!"
      return 1
    fi
  done
  return 0
}
assert_success "Scenario 1A: /proc process visibility isolation" test_1a

# Scenario 1B: Zombie process accumulation / reaping
test_1b() {
  local output
  output=$(unshare -r ./isol8 run /bin/sh -c '
    for i in $(seq 1 50); do
      (exit 0) &
    done
    wait
    ps aux
  ' | grep -v '\[Child\]')
  if echo "$output" | grep -q "defunct"; then
    echo "Defunct zombie processes detected!"
    return 1
  fi
  return 0
}
assert_success "Scenario 1B: Zombie process accumulation and reaping" test_1b

# Scenario 1C: PID 1 signal resistance
test_1c() {
  local output
  output=$(unshare -r ./isol8 run /bin/sh -c '
    (sleep 0.05 && kill -TERM 1) &
    sleep 0.2
    echo "SURVIVED"
  ' | grep -v '\[Child\]')
  echo "$output" | grep -q "SURVIVED"
}
assert_success "Scenario 1C: PID 1 signal resistance to unhandled SIGTERM" test_1c

echo ""
echo "=== 2. Mount Namespace & Filesystem Escape Testing ==="

# Scenario 2A: Cross-namespace mount leakage
test_2a() {
  local before_mounts after_mounts
  before_mounts=$(cat /proc/mounts | grep -c '/mnt' || true)
  
  unshare -r ./isol8 run /bin/sh -c '
    mount -t tmpfs tmpfs /mnt
    echo "secret_canary" > /mnt/canary.txt
  ' > /dev/null

  after_mounts=$(cat /proc/mounts | grep -c '/mnt' || true)
  if [ "$before_mounts" -ne "$after_mounts" ]; then
    echo "Mount leaked into host /proc/mounts!"
    return 1
  fi
  if [ -f "/mnt/canary.txt" ]; then
    echo "File leaked into host /mnt!"
    return 1
  fi
  return 0
}
assert_success "Scenario 2A: Cross-namespace mount leakage" test_2a

# Scenario 2B: Relative path traversal escape
test_2b() {
  local output
  output=$(unshare -r ./isol8 run /bin/sh -c '
    cd ../../../../../../../..
    pwd
    ls home 2>&1 || true
  ' | grep -v '\[Child\]')
  if echo "$output" | grep -q "/home/dibyo"; then
    echo "Host home directory exposed!"
    return 1
  fi
  echo "$output" | grep -q "^/"
}
assert_success "Scenario 2B: Relative path traversal escape (/.. containment)" test_2b

# Scenario 2C: File descriptor inheritance leak
test_2c() {
  local fds
  fds=$(unshare -r ./isol8 run /bin/sh -c 'ls /proc/self/fd' | grep -v '\[Child\]' | tr '\n' ' ')
  for fd in $fds; do
    # Only standard fds (0, 1, 2) and the directory listing fd (e.g. 3) should exist
    if [ "$fd" -gt 3 ]; then
      echo "Host file descriptor $fd leaked into container!"
      return 1
    fi
  done
  return 0
}
assert_success "Scenario 2C: Host file descriptor leak prevention" test_2c

echo ""
echo "=== 3. UTS Namespace Stress Testing ==="

# Scenario 3A: Hostname mutation collision
test_3a() {
  local host_before host_after container_hn
  host_before=$(hostname)
  container_hn=$(unshare -r env ISOL8_HOSTNAME=container-test-box ./isol8 run /bin/hostname | grep -v '\[Child\]')
  host_after=$(hostname)

  if [ "$host_before" != "$host_after" ]; then
    echo "Host hostname was mutated!"
    return 1
  fi
  if [ "$container_hn" != "container-test-box" ]; then
    echo "Container failed to set custom hostname: $container_hn"
    return 1
  fi
  return 0
}
assert_success "Scenario 3A: UTS Hostname mutation collision" test_3a

echo ""
echo "=== 4. Multi-Container Concurrency & Namespace Collision Test ==="

# Scenario 4: Concurrency load test (50 concurrent instances)
test_4() {
  local count=50
  local pids=()
  local mount_count_before mount_count_after

  mount_count_before=$(wc -l < /proc/mounts)

  echo "Spawning $count concurrent container instances..."
  for i in $(seq 1 "$count"); do
    (
      unshare -r env ISOL8_HOSTNAME="concurrent-worker-$i" ./isol8 run /bin/sh -c '
        # 1. Verify unique hostname
        if [ "$(hostname)" != "'concurrent-worker-$i'" ]; then
          echo "Hostname mismatch in worker '$i'"
          exit 1
        fi
        # 2. Mount private tmpfs
        mount -t tmpfs -o size=50M tmpfs /mnt
        # 3. Write dummy data
        yes "isol8-stress-test-data-block-0123456789" | head -c 10485760 > /mnt/dummy.bin
        # Verify write
        if [ ! -s /mnt/dummy.bin ]; then
          echo "Failed dummy write in worker '$i'"
          exit 1
        fi
      ' > /dev/null
    ) &
    pids+=($!)
  done

  # Wait for all background containers
  local failed=0
  for pid in "${pids[@]}"; do
    if ! wait "$pid"; then
      failed=$((failed + 1))
    fi
  done

  if [ "$failed" -ne 0 ]; then
    echo "$failed / $count concurrent containers failed!"
    return 1
  fi

  mount_count_after=$(wc -l < /proc/mounts)
  if [ "$mount_count_before" -ne "$mount_count_after" ]; then
    echo "Mount count mismatch: before=$mount_count_before, after=$mount_count_after"
    return 1
  fi

  echo "All $count concurrent containers exited cleanly with zero mount leaks."
  return 0
}
assert_success "Scenario 4: 50 concurrent containers with private mounts and writes" test_4

echo ""
echo "=========================================="
echo "Test Summary: $PASSED passed, $FAILED failed"
echo "=========================================="

if [ "$FAILED" -ne 0 ]; then
  exit 1
fi

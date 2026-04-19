// Package pidutil contains small process helpers shared across GC packages.
package pidutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Alive reports whether a PID exists and is not a zombie.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if state, ok := procProcessState(pid); ok {
		return state != "Z"
	}
	if state, ok := psProcessState(pid); ok {
		return !strings.HasPrefix(state, "Z")
	}
	return true
}

func procProcessState(pid int) (string, bool) {
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return "", false
	}
	return fields[2], true
}

func psProcessState(pid int) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

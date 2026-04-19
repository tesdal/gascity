package pidutil

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestAliveTreatsZombieAsDead(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin":
	default:
		t.Skip("zombie detection is only covered on Linux and macOS")
	}
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("ps"); err != nil {
			t.Skip("ps not installed")
		}
	}

	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(cmd.Process.Pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Alive(%d) stayed true for exited child", cmd.Process.Pid)
}

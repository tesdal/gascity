//go:build (linux && !android) || (darwin && !ios)

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProductMetricsTaggedBinaryProcessContracts(t *testing.T) {
	skipSlowCmdGCTest(t, "builds and executes a tagged gc binary")
	configureProductMetricsTrustedProcessTempRoot(t)

	taggedBinary := filepath.Join(t.TempDir(), "gc-productmetrics-tagged")
	buildGCBinaryForProductMetricsTest(t, taggedBinary, "productmetrics_testhook")

	t.Run("private uploader bypasses normal startup", func(t *testing.T) {
		runProductMetricsPrivateUploaderProcessContract(t, taggedBinary)
	})
	t.Run("control flow bypasses city and pack state", func(t *testing.T) {
		workingDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workingDir, "city.toml"), []byte("invalid = [\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		holdProductMetricsPackCacheLock(t, home)
		runProductMetricsTaggedControlFlow(t, taggedBinary, workingDir, home)
	})
}

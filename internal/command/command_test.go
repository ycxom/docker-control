package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controller "github.com/ycxom/docker-control/internal/controller"
)

func TestConfigCommandCreatesSelfContainedState(t *testing.T) {
	home := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exit := Run([]string{"config", "--home", home, "--token", "test-secret"}, "test", &stdout, &stderr)

	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	layout, _ := ResolveLayout(home)
	for _, path := range []string{layout.Config, layout.Marker} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected generated file %s: %v", path, err)
		}
	}
	if strings.Contains(stdout.String(), "test-secret") {
		t.Fatal("config command exposed the management token")
	}
}

func TestSystemdUnitRunsServerFromLocalState(t *testing.T) {
	layout, _ := ResolveLayout(t.TempDir())
	unit := systemdUnit(layout)
	if !strings.Contains(unit, "server --home") || !strings.Contains(unit, ".docker-control") {
		t.Fatalf("unit does not use fused server command: %s", unit)
	}
	if strings.Contains(unit, "/usr/local/lib") || strings.Contains(unit, "/etc/docker-control") {
		t.Fatalf("unit escaped the local deployment directory: %s", unit)
	}
}

func TestSafeRemoveStateRequiresMatchingMarker(t *testing.T) {
	home := t.TempDir()
	layout, _ := ResolveLayout(home)
	if err := os.MkdirAll(layout.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := safeRemoveState(layout); err == nil {
		t.Fatal("state without a marker was removed")
	}
	cfg := controller.Config{BearerToken: "token", ListenAddress: ":16544", DockerSocket: "/var/run/docker.sock", Image: "ubuntu:22.04", PullMissingImage: true, AllowNetwork: true, SandboxNetwork: "bridge", PublicEndpoint: "auto", MemoryBytes: 1, NanoCPUs: 1, PIDsLimit: 1, WorkspaceBytes: 1, ExecutionTimeout: 1, MaxOutputBytes: 1, MaxFileBytes: 1, MaxContainers: 1, TerminalIdleTimeout: 1, MaxTerminalWindows: 1, ContainerNamePrefix: "sandbox"}
	if err := ensureLocalFiles(layout, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.State, "owned"), []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := safeRemoveState(layout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.State); !os.IsNotExist(err) {
		t.Fatal("marked state directory was not removed")
	}
}

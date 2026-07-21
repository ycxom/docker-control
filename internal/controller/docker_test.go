package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenericLabelsMapToSandbox(t *testing.T) {
	item := dockerContainerSummary{
		ID: "id", Image: "python:test", State: "running", Names: []string{"/sandbox-demo"},
		Labels: map[string]string{
			managedLabel: "true", keyLabel: "generic-key", endpointLabel: "ws://generic",
		},
	}
	sandbox := item.container()
	if sandbox.Key != "generic-key" || sandbox.Name != "sandbox-demo" || sandbox.ControlledEndpoint != "ws://generic" {
		t.Fatalf("generic labels were not mapped: %#v", sandbox)
	}
}

func TestSandboxUsesDockerDefaultPermissionsWithoutPrivilegedMode(t *testing.T) {
	host := sandboxHostConfig(CreateSpec{NetworkMode: "sandbox", WorkspaceBytes: 1024})

	if privileged, ok := host["Privileged"].(bool); !ok || privileged {
		t.Fatalf("Privileged = %#v, want false", host["Privileged"])
	}
	if _, exists := host["CapDrop"]; exists {
		t.Fatal("CapDrop must be omitted so apt and general-purpose software can use Docker's default capabilities")
	}
	if _, exists := host["SecurityOpt"]; exists {
		t.Fatal("SecurityOpt must not force no-new-privileges for general-purpose software")
	}
}

func TestContainerReportsLogicalImageAfterSnapshotRestore(t *testing.T) {
	item := dockerContainerSummary{Image: "docker-control-snapshot:key-id", Labels: map[string]string{imageLabel: "ubuntu:22.04"}}
	if got := item.container().Image; got != "ubuntu:22.04" {
		t.Fatalf("logical image = %q, want ubuntu:22.04", got)
	}
}

func TestSnapshotLabelChangeEscapesValues(t *testing.T) {
	got := dockerLabelChange(snapshotNameLabel, `before "upgrade"`)
	want := `LABEL io.github.ycxom.docker-control.snapshot-name="before \"upgrade\""`
	if got != want {
		t.Fatalf("label change = %q, want %q", got, want)
	}
}

func TestCreateSnapshotCapturesWorkspaceAndCommitsImage(t *testing.T) {
	var committed atomic.Bool
	var archiveCopied atomic.Bool
	var snapshotID string
	engineServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/images/json":
			if !committed.Load() {
				_, _ = response.Write([]byte("[]"))
				return
			}
			payload := []dockerImageSummary{{
				ID: "sha256:snapshot", RepoTags: []string{"docker-control-snapshot:sandbox01-" + snapshotID},
				Created: time.Now().Unix(), Size: 2048,
				Labels: map[string]string{snapshotLabel: "true", snapshotKeyLabel: "sandbox01", snapshotIDLabel: snapshotID, snapshotNameLabel: "before-upgrade", snapshotSourceLabel: "ubuntu:22.04"},
			}}
			_ = json.NewEncoder(response).Encode(payload)
		case request.URL.Path == "/containers/json":
			_, _ = response.Write([]byte(`[{"Id":"container-id","Image":"ubuntu:22.04","State":"running","Status":"Up (healthy)","Labels":{"io.github.ycxom.docker-control.managed":"true","io.github.ycxom.docker-control.key":"sandbox01","io.github.ycxom.docker-control.image":"ubuntu:22.04"}}]`))
		case request.URL.Path == "/containers/container-id/json":
			_, _ = response.Write([]byte(`{"State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`))
		case request.URL.Path == "/containers/container-id/exec":
			_, _ = response.Write([]byte(`{"Id":"exec-id"}`))
		case request.URL.Path == "/exec/exec-id/start":
			response.Header().Set("Content-Type", "application/octet-stream")
		case request.URL.Path == "/exec/exec-id/json":
			_, _ = response.Write([]byte(`{"ExitCode":0}`))
		case request.URL.Path == "/containers/container-id/archive" && request.Method == http.MethodGet:
			response.Header().Set("Content-Type", "application/x-tar")
			_, _ = response.Write([]byte("workspace-archive"))
		case request.URL.Path == "/containers/container-id/archive" && request.Method == http.MethodPut:
			payload, _ := io.ReadAll(request.Body)
			archiveCopied.Store(string(payload) == "workspace-archive" && request.URL.Query().Get("path") == "/.docker-control-snapshot")
		case request.URL.Path == "/commit":
			tag := request.URL.Query().Get("tag")
			parts := strings.Split(tag, "-")
			snapshotID = parts[len(parts)-1]
			changes := strings.Join(request.URL.Query()["changes"], "\n")
			if !strings.Contains(changes, snapshotLabel) || !strings.Contains(changes, snapshotSourceLabel) {
				t.Fatalf("commit labels = %s", changes)
			}
			committed.Store(true)
			_, _ = response.Write([]byte(`{"Id":"sha256:snapshot"}`))
		default:
			t.Fatalf("unexpected Docker endpoint: %s %s", request.Method, request.URL.String())
		}
	}))
	defer engineServer.Close()
	engine := NewDockerEngine(engineServer.URL, engineServer.Client())

	snapshot, err := engine.CreateSnapshot(context.Background(), "sandbox01", "before-upgrade", 10)

	if err != nil {
		t.Fatal(err)
	}
	if !archiveCopied.Load() || snapshot.ID != snapshotID || snapshot.SourceImage != "ubuntu:22.04" {
		t.Fatalf("archiveCopied=%v snapshot=%#v generatedID=%s", archiveCopied.Load(), snapshot, snapshotID)
	}
}

func TestLegacyLabelsRemainReadable(t *testing.T) {
	item := dockerContainerSummary{
		ID: "id", Labels: map[string]string{
			legacyManagedLabel: "true", legacyKeyLabel: "legacy-key",
			legacyEndpointLabel: "ws://legacy",
		},
	}
	sandbox := item.container()
	if sandbox.Key != "legacy-key" || sandbox.ControlledEndpoint != "ws://legacy" {
		t.Fatalf("legacy labels were not mapped: %#v", sandbox)
	}
}

func TestSummaryReadinessStates(t *testing.T) {
	tests := []struct {
		state, status, readiness string
		ready                    bool
	}{
		{"running", "Up 1 second (healthy)", "ready", true},
		{"running", "Up 1 second (health: starting)", "starting", false},
		{"running", "Up 1 second (unhealthy)", "unhealthy", false},
		{"exited", "Exited (1)", "stopped", false},
	}
	for _, test := range tests {
		ready, readiness := summaryReadiness(test.state, test.status)
		if ready != test.ready || readiness != test.readiness {
			t.Errorf("%s/%s = %v/%s", test.state, test.status, ready, readiness)
		}
	}
}

func TestExecRejectsSandboxThatIsStillStarting(t *testing.T) {
	engineServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/containers/json":
			_, _ = response.Write([]byte(`[{"Id":"id","Image":"ubuntu:22.04","State":"running","Status":"Up (health: starting)","Labels":{"io.github.ycxom.docker-control.managed":"true","io.github.ycxom.docker-control.key":"sandbox01"}}]`))
		case "/containers/id/json":
			_, _ = response.Write([]byte(`{"State":{"Status":"running","Running":true,"Health":{"Status":"starting"}}}`))
		default:
			t.Fatalf("unexpected Docker endpoint: %s", request.URL.Path)
		}
	}))
	defer engineServer.Close()
	engine := NewDockerEngine(engineServer.URL, engineServer.Client())

	_, err := engine.Exec(context.Background(), "sandbox01", ExecRequest{Command: []string{"sh", "-lc", "true"}, WorkDir: "/workspace"}, time.Second, 1024)

	if !errors.Is(err, ErrSandboxNotReady) {
		t.Fatalf("Exec error = %v, want ErrSandboxNotReady", err)
	}
}

func TestEnsureStartsMissingImagePullWithoutBlockingRequest(t *testing.T) {
	pullStarted := make(chan struct{})
	releasePull := make(chan struct{})
	var imageReady atomic.Bool
	engineServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/containers/json":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte("[]"))
		case request.URL.Path == "/images/ubuntu:22.04/json":
			response.Header().Set("Content-Type", "application/json")
			if imageReady.Load() {
				_, _ = response.Write([]byte("{}"))
				return
			}
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"message":"not found"}`))
		case request.URL.Path == "/images/create":
			close(pullStarted)
			<-releasePull
			imageReady.Store(true)
			_, _ = response.Write([]byte("{}\n"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer engineServer.Close()
	engine := NewDockerEngine(engineServer.URL, engineServer.Client())
	started := time.Now()
	_, _, err := engine.Ensure(context.Background(), CreateSpec{Key: "sandbox01", Name: "sandbox-sandbox01", Image: "ubuntu:22.04"}, true, 20)

	if !errors.Is(err, ErrImagePreparing) {
		t.Fatalf("Ensure error = %v, want ErrImagePreparing", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Ensure blocked on image pull for %s", elapsed)
	}
	select {
	case <-pullStarted:
	case <-time.After(time.Second):
		t.Fatal("background image pull did not start")
	}
	close(releasePull)
}

func TestImageSwitchPreservesOldContainerUntilNewImageIsReady(t *testing.T) {
	pullStarted := make(chan struct{})
	releasePull := make(chan struct{})
	var deleted atomic.Bool
	engineServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/containers/json":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`[{"Id":"old","Image":"ubuntu:22.04","State":"running","Labels":{"io.github.ycxom.docker-control.managed":"true","io.github.ycxom.docker-control.key":"sandbox01"}}]`))
		case request.URL.Path == "/images/debian:12-slim/json":
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"message":"not found"}`))
		case request.URL.Path == "/images/create":
			close(pullStarted)
			<-releasePull
			_, _ = response.Write([]byte("{}\n"))
		case request.Method == http.MethodDelete:
			deleted.Store(true)
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer engineServer.Close()
	engine := NewDockerEngine(engineServer.URL, engineServer.Client())

	_, _, err := engine.Ensure(context.Background(), CreateSpec{Key: "sandbox01", Name: "sandbox-sandbox01", Image: "debian:12-slim"}, true, 20)

	if !errors.Is(err, ErrImagePreparing) {
		t.Fatalf("Ensure error = %v, want ErrImagePreparing", err)
	}
	if deleted.Load() {
		t.Fatal("old container was deleted before the new image became ready")
	}
	select {
	case <-pullStarted:
	case <-time.After(time.Second):
		t.Fatal("new image pull did not start")
	}
	close(releasePull)
}

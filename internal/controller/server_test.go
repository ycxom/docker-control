package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type fakeEngine struct {
	spec      CreateSpec
	container Container
	removed   bool
}

type preparingFakeEngine struct{ *fakeEngine }

func (f *preparingFakeEngine) PrepareImage(context.Context, string, bool) (bool, error) {
	return false, nil
}

func TestManagementTerminalStreamsOutputOverWebSocket(t *testing.T) {
	engine := &fakeEngine{container: Container{ID: "id", Key: "abcdef12", State: "running"}}
	server := httptest.NewServer(NewServer(testConfig(), engine))
	defer server.Close()
	conn, reader := dialTestWebSocket(t, server.URL, "/v1/sandboxes/abcdef12/terminal", map[string]string{"Authorization": "Bearer manager-secret"})
	defer conn.Close()

	ready := readTestJSONFrame(t, reader)
	if ready["type"] != "ready" {
		t.Fatalf("first event = %#v", ready)
	}
	writeTestJSONFrame(t, conn, map[string]any{"type": "exec", "request_id": "r1", "command": "printf ok"})
	started := readTestJSONFrame(t, reader)
	output := readTestJSONFrame(t, reader)
	exit := readTestJSONFrame(t, reader)
	if started["type"] != "started" || output["type"] != "output" || exit["type"] != "exit" {
		t.Fatalf("events = %#v %#v %#v", started, output, exit)
	}
	decoded, err := base64.StdEncoding.DecodeString(output["data"].(string))
	if err != nil || string(decoded) != "ok" {
		t.Fatalf("output = %q, %v", decoded, err)
	}
}

func TestIdleTerminalWindowIsReclaimed(t *testing.T) {
	cfg := testConfig()
	cfg.TerminalIdleTimeout = 50 * time.Millisecond
	engine := &fakeEngine{container: Container{ID: "id", Key: "abcdef12", State: "running"}}
	server := httptest.NewServer(NewServer(cfg, engine))
	defer server.Close()
	conn, reader := dialTestWebSocket(t, server.URL, "/v1/sandboxes/abcdef12/terminal", map[string]string{"Authorization": "Bearer manager-secret"})
	defer conn.Close()
	_ = readTestJSONFrame(t, reader)
	reclaimed := readTestJSONFrame(t, reader)
	if reclaimed["type"] != "reclaimed" {
		t.Fatalf("event = %#v", reclaimed)
	}
}

func TestListenWithFallbackUsesNextPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	if port == 65535 {
		t.Skip("cannot test fallback from the last port")
	}
	listener, err := ListenWithFallback(fmt.Sprintf("127.0.0.1:%d", port), 20)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if got := listener.Addr().(*net.TCPAddr).Port; got <= port || got > port+20 {
		t.Fatalf("fallback port = %d, want a port in %d..%d", got, port+1, port+20)
	}
}

func TestGenericEnvironmentNamesOverrideLegacyAliases(t *testing.T) {
	t.Setenv("DOCKER_CONTROL_TOKEN", "generic-token")
	t.Setenv("CONTROLLER_TOKEN", "legacy-token")
	t.Setenv("DOCKER_CONTROL_IMAGE", "generic:image")
	t.Setenv("SANDBOX_IMAGE", "legacy:image")
	t.Setenv("CONTAINER_NAME_PREFIX", "sandbox")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BearerToken != "generic-token" || cfg.Image != "generic:image" || cfg.ContainerNamePrefix != "sandbox" {
		t.Fatalf("generic environment was not preferred: %#v", cfg)
	}
}

func dialTestWebSocket(t *testing.T, serverURL, requestPath string, headers map[string]string) (net.Conn, *bufio.Reader) {
	t.Helper()
	target, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", requestPath, target.Host, key)
	for name, value := range headers {
		_, _ = fmt.Fprintf(conn, "%s: %s\r\n", name, value)
	}
	_, _ = io.WriteString(conn, "\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		t.Fatalf("upgrade status = %d", response.StatusCode)
	}
	return conn, reader
}

func readTestJSONFrame(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatal(err)
	}
	length := int(header[1] & 0x7f)
	if length == 126 {
		var extended uint16
		if err := binary.Read(reader, binary.BigEndian, &extended); err != nil {
			t.Fatal(err)
		}
		length = int(extended)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func writeTestJSONFrame(t *testing.T, conn net.Conn, value any) {
	t.Helper()
	payload, _ := json.Marshal(value)
	mask := []byte{1, 2, 3, 4}
	masked := make([]byte, len(payload))
	for index := range payload {
		masked[index] = payload[index] ^ mask[index%4]
	}
	header := []byte{0x81, 0x80 | byte(len(payload))}
	if len(payload) >= 126 {
		t.Fatal("test payload unexpectedly large")
	}
	_, _ = conn.Write(append(append(header, mask...), masked...))
}

func (f *fakeEngine) Version(context.Context) (string, error)   { return "test", nil }
func (f *fakeEngine) List(context.Context) ([]Container, error) { return []Container{f.container}, nil }
func (f *fakeEngine) Get(_ context.Context, session string) (Container, error) {
	if f.container.Key != session {
		return Container{}, ErrNotFound
	}
	return f.container, nil
}
func (f *fakeEngine) Ensure(_ context.Context, spec CreateSpec, _ bool, _ int) (Container, bool, error) {
	f.spec = spec
	f.container = Container{ID: "container-id", Name: spec.Name, Key: spec.Key, Image: spec.Image, State: "running", Ready: true, Readiness: "ready", ControlledEndpoint: spec.ControlledEndpoint}
	return f.container, true, nil
}
func (f *fakeEngine) Remove(context.Context, string) (bool, error) {
	f.removed = true
	return true, nil
}
func (f *fakeEngine) Exec(context.Context, string, ExecRequest, time.Duration, int) (ExecResult, error) {
	return ExecResult{ExitCode: 0, Output: "ok"}, nil
}
func (f *fakeEngine) ExecStream(_ context.Context, _ string, _ ExecRequest, _ time.Duration, _ int, output func([]byte) error) (ExecResult, error) {
	if output != nil {
		_ = output([]byte("ok"))
	}
	return ExecResult{ExitCode: 0, Output: "ok"}, nil
}
func (f *fakeEngine) WriteFile(context.Context, string, string, []byte) error { return nil }
func (f *fakeEngine) ReadFile(context.Context, string, string, int64) ([]byte, bool, error) {
	return []byte("file"), false, nil
}
func (f *fakeEngine) VerifyControlledToken(_ context.Context, session, token string) (bool, error) {
	return session == f.container.Key && controlledTokenHash(token) == f.spec.ControlledTokenSHA256, nil
}

func testConfig() Config {
	return Config{
		BearerToken: "manager-secret", Image: "ubuntu:22.04", PublicEndpoint: "ws://controller:16544",
		SandboxNetwork: "sandbox", MemoryBytes: 1024, NanoCPUs: 1000, PIDsLimit: 16,
		WorkspaceBytes: 2048, ExecutionTimeout: time.Second, MaxOutputBytes: 1024,
		MaxFileBytes: 1024, MaxContainers: 3,
		TerminalIdleTimeout: time.Minute, MaxTerminalWindows: 3,
		ContainerNamePrefix: "sandbox",
	}
}

func TestManagementRoutesRequireBearerToken(t *testing.T) {
	handler := NewServer(testConfig(), &fakeEngine{})
	request := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestEnsureInjectsNarrowControlledEndpoint(t *testing.T) {
	engine := &fakeEngine{}
	handler := NewServer(testConfig(), engine)
	request := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/abcdef12", bytes.NewBufferString("{}"))
	request.Header.Set("Authorization", "Bearer manager-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if engine.spec.ControlledEndpoint != "ws://controller:16544/v1/controlled/abcdef12/terminal" {
		t.Fatalf("unexpected endpoint: %s", engine.spec.ControlledEndpoint)
	}
	if engine.spec.Name != "sandbox-abcdef12" || engine.spec.Key != "abcdef12" {
		t.Fatalf("generic sandbox identity was not applied: %#v", engine.spec)
	}
	if engine.spec.ControlledToken == "" || engine.spec.ControlledTokenSHA256 != controlledTokenHash(engine.spec.ControlledToken) {
		t.Fatal("controlled token was not generated and hashed")
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["controlled_token"] != engine.spec.ControlledToken {
		t.Fatal("initial response did not return the injected per-container token")
	}
}

func TestEnsureUsesTrustedRequestedImage(t *testing.T) {
	engine := &fakeEngine{}
	handler := NewServer(testConfig(), engine)
	request := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/abcdef12", bytes.NewBufferString(`{"image":"debian:12-slim"}`))
	request.Header.Set("Authorization", "Bearer manager-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || engine.spec.Image != "debian:12-slim" {
		t.Fatalf("status=%d image=%q body=%s", response.Code, engine.spec.Image, response.Body.String())
	}
}

func TestOpenAPIAndCapabilitiesArePublic(t *testing.T) {
	handler := NewServer(testConfig(), &fakeEngine{})
	for _, path := range []string{"/openapi.yaml", "/v1/capabilities"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
}

func TestLegacyContainerRouteRemainsAnAlias(t *testing.T) {
	engine := &fakeEngine{container: Container{ID: "id", Key: "abcdef12", State: "running"}}
	handler := NewServer(testConfig(), engine)
	request := httptest.NewRequest(http.MethodGet, "/v1/containers/abcdef12", nil)
	request.Header.Set("Authorization", "Bearer manager-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("legacy alias status = %d: %s", response.Code, response.Body.String())
	}
}

func TestRebuildDeletesAndRecreatesSandbox(t *testing.T) {
	engine := &fakeEngine{container: Container{ID: "old", Key: "abcdef12", State: "running", Ready: true, Readiness: "ready"}}
	handler := NewServer(testConfig(), engine)
	request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/abcdef12/rebuild", bytes.NewBufferString(`{"image":"debian:12-slim"}`))
	request.Header.Set("Authorization", "Bearer manager-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !engine.removed || engine.spec.Key != "abcdef12" || engine.spec.Image != "debian:12-slim" {
		t.Fatalf("rebuild status=%d removed=%v spec=%#v body=%s", response.Code, engine.removed, engine.spec, response.Body.String())
	}
}

func TestRebuildPreservesSandboxWhileImageIsPreparing(t *testing.T) {
	base := &fakeEngine{container: Container{ID: "old", Key: "abcdef12", Image: "ubuntu:22.04", State: "running"}}
	handler := NewServer(testConfig(), &preparingFakeEngine{fakeEngine: base})
	request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/abcdef12/rebuild", bytes.NewBufferString(`{"image":"debian:12-slim"}`))
	request.Header.Set("Authorization", "Bearer manager-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable || base.removed {
		t.Fatalf("status=%d removed=%v body=%s", response.Code, base.removed, response.Body.String())
	}
}

func TestControlledEndpointRejectsManagerTokenAndAcceptsContainerToken(t *testing.T) {
	engine := &fakeEngine{}
	handler := NewServer(testConfig(), engine)
	ensure := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/abcdef12", bytes.NewBufferString("{}"))
	ensure.Header.Set("Authorization", "Bearer manager-secret")
	handler.ServeHTTP(httptest.NewRecorder(), ensure)

	bad := httptest.NewRequest(http.MethodGet, "/v1/controlled/abcdef12", nil)
	bad.Header.Set("X-Controlled-Docker-Token", "manager-secret")
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusUnauthorized {
		t.Fatalf("manager token status = %d, want 401", badResponse.Code)
	}

	good := httptest.NewRequest(http.MethodGet, "/v1/controlled/abcdef12", nil)
	good.Header.Set("X-Controlled-Docker-Token", engine.spec.ControlledToken)
	goodResponse := httptest.NewRecorder()
	handler.ServeHTTP(goodResponse, good)
	if goodResponse.Code != http.StatusOK {
		t.Fatalf("controlled endpoint status = %d: %s", goodResponse.Code, goodResponse.Body.String())
	}
}

func TestSafeWorkspacePath(t *testing.T) {
	for _, unsafe := range []string{"", "/etc/passwd", "../secret", "a/../../b", "a//b"} {
		if _, err := safeWorkspacePath(unsafe); err == nil {
			t.Errorf("safeWorkspacePath(%q) accepted unsafe path", unsafe)
		}
	}
	if got, err := safeWorkspacePath("src/app.py"); err != nil || got != "/workspace/src/app.py" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestReadDockerStreamBoundsMultiplexedOutput(t *testing.T) {
	var stream bytes.Buffer
	for _, frame := range []string{"hello", " world"} {
		header := make([]byte, 8)
		header[0] = 1
		binary.BigEndian.PutUint32(header[4:], uint32(len(frame)))
		stream.Write(header)
		stream.WriteString(frame)
	}
	output, truncated, err := readDockerStream(&stream, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "hello wo" || !truncated {
		t.Fatalf("output=%q truncated=%v", output, truncated)
	}
}

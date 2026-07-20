package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Server struct {
	cfg       Config
	engine    Engine
	mux       *http.ServeMux
	terminals *TerminalHub
}

func NewServer(cfg Config, engine Engine) http.Handler {
	server := &Server{cfg: cfg, engine: engine, mux: http.NewServeMux(), terminals: NewTerminalHub(cfg.TerminalIdleTimeout, cfg.MaxTerminalWindows)}
	server.mux.HandleFunc("GET /v1/health", server.health)
	server.mux.HandleFunc("GET /v1/capabilities", server.capabilities)
	server.mux.HandleFunc("GET /openapi.yaml", server.openAPISpec)
	server.mux.HandleFunc("GET /v1/sandboxes", server.listContainers)
	server.mux.HandleFunc("PUT /v1/sandboxes/{key}", server.ensureContainer)
	server.mux.HandleFunc("GET /v1/sandboxes/{key}", server.getContainer)
	server.mux.HandleFunc("DELETE /v1/sandboxes/{key}", server.deleteContainer)
	server.mux.HandleFunc("POST /v1/sandboxes/{key}/rebuild", server.rebuildContainer)
	server.mux.HandleFunc("GET /v1/sandboxes/{key}/terminal", server.managementTerminal)
	server.mux.HandleFunc("PUT /v1/sandboxes/{key}/files", server.writeFile)
	server.mux.HandleFunc("GET /v1/sandboxes/{key}/files", server.readFile)
	// v2 compatibility aliases. New integrations should use /v1/sandboxes.
	server.mux.HandleFunc("GET /v1/containers", server.listContainers)
	server.mux.HandleFunc("PUT /v1/containers/{key}", server.ensureContainer)
	server.mux.HandleFunc("GET /v1/containers/{key}", server.getContainer)
	server.mux.HandleFunc("DELETE /v1/containers/{key}", server.deleteContainer)
	server.mux.HandleFunc("POST /v1/containers/{key}/rebuild", server.rebuildContainer)
	server.mux.HandleFunc("GET /v1/containers/{key}/terminal", server.managementTerminal)
	server.mux.HandleFunc("PUT /v1/containers/{key}/files", server.writeFile)
	server.mux.HandleFunc("GET /v1/containers/{key}/files", server.readFile)
	server.mux.HandleFunc("GET /v1/controlled/{key}", server.controlledEndpoint)
	server.mux.HandleFunc("GET /v1/controlled/{key}/terminal", server.controlledTerminal)
	return server
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/v1/controlled/") || request.URL.Path == "/v1/health" || request.URL.Path == "/v1/capabilities" || request.URL.Path == "/openapi.yaml" {
		s.mux.ServeHTTP(response, request)
		return
	}
	if !secureEqual(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), s.cfg.BearerToken) {
		writeError(response, http.StatusUnauthorized, "valid bearer token required")
		return
	}
	s.mux.ServeHTTP(response, request)
}

func (s *Server) capabilities(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]any{
		"name": "docker-control", "api_version": "v1",
		"resources":         []string{"sandboxes", "files", "terminal"},
		"terminal_protocol": "docker-control-terminal-v1",
		"legacy_routes":     []string{"/v1/containers"},
	})
}

func (s *Server) openAPISpec(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(openAPISpec)
}

func (s *Server) health(response http.ResponseWriter, request *http.Request) {
	version, err := s.engine.Version(request.Context())
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, err.Error())
		return
	}
	imageStatus := "unknown"
	if preparer, ok := s.engine.(interface {
		PrepareImage(context.Context, string, bool) (bool, error)
	}); ok {
		ready, prepareErr := preparer.PrepareImage(request.Context(), s.cfg.Image, s.cfg.PullMissingImage)
		if prepareErr != nil {
			imageStatus = "error: " + prepareErr.Error()
		} else if ready {
			imageStatus = "ready"
		} else {
			imageStatus = "preparing"
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"status": "ok", "docker_version": version, "image": s.cfg.Image,
		"image_status":          imageStatus,
		"terminal_idle_seconds": int(s.cfg.TerminalIdleTimeout.Seconds()),
		"max_terminal_windows":  s.cfg.MaxTerminalWindows,
	})
}

func (s *Server) listContainers(response http.ResponseWriter, request *http.Request) {
	containers, err := s.engine.List(request.Context())
	if err != nil {
		writeEngineError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"sandboxes": containers, "containers": containers})
}

func (s *Server) ensureContainer(response http.ResponseWriter, request *http.Request) {
	key := request.PathValue("key")
	image, err := s.requestedImage(response, request)
	if err != nil {
		return
	}
	container, created, token, err := s.ensureSandbox(request.Context(), key, image)
	if err != nil {
		writeEngineError(response, err)
		return
	}
	payload := map[string]any{"sandbox": container, "container": container, "created": created}
	if created {
		payload["controlled_token"] = token
	}
	writeJSON(response, http.StatusOK, payload)
}

func (s *Server) ensureSandbox(ctx context.Context, key, image string) (Container, bool, string, error) {
	if !validKey(key) {
		return Container{}, false, "", ErrInvalidSandboxKey
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Container{}, false, "", errors.New("cannot generate controlled endpoint token")
	}
	token := hex.EncodeToString(tokenBytes)
	endpoint := endpointFor(s.cfg.PublicEndpoint, key)
	healthEndpoint := healthEndpointFor(s.cfg.PublicEndpoint, key)
	container, created, err := s.engine.Ensure(ctx, CreateSpec{
		Key: key, Name: s.cfg.ContainerNamePrefix + "-" + key, Image: image,
		NetworkMode: s.cfg.SandboxNetwork, MemoryBytes: s.cfg.MemoryBytes,
		NanoCPUs: s.cfg.NanoCPUs, PIDsLimit: s.cfg.PIDsLimit,
		WorkspaceBytes: s.cfg.WorkspaceBytes, ControlledEndpoint: endpoint,
		ControlledHealthEndpoint: healthEndpoint,
		ControlledToken:          token, ControlledTokenSHA256: controlledTokenHash(token),
	}, s.cfg.PullMissingImage, s.cfg.MaxContainers)
	if err != nil {
		return Container{}, false, "", err
	}
	return container, created, token, nil
}

func (s *Server) rebuildContainer(response http.ResponseWriter, request *http.Request) {
	key := request.PathValue("key")
	image, err := s.requestedImage(response, request)
	if err != nil {
		return
	}
	if preparer, ok := s.engine.(interface {
		PrepareImage(context.Context, string, bool) (bool, error)
	}); ok {
		ready, prepareErr := preparer.PrepareImage(request.Context(), image, s.cfg.PullMissingImage)
		if prepareErr != nil {
			writeEngineError(response, prepareErr)
			return
		}
		if !ready {
			writeEngineError(response, fmt.Errorf("%w: %s; current sandbox was preserved", ErrImagePreparing, image))
			return
		}
	}
	removed, err := s.engine.Remove(request.Context(), key)
	if err != nil {
		writeEngineError(response, err)
		return
	}
	container, created, token, err := s.ensureSandbox(request.Context(), key, image)
	if err != nil {
		writeEngineError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"sandbox": container, "container": container, "created": created,
		"rebuilt": removed, "controlled_token": token,
	})
}

func (s *Server) requestedImage(response http.ResponseWriter, request *http.Request) (string, error) {
	image := s.cfg.Image
	if request.Body != nil && request.ContentLength != 0 {
		var payload CreateRequest
		decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
			writeError(response, http.StatusBadRequest, "invalid sandbox request: "+err.Error())
			return "", err
		}
		if strings.TrimSpace(payload.Image) != "" {
			image = strings.TrimSpace(payload.Image)
		}
	}
	if !validImage(image) {
		err := errors.New("image must be 1-255 characters without control characters")
		writeError(response, http.StatusBadRequest, err.Error())
		return "", err
	}
	return image, nil
}

func (s *Server) getContainer(response http.ResponseWriter, request *http.Request) {
	container, err := s.engine.Get(request.Context(), request.PathValue("key"))
	if err != nil {
		writeEngineError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"sandbox": container, "container": container})
}

func (s *Server) deleteContainer(response http.ResponseWriter, request *http.Request) {
	removed, err := s.engine.Remove(request.Context(), request.PathValue("key"))
	if err != nil {
		writeEngineError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) managementTerminal(response http.ResponseWriter, request *http.Request) {
	s.terminals.Serve(response, request, request.PathValue("key"), s.engine, s.cfg)
}

func (s *Server) writeFile(response http.ResponseWriter, request *http.Request) {
	path, err := safeWorkspacePath(request.URL.Query().Get("path"))
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	reader := http.MaxBytesReader(response, request.Body, s.cfg.MaxFileBytes)
	payload, err := io.ReadAll(reader)
	if err != nil {
		writeError(response, http.StatusRequestEntityTooLarge, "file exceeds configured limit")
		return
	}
	if err := s.engine.WriteFile(request.Context(), request.PathValue("key"), path, payload); err != nil {
		writeEngineError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"path": path, "bytes": len(payload)})
}

func (s *Server) readFile(response http.ResponseWriter, request *http.Request) {
	path, err := safeWorkspacePath(request.URL.Query().Get("path"))
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	payload, truncated, err := s.engine.ReadFile(request.Context(), request.PathValue("key"), path, s.cfg.MaxFileBytes)
	if err != nil {
		writeEngineError(response, err)
		return
	}
	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("X-Content-Truncated", fmt.Sprint(truncated))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(payload)
}

func (s *Server) controlledEndpoint(response http.ResponseWriter, request *http.Request) {
	key := request.PathValue("key")
	container, err := s.engine.Get(request.Context(), key)
	if err != nil {
		writeEngineError(response, err)
		return
	}
	provided := request.Header.Get("X-Controlled-Docker-Token")
	// Get returns only public data, so token verification is delegated to a
	// narrow optional interface without exposing Docker-wide credentials.
	verifier, ok := s.engine.(interface {
		VerifyControlledToken(context.Context, string, string) (bool, error)
	})
	if !ok {
		writeError(response, http.StatusNotImplemented, "controlled endpoint verification unavailable")
		return
	}
	valid, err := verifier.VerifyControlledToken(request.Context(), key, provided)
	if err != nil || !valid {
		writeError(response, http.StatusUnauthorized, "invalid controlled endpoint token")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"key": container.Key, "container_id": container.ID,
		"state": container.State, "ready": container.Ready, "readiness": container.Readiness,
		"capabilities": []string{"identity", "health", "terminal", "rebuild"},
	})
}

func (s *Server) controlledTerminal(response http.ResponseWriter, request *http.Request) {
	key := request.PathValue("key")
	if !s.verifyControlledRequest(response, request, key) {
		return
	}
	s.terminals.Serve(response, request, key, s.engine, s.cfg)
}

func (s *Server) verifyControlledRequest(response http.ResponseWriter, request *http.Request, key string) bool {
	verifier, ok := s.engine.(interface {
		VerifyControlledToken(context.Context, string, string) (bool, error)
	})
	if !ok {
		writeError(response, http.StatusNotImplemented, "controlled endpoint verification unavailable")
		return false
	}
	valid, err := verifier.VerifyControlledToken(request.Context(), key, request.Header.Get("X-Controlled-Docker-Token"))
	if err != nil || !valid {
		writeError(response, http.StatusUnauthorized, "invalid controlled endpoint token")
		return false
	}
	return true
}

func decodeJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1024*1024))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, map[string]any{
		"error":   map[string]any{"code": http.StatusText(status), "message": message, "status": status},
		"message": message,
	})
}

func writeEngineError(response http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, ErrInvalidSandboxKey) {
		status = http.StatusBadRequest
	} else if errors.Is(err, ErrImagePreparing) || errors.Is(err, ErrSandboxNotReady) {
		status = http.StatusServiceUnavailable
	}
	writeError(response, status, err.Error())
}

func escapedPath(value string) string { return url.PathEscape(value) }

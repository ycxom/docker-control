package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type TerminalHub struct {
	idle       time.Duration
	maxWindows int64
	open       atomic.Int64
}

type terminalCommand struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Command   string `json:"command"`
	WorkDir   string `json:"workdir"`
}

func NewTerminalHub(idle time.Duration, maxWindows int) *TerminalHub {
	return &TerminalHub{idle: idle, maxWindows: int64(maxWindows)}
}

func (hub *TerminalHub) Serve(response http.ResponseWriter, request *http.Request, session string, engine Engine, cfg Config) {
	if !hub.acquire() {
		writeError(response, http.StatusServiceUnavailable, "terminal window limit reached")
		return
	}
	defer hub.open.Add(-1)
	ws, err := upgradeWebSocket(response, request)
	if err != nil {
		writeError(response, http.StatusBadRequest, err.Error())
		return
	}
	defer ws.close(1000, "terminal closed")
	windowID := randomIdentifier(12)
	if err := ws.writeJSON(map[string]any{
		"type": "ready", "window_id": windowID,
		"idle_timeout_seconds": int(hub.idle.Seconds()),
		"protocol":             "docker-control-terminal-v1",
	}); err != nil {
		return
	}
	for {
		_ = ws.conn.SetReadDeadline(time.Now().Add(hub.idle))
		payload, err := ws.readText(1024 * 1024)
		if err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				_ = ws.writeJSON(map[string]any{"type": "reclaimed", "reason": "idle timeout"})
			}
			return
		}
		var command terminalCommand
		if err := json.Unmarshal(payload, &command); err != nil {
			_ = ws.writeJSON(map[string]any{"type": "error", "error": "invalid command JSON"})
			continue
		}
		if command.Type == "ping" {
			_ = ws.writeJSON(map[string]any{"type": "pong"})
			continue
		}
		if command.Type == "close" {
			return
		}
		if command.Type != "exec" || strings.TrimSpace(command.Command) == "" {
			_ = ws.writeJSON(map[string]any{"type": "error", "request_id": command.RequestID, "error": "type=exec and command are required"})
			continue
		}
		workdir, err := safeTerminalWorkdir(command.WorkDir)
		if err != nil {
			_ = ws.writeJSON(map[string]any{"type": "error", "request_id": command.RequestID, "error": err.Error()})
			continue
		}
		if command.RequestID == "" {
			command.RequestID = randomIdentifier(8)
		}
		_ = ws.conn.SetReadDeadline(time.Time{})
		if err := ws.writeJSON(map[string]any{"type": "started", "request_id": command.RequestID}); err != nil {
			return
		}
		result, err := engine.ExecStream(
			request.Context(), session,
			ExecRequest{Command: []string{"sh", "-lc", command.Command}, WorkDir: workdir},
			cfg.ExecutionTimeout, cfg.MaxOutputBytes,
			func(chunk []byte) error {
				return ws.writeJSON(map[string]any{
					"type": "output", "request_id": command.RequestID,
					"encoding": "base64", "data": base64.StdEncoding.EncodeToString(chunk),
				})
			},
		)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				return
			}
			if writeErr := ws.writeJSON(map[string]any{"type": "error", "request_id": command.RequestID, "error": err.Error()}); writeErr != nil {
				return
			}
			continue
		}
		if err := ws.writeJSON(map[string]any{
			"type": "exit", "request_id": command.RequestID,
			"exit_code": result.ExitCode, "truncated": result.Truncated, "timed_out": result.TimedOut,
		}); err != nil {
			return
		}
	}
}

func (hub *TerminalHub) acquire() bool {
	for {
		current := hub.open.Load()
		if current >= hub.maxWindows {
			return false
		}
		if hub.open.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func safeTerminalWorkdir(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "/workspace", nil
	}
	if value == "/workspace" {
		return value, nil
	}
	if !strings.HasPrefix(value, "/workspace/") || strings.Contains(value, "..") || strings.Contains(value, "//") {
		return "", errors.New("workdir must stay inside /workspace")
	}
	return value, nil
}

func randomIdentifier(bytesCount int) string {
	payload := make([]byte, bytesCount)
	if _, err := rand.Read(payload); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:bytesCount*2]
	}
	return hex.EncodeToString(payload)
}

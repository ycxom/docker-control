package controller

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("invalid env file line: %q", line)
		}
		name = strings.TrimSpace(name)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if _, exists := os.LookupEnv(name); !exists {
			if err := os.Setenv(name, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

type RuntimeInfo struct {
	ListenAddress      string `json:"listen_address"`
	ManagementEndpoint string `json:"management_ws_endpoint"`
	Image              string `json:"image"`
	PID                int    `json:"pid"`
}

func ListenWithFallback(address string, fallbackPorts int) (net.Listener, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid listen port %q", portText)
	}
	if port == 0 {
		return net.Listen("tcp", address)
	}
	var lastError error
	for offset := 0; offset <= fallbackPorts && port+offset <= 65535; offset++ {
		candidate := net.JoinHostPort(host, strconv.Itoa(port+offset))
		listener, err := net.Listen("tcp", candidate)
		if err == nil {
			return listener, nil
		}
		lastError = err
		if !isPortUnavailable(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no free port in %s plus %d fallbacks: %w", address, fallbackPorts, lastError)
}

func isPortUnavailable(err error) bool {
	// 10048 is WSAEADDRINUSE and 10013 is WSAEACCES (commonly a Windows
	// excluded port range). Keeping them local preserves cross-compilation.
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.Errno(10048)) || errors.Is(err, syscall.Errno(10013))
}

func WithPort(address string, port int) (string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if port < 0 || port > 65535 {
		return "", errors.New("port must be between 0 and 65535")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func EndpointFromListener(listener net.Listener) string {
	address := listener.Addr().(*net.TCPAddr)
	host := address.IP.String()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "ws://" + net.JoinHostPort(host, strconv.Itoa(address.Port))
}

func PortFromListener(listener net.Listener) int {
	return listener.Addr().(*net.TCPAddr).Port
}

func WriteRuntimeFile(path string, info RuntimeInfo) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	payload, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(temporary, path)
}

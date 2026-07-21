package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	controller "github.com/ycxom/docker-control/internal/controller"
)

const (
	serviceName = "docker-control.service"
	markerKind  = "docker-control-installation-v1"
)

type Layout struct {
	Root    string
	State   string
	Bin     string
	Config  string
	Unit    string
	Runtime string
	Marker  string
}

type marker struct {
	Kind      string    `json:"kind"`
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
}

type options struct {
	home               string
	envFile            string
	port               int
	listen             string
	portFallbacks      int
	image              string
	token              string
	controlledEndpoint string
	runtimeFile        string
	namePrefix         string
	noStart            bool
	migrateLegacy      bool
}

func Run(arguments []string, version string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 {
		printHelp(stdout)
		return 0
	}
	command := arguments[0]
	args := arguments[1:]
	if strings.HasPrefix(command, "-") { // v2 compatibility: flags implied server.
		command = "server"
		args = arguments
	}
	var err error
	switch command {
	case "server":
		err = runServer(args, stdout, stderr)
	case "install":
		err = runInstall(args, stdout, stderr)
	case "uninstall":
		err = runUninstall(args, stdout, stderr)
	case "status":
		err = runStatus(args, stdout, stderr)
	case "config":
		err = runConfig(args, stdout, stderr)
	case "version", "--version", "-version":
		_, err = fmt.Fprintf(stdout, "docker-control %s\n", version)
	case "help", "--help", "-h":
		printHelp(stdout)
	default:
		printHelp(stderr)
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "docker-control: %v\n", err)
		return 1
	}
	return 0
}

func printHelp(output io.Writer) {
	_, _ = fmt.Fprint(output, `docker-control - portable managed Docker sandbox

Usage:
  docker-control server [options]     Create local state/config and run foreground
  docker-control install [options]    Self-install into $PWD/.docker-control and enable systemd
  docker-control uninstall [options]  Disable systemd, delete managed sandboxes and local state
  docker-control status [options]     Show service, runtime and sandbox state
  docker-control config [options]     Create/show local configuration paths
  docker-control version
  docker-control help

Common options:
  --home PATH                 Deployment root (default: current directory)
  --env-file PATH             Use an explicit environment file
  --port PORT                 Listen port (default: 16544)
  --port-fallbacks COUNT      Higher ports to try when unavailable (default: 20)
  --image IMAGE               Fixed sandbox image
  --token TOKEN               Management token (generated when omitted)
  --controlled-endpoint URL   ws/wss base injected into sandboxes
  --name-prefix PREFIX        Container name prefix (default: sandbox)

Install option:
  --no-start                  Install/link systemd without enabling or starting it
  --migrate-legacy            Import and stop the former AstrBot-named systemd unit

Uninstall options:
  --keep-containers           Do not delete managed sandboxes
  --keep-files                Keep $PWD/.docker-control after disabling the service
  --force                     Continue file cleanup if Docker cleanup fails
`)
}

func ResolveLayout(home string) (Layout, error) {
	if strings.TrimSpace(home) == "" {
		var err error
		home, err = os.Getwd()
		if err != nil {
			return Layout{}, err
		}
	}
	root, err := filepath.Abs(home)
	if err != nil {
		return Layout{}, err
	}
	root = filepath.Clean(root)
	state := filepath.Join(root, ".docker-control")
	return Layout{
		Root: root, State: state, Bin: filepath.Join(state, "bin", "docker-control"),
		Config:  filepath.Join(state, "docker-control.env"),
		Unit:    filepath.Join(state, "docker-control.service"),
		Runtime: filepath.Join(state, "runtime.json"), Marker: filepath.Join(state, "installation.json"),
	}, nil
}

func preArgument(arguments []string, name string) string {
	value := ""
	for index, argument := range arguments {
		if strings.HasPrefix(argument, name+"=") {
			value = strings.TrimPrefix(argument, name+"=")
		}
		if argument == name && index+1 < len(arguments) {
			value = arguments[index+1]
		}
	}
	return value
}

func hasArgument(arguments []string, name string) bool {
	for _, argument := range arguments {
		if argument == name {
			return true
		}
	}
	return false
}

func prepare(arguments []string, mode string, stdout, stderr io.Writer) (controller.Config, Layout, options, error) {
	layout, err := ResolveLayout(preArgument(arguments, "--home"))
	if err != nil {
		return controller.Config{}, Layout{}, options{}, err
	}
	explicitEnv := preArgument(arguments, "--env-file")
	if mode == "install" && hasArgument(arguments, "--migrate-legacy") {
		legacy := "/etc/astrbot-docker-controller/controller.env"
		if _, err := os.Stat(layout.Config); errors.Is(err, os.ErrNotExist) {
			if payload, readErr := os.ReadFile(legacy); readErr == nil {
				if mkdirErr := os.MkdirAll(layout.State, 0o700); mkdirErr != nil {
					return controller.Config{}, Layout{}, options{}, mkdirErr
				}
				if writeErr := os.WriteFile(layout.Config, payload, 0o600); writeErr != nil {
					return controller.Config{}, Layout{}, options{}, writeErr
				}
			}
		}
	}
	envPath := explicitEnv
	if envPath == "" {
		envPath = layout.Config
	}
	if _, err := os.Stat(envPath); err == nil {
		if err := controller.LoadEnvFile(envPath); err != nil {
			return controller.Config{}, Layout{}, options{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return controller.Config{}, Layout{}, options{}, err
	}
	cfg, _ := controller.ConfigFromEnv()
	if cfg.BearerToken == "" {
		cfg.BearerToken, err = randomToken()
		if err != nil {
			return controller.Config{}, Layout{}, options{}, err
		}
	}
	opt := options{
		home: layout.Root, envFile: explicitEnv, port: -1, listen: cfg.ListenAddress,
		portFallbacks: cfg.PortFallbacks, image: cfg.Image, token: cfg.BearerToken,
		controlledEndpoint: cfg.PublicEndpoint, runtimeFile: layout.Runtime,
		namePrefix: cfg.ContainerNamePrefix,
	}
	set := flag.NewFlagSet(mode, flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&opt.home, "home", opt.home, "deployment root")
	set.StringVar(&opt.envFile, "env-file", opt.envFile, "explicit environment file")
	set.IntVar(&opt.port, "port", opt.port, "listen port")
	set.StringVar(&opt.listen, "listen", opt.listen, "listen address")
	set.IntVar(&opt.portFallbacks, "port-fallbacks", opt.portFallbacks, "higher ports to try")
	set.StringVar(&opt.image, "image", opt.image, "default sandbox image")
	set.StringVar(&opt.token, "token", opt.token, "management token")
	set.StringVar(&opt.controlledEndpoint, "controlled-endpoint", opt.controlledEndpoint, "controlled ws/wss base")
	set.StringVar(&opt.runtimeFile, "runtime-file", opt.runtimeFile, "runtime discovery JSON")
	set.StringVar(&opt.namePrefix, "name-prefix", opt.namePrefix, "container name prefix")
	if mode == "install" {
		set.BoolVar(&opt.noStart, "no-start", false, "do not enable/start systemd")
		set.BoolVar(&opt.migrateLegacy, "migrate-legacy", false, "import former service configuration")
	}
	if err := set.Parse(arguments); err != nil {
		return controller.Config{}, Layout{}, options{}, err
	}
	resolvedLayout, err := ResolveLayout(opt.home)
	if err != nil {
		return controller.Config{}, Layout{}, options{}, err
	}
	if resolvedLayout.Root != layout.Root && explicitEnv == "" {
		return controller.Config{}, Layout{}, options{}, errors.New("--home must be resolved before configuration; place it before other options")
	}
	layout = resolvedLayout
	if opt.runtimeFile == "" || opt.runtimeFile == filepath.Join(layout.Root, ".docker-control", "runtime.json") {
		opt.runtimeFile = layout.Runtime
	}
	cfg.ListenAddress = opt.listen
	if opt.port >= 0 {
		cfg.ListenAddress, err = controller.WithPort(cfg.ListenAddress, opt.port)
		if err != nil {
			return controller.Config{}, Layout{}, options{}, err
		}
	}
	cfg.PortFallbacks = opt.portFallbacks
	cfg.Image = opt.image
	cfg.BearerToken = opt.token
	cfg.PublicEndpoint = opt.controlledEndpoint
	cfg.RuntimeFile = opt.runtimeFile
	cfg.ContainerNamePrefix = opt.namePrefix
	if !cfg.AllowNetwork {
		cfg.SandboxNetwork = "none"
	}
	if err := cfg.Validate(); err != nil {
		return controller.Config{}, Layout{}, options{}, err
	}
	if explicitEnv == "" {
		if err := ensureLocalFiles(layout, cfg); err != nil {
			return controller.Config{}, Layout{}, options{}, err
		}
		_, _ = fmt.Fprintf(stdout, "state: %s\nconfig: %s\n", layout.State, layout.Config)
	} else {
		if err := ensureMarker(layout); err != nil {
			return controller.Config{}, Layout{}, options{}, err
		}
		if mode == "install" {
			payload, err := os.ReadFile(explicitEnv)
			if err != nil {
				return controller.Config{}, Layout{}, options{}, err
			}
			if err := os.WriteFile(layout.Config, payload, 0o600); err != nil {
				return controller.Config{}, Layout{}, options{}, err
			}
		}
	}
	return cfg, layout, opt, nil
}

func ensureLocalFiles(layout Layout, cfg controller.Config) error {
	if err := os.MkdirAll(filepath.Dir(layout.Bin), 0o700); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"token": cfg.BearerToken, "listen": cfg.ListenAddress, "socket": cfg.DockerSocket,
		"endpoint": cfg.PublicEndpoint, "image": cfg.Image, "prefix": cfg.ContainerNamePrefix,
		"network": cfg.SandboxNetwork,
	} {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s contains a line break", name)
		}
	}
	if _, err := os.Stat(layout.Config); errors.Is(err, os.ErrNotExist) {
		content := fmt.Sprintf(`DOCKER_CONTROL_TOKEN=%s
DOCKER_CONTROL_LISTEN=%s
PORT_FALLBACKS=%d
DOCKER_CONTROL_SOCKET=%s
DOCKER_CONTROL_PUBLIC_ENDPOINT=%s
DOCKER_CONTROL_IMAGE=%s
CONTAINER_NAME_PREFIX=%s
PULL_MISSING_IMAGE=%t
ALLOW_NETWORK=%t
SANDBOX_NETWORK=%s
TERMINAL_IDLE_SECONDS=%d
MAX_TERMINAL_WINDOWS=%d
MAX_CONTAINERS=%d
EXECUTION_TIMEOUT_SECONDS=%d
`, cfg.BearerToken, cfg.ListenAddress, cfg.PortFallbacks, cfg.DockerSocket,
			cfg.PublicEndpoint, cfg.Image, cfg.ContainerNamePrefix, cfg.PullMissingImage,
			cfg.AllowNetwork, cfg.SandboxNetwork, int(cfg.TerminalIdleTimeout.Seconds()),
			cfg.MaxTerminalWindows, cfg.MaxContainers, int(cfg.ExecutionTimeout.Seconds()))
		if err := os.WriteFile(layout.Config, []byte(content), 0o600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return ensureMarker(layout)
}

func ensureMarker(layout Layout) error {
	if err := os.MkdirAll(filepath.Dir(layout.Bin), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(layout.Marker); errors.Is(err, os.ErrNotExist) {
		payload, _ := json.MarshalIndent(marker{Kind: markerKind, Root: layout.Root, CreatedAt: time.Now().UTC()}, "", "  ")
		return os.WriteFile(layout.Marker, payload, 0o600)
	} else {
		return err
	}
}

func randomToken() (string, error) {
	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return hex.EncodeToString(payload), nil
}

func runServer(arguments []string, stdout, stderr io.Writer) error {
	cfg, _, _, err := prepare(arguments, "server", stdout, stderr)
	if err != nil {
		return err
	}
	listener, err := controller.ListenWithFallback(cfg.ListenAddress, cfg.PortFallbacks)
	if err != nil {
		return err
	}
	discoveredEndpoint := controller.EndpointFromListener(listener)
	if cfg.PublicEndpoint == "" || cfg.PublicEndpoint == "auto" {
		cfg.PublicEndpoint = discoveredEndpoint
	} else {
		cfg.PublicEndpoint = strings.ReplaceAll(cfg.PublicEndpoint, "{port}", strconv.Itoa(controller.PortFromListener(listener)))
	}
	info := controller.RuntimeInfo{ListenAddress: listener.Addr().String(), ManagementEndpoint: discoveredEndpoint, Image: cfg.Image, PID: os.Getpid()}
	if err := controller.WriteRuntimeFile(cfg.RuntimeFile, info); err != nil {
		_ = listener.Close()
		return err
	}
	engine := controller.NewDockerEngine(cfg.DockerSocket, &http.Client{Timeout: 0})
	prepareContext, cancelPrepare := context.WithTimeout(context.Background(), 10*time.Second)
	imageReady, prepareErr := engine.PrepareImage(prepareContext, cfg.Image, cfg.PullMissingImage)
	cancelPrepare()
	if prepareErr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: image preflight failed: %v\n", prepareErr)
	} else if !imageReady {
		_, _ = fmt.Fprintf(stdout, "image preparation started in background: %s\n", cfg.Image)
	}
	server := &http.Server{Handler: controller.NewServer(cfg, engine), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	_, _ = fmt.Fprintf(stdout, "docker-control listening on %s (image=%s)\n", listener.Addr(), cfg.Image)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runConfig(arguments []string, stdout, stderr io.Writer) error {
	cfg, layout, _, err := prepare(arguments, "config", stdout, stderr)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "home: %s\nlisten: %s\nimage: %s\ntoken: stored in %s\n", layout.Root, cfg.ListenAddress, cfg.Image, layout.Config)
	return err
}

func runInstall(arguments []string, stdout, stderr io.Writer) error {
	if runtime.GOOS != "linux" {
		return errors.New("install requires the Linux binary and systemd")
	}
	_, layout, opt, err := prepare(arguments, "install", stdout, stderr)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := copyExecutable(executable, layout.Bin); err != nil {
		return err
	}
	unit := systemdUnit(layout)
	if err := os.WriteFile(layout.Unit, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := systemctl("link", "--force", layout.Unit); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if opt.migrateLegacy {
		_ = systemctl("disable", "--now", "astrbot-docker-controller.service")
	}
	if !opt.noStart {
		if err := systemctl("enable", "--now", serviceName); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "installed: %s\nunit: %s\nservice: %s\n", layout.Bin, layout.Unit, serviceName)
	return err
}

func copyExecutable(source, target string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	if source == target {
		return os.Chmod(target, 0o755)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := target + ".tmp"
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	_ = os.Remove(target)
	return os.Rename(temporary, target)
}

func systemdQuote(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func systemdUnit(layout Layout) string {
	return fmt.Sprintf(`[Unit]
Description=Portable managed Docker sandbox control
Wants=network-online.target
After=network-online.target docker.service

[Service]
Type=simple
User=root
Group=root
EnvironmentFile=%s
ExecStart=%s server --home %s
WorkingDirectory=%s
Restart=on-failure
RestartSec=3s
TimeoutStopSec=15s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

[Install]
WantedBy=multi-user.target
`, systemdQuote(layout.Config), systemdQuote(layout.Bin), systemdQuote(layout.Root),
		systemdQuote(layout.Root), systemdQuote(layout.State))
}

func systemctl(arguments ...string) error {
	command := exec.Command("systemctl", arguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(arguments, " "), err)
	}
	return nil
}

func runUninstall(arguments []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	set.SetOutput(stderr)
	home := preArgument(arguments, "--home")
	keepContainers := false
	keepFiles := false
	force := false
	set.StringVar(&home, "home", home, "deployment root")
	set.BoolVar(&keepContainers, "keep-containers", false, "do not delete managed sandboxes")
	set.BoolVar(&keepFiles, "keep-files", false, "keep local generated files")
	set.BoolVar(&force, "force", false, "continue if Docker cleanup fails")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	layout, err := ResolveLayout(home)
	if err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		_ = systemctl("disable", "--now", serviceName)
		_ = removeOwnedUnitLink(layout)
		_ = systemctl("daemon-reload")
	}
	if !keepContainers {
		if err := cleanManagedSandboxes(layout, stdout); err != nil {
			if !force {
				return fmt.Errorf("sandbox cleanup failed; files retained (use --force to override): %w", err)
			}
			_, _ = fmt.Fprintf(stderr, "warning: sandbox cleanup failed: %v\n", err)
		}
	}
	if !keepFiles {
		if err := safeRemoveState(layout); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "uninstalled %s\n", layout.State)
	return err
}

func removeOwnedUnitLink(layout Layout) error {
	link := filepath.Join(string(filepath.Separator), "etc", "systemd", "system", serviceName)
	target, err := os.Readlink(link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if filepath.Clean(target) != filepath.Clean(layout.Unit) {
		return errors.New("systemd unit link is not owned by this deployment")
	}
	return os.Remove(link)
}

func cleanManagedSandboxes(layout Layout, stdout io.Writer) error {
	if _, err := os.Stat(layout.Config); err == nil {
		if err := controller.LoadEnvFile(layout.Config); err != nil {
			return err
		}
	}
	cfg, _ := controller.ConfigFromEnv()
	engine := controller.NewDockerEngine(cfg.DockerSocket, &http.Client{Timeout: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sandboxes, err := engine.List(ctx)
	if err != nil {
		return err
	}
	for _, sandbox := range sandboxes {
		removed, err := engine.Remove(ctx, sandbox.Key)
		if err != nil {
			return err
		}
		if removed {
			_, _ = fmt.Fprintf(stdout, "removed sandbox %s (%s)\n", sandbox.Key, sandbox.ID)
		}
	}
	snapshots, err := engine.RemoveAllSnapshots(ctx)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		_, _ = fmt.Fprintf(stdout, "removed snapshot %s/%s (%s)\n", snapshot.Key, snapshot.ID, snapshot.Name)
	}
	return nil
}

func safeRemoveState(layout Layout) error {
	expected := filepath.Join(layout.Root, ".docker-control")
	if filepath.Clean(layout.State) != filepath.Clean(expected) || layout.State == layout.Root {
		return errors.New("refusing to remove unsafe state path")
	}
	payload, err := os.ReadFile(layout.Marker)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("installation marker missing; refusing to remove files")
	}
	if err != nil {
		return err
	}
	var installed marker
	if err := json.Unmarshal(payload, &installed); err != nil {
		return err
	}
	if installed.Kind != markerKind || filepath.Clean(installed.Root) != filepath.Clean(layout.Root) {
		return errors.New("installation marker does not match deployment root")
	}
	return os.RemoveAll(layout.State)
}

func runStatus(arguments []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("status", flag.ContinueOnError)
	set.SetOutput(stderr)
	home := preArgument(arguments, "--home")
	set.StringVar(&home, "home", home, "deployment root")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	layout, err := ResolveLayout(home)
	if err != nil {
		return err
	}
	result := map[string]any{"home": layout.Root, "state_directory": layout.State}
	if payload, err := os.ReadFile(layout.Runtime); err == nil {
		var runtimeInfo any
		if json.Unmarshal(payload, &runtimeInfo) == nil {
			result["runtime"] = runtimeInfo
		}
	}
	if runtime.GOOS == "linux" {
		output, err := exec.Command("systemctl", "is-active", serviceName).CombinedOutput()
		result["service"] = strings.TrimSpace(string(output))
		if err != nil {
			result["service_error"] = err.Error()
		}
	}
	if _, err := os.Stat(layout.Config); err == nil {
		_ = controller.LoadEnvFile(layout.Config)
		cfg, _ := controller.ConfigFromEnv()
		engine := controller.NewDockerEngine(cfg.DockerSocket, &http.Client{Timeout: 0})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if sandboxes, err := engine.List(ctx); err == nil {
			result["sandboxes"] = sandboxes
		} else {
			result["docker_error"] = err.Error()
		}
	}
	payload, _ := json.MarshalIndent(result, "", "  ")
	_, err = fmt.Fprintln(stdout, string(payload))
	return err
}

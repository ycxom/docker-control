package controller

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type imagePullState struct {
	done chan struct{}
	err  error
}

type DockerEngine struct {
	client    *http.Client
	base      string
	pullMu    sync.Mutex
	imagePull map[string]*imagePullState
}

func NewDockerEngine(endpoint string, client *http.Client) *DockerEngine {
	if client == nil {
		client = &http.Client{}
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return &DockerEngine{client: client, base: strings.TrimRight(endpoint, "/"), imagePull: map[string]*imagePullState{}}
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		},
	}
	return &DockerEngine{client: &http.Client{Transport: transport}, base: "http://docker", imagePull: map[string]*imagePullState{}}
}

func (d *DockerEngine) Version(ctx context.Context) (string, error) {
	var response struct {
		Version string `json:"Version"`
	}
	if err := d.json(ctx, http.MethodGet, "/version", nil, &response); err != nil {
		return "", err
	}
	return response.Version, nil
}

func (d *DockerEngine) List(ctx context.Context) ([]Container, error) {
	containers := []Container{}
	seen := map[string]bool{}
	for _, label := range []string{managedLabel, legacyManagedLabel} {
		raw, err := d.listByManagedLabel(ctx, label)
		if err != nil {
			return nil, err
		}
		for _, item := range raw {
			if !seen[item.ID] {
				containers = append(containers, item.container())
				seen[item.ID] = true
			}
		}
	}
	return containers, nil
}

func (d *DockerEngine) listByManagedLabel(ctx context.Context, label string) ([]dockerContainerSummary, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label + "=true"}})
	var raw []dockerContainerSummary
	err := d.json(ctx, http.MethodGet, "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &raw)
	return raw, err
}

func (d *DockerEngine) Get(ctx context.Context, session string) (Container, error) {
	item, err := d.find(ctx, session)
	if err != nil {
		return Container{}, err
	}
	return d.inspectReadiness(ctx, item)
}

func (d *DockerEngine) Ensure(ctx context.Context, spec CreateSpec, pullMissing bool, maxContainers int) (Container, bool, error) {
	item, err := d.find(ctx, spec.Key)
	if err == nil {
		if item.container().Image == spec.Image {
			if item.State != "running" {
				if err := d.empty(ctx, http.MethodPost, "/containers/"+url.PathEscape(item.ID)+"/start"); err != nil {
					return Container{}, false, err
				}
				item.State = "running"
			}
			container, err := d.waitForReady(ctx, item, 5*time.Second)
			return container, false, err
		}
		ready, prepareErr := d.PrepareImage(ctx, spec.Image, pullMissing)
		if prepareErr != nil {
			return Container{}, false, prepareErr
		}
		if !ready {
			return Container{}, false, fmt.Errorf("%w: %s; current sandbox was preserved", ErrImagePreparing, spec.Image)
		}
		if err := d.empty(ctx, http.MethodDelete, "/containers/"+url.PathEscape(item.ID)+"?force=true"); err != nil {
			return Container{}, false, err
		}
		err = ErrNotFound
	}
	if !errors.Is(err, ErrNotFound) {
		return Container{}, false, err
	}
	managed, err := d.List(ctx)
	if err != nil {
		return Container{}, false, err
	}
	if len(managed) >= maxContainers {
		return Container{}, false, fmt.Errorf("managed container limit reached (%d)", maxContainers)
	}
	ready, err := d.PrepareImage(ctx, spec.Image, pullMissing)
	if err != nil {
		return Container{}, false, err
	}
	if !ready {
		return Container{}, false, fmt.Errorf("%w: %s; retry after the controller finishes pulling it", ErrImagePreparing, spec.Image)
	}
	return d.createSandbox(ctx, spec, spec.Image, spec.Image, spec.Name, false)
}

func (d *DockerEngine) createSandbox(ctx context.Context, spec CreateSpec, runtimeImage, logicalImage, name string, restoreWorkspace bool) (Container, bool, error) {
	startup := "mkdir -p /workspace && touch /workspace/.docker-control-ready && tail -f /dev/null"
	if restoreWorkspace {
		startup = "mkdir -p /workspace && if [ -d /.docker-control-snapshot/workspace ]; then cp -a /.docker-control-snapshot/workspace/. /workspace/; fi && touch /workspace/.docker-control-ready && tail -f /dev/null"
	}
	create := map[string]any{
		"Image": runtimeImage,
		"Labels": map[string]string{
			managedLabel: "true", keyLabel: spec.Key,
			endpointLabel: spec.ControlledEndpoint, healthLabel: spec.ControlledHealthEndpoint,
			tokenLabel: spec.ControlledTokenSHA256, imageLabel: logicalImage,
		},
		"WorkingDir": "/workspace",
		"Env": []string{
			"HOME=/workspace", "PYTHONPATH=/workspace/.packages", "PIP_TARGET=/workspace/.packages",
			"CONTROLLED_DOCKER_ENDPOINT=" + spec.ControlledEndpoint,
			"CONTROLLED_DOCKER_HEALTH_ENDPOINT=" + spec.ControlledHealthEndpoint,
			"CONTROLLED_DOCKER_TOKEN=" + spec.ControlledToken,
		},
		"Cmd":         []string{"sh", "-lc", startup},
		"StopTimeout": 2,
		"Healthcheck": map[string]any{
			"Test":     []string{"CMD-SHELL", "test -f /workspace/.docker-control-ready"},
			"Interval": int64(time.Second), "Timeout": int64(time.Second),
			"StartPeriod": int64(time.Second), "Retries": 30,
		},
		"HostConfig": sandboxHostConfig(spec),
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.json(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), create, &created); err != nil {
		return Container{}, false, err
	}
	if err := d.empty(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.ID)+"/start"); err != nil {
		_ = d.empty(ctx, http.MethodDelete, "/containers/"+url.PathEscape(created.ID)+"?force=true")
		return Container{}, false, err
	}
	item := dockerContainerSummary{ID: created.ID, Names: []string{"/" + name}, Image: runtimeImage, State: "running", Labels: map[string]string{keyLabel: spec.Key, endpointLabel: spec.ControlledEndpoint, imageLabel: logicalImage}}
	container, err := d.waitForReady(ctx, item, 5*time.Second)
	return container, true, err
}

func sandboxHostConfig(spec CreateSpec) map[string]any {
	return map[string]any{
		"Memory": spec.MemoryBytes, "NanoCpus": spec.NanoCPUs, "PidsLimit": spec.PIDsLimit,
		"NetworkMode": spec.NetworkMode, "Privileged": false,
		"ExtraHosts":     []string{"host.docker.internal:host-gateway"},
		"ReadonlyRootfs": false,
		"Tmpfs": map[string]string{
			"/workspace": fmt.Sprintf("rw,nosuid,nodev,size=%d,mode=0700", spec.WorkspaceBytes),
			"/tmp":       "rw,nosuid,nodev,size=67108864,mode=1777",
		},
	}
}

func (d *DockerEngine) Remove(ctx context.Context, session string) (bool, error) {
	item, err := d.find(ctx, session)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := d.empty(ctx, http.MethodDelete, "/containers/"+url.PathEscape(item.ID)+"?force=true"); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DockerEngine) CreateSnapshot(ctx context.Context, key, name string, maxSnapshots int) (Snapshot, error) {
	if !validKey(key) || !validSnapshotName(name) {
		return Snapshot{}, ErrInvalidSandboxKey
	}
	existing, err := d.ListSnapshots(ctx, key)
	if err != nil {
		return Snapshot{}, err
	}
	if len(existing) >= maxSnapshots {
		return Snapshot{}, fmt.Errorf("%w: %d per sandbox", ErrSnapshotLimit, maxSnapshots)
	}
	item, err := d.requireReady(ctx, key)
	if err != nil {
		return Snapshot{}, err
	}
	identifierBytes := make([]byte, 6)
	if _, err := rand.Read(identifierBytes); err != nil {
		return Snapshot{}, err
	}
	id := hex.EncodeToString(identifierBytes)
	if strings.TrimSpace(name) == "" {
		name = "restore-point-" + time.Now().UTC().Format("20060102-150405")
	}
	prepare, err := d.Exec(ctx, key, ExecRequest{Command: []string{"sh", "-lc", "rm -rf /.docker-control-snapshot && mkdir -p /.docker-control-snapshot"}, WorkDir: "/workspace"}, 30*time.Second, 4096)
	if err != nil {
		return Snapshot{}, err
	}
	if prepare.ExitCode != 0 {
		return Snapshot{}, fmt.Errorf("prepare snapshot workspace: %s", prepare.Output)
	}
	defer func() {
		_, _ = d.Exec(context.Background(), key, ExecRequest{Command: []string{"rm", "-rf", "/.docker-control-snapshot"}, WorkDir: "/workspace"}, 30*time.Second, 4096)
	}()
	if err := d.copyContainerArchive(ctx, item.ID, "/workspace", "/.docker-control-snapshot"); err != nil {
		return Snapshot{}, fmt.Errorf("capture workspace: %w", err)
	}
	repository := "docker-control-snapshot"
	tag := key + "-" + id
	logicalImage := item.container().Image
	query := url.Values{}
	query.Set("container", item.ID)
	query.Set("repo", repository)
	query.Set("tag", tag)
	query.Set("pause", "true")
	for label, value := range map[string]string{
		snapshotLabel: "true", snapshotKeyLabel: key, snapshotIDLabel: id,
		snapshotNameLabel: name, snapshotSourceLabel: logicalImage,
	} {
		query.Add("changes", dockerLabelChange(label, value))
	}
	if err := d.empty(ctx, http.MethodPost, "/commit?"+query.Encode()); err != nil {
		return Snapshot{}, err
	}
	return d.findSnapshot(ctx, key, id)
}

func (d *DockerEngine) ListSnapshots(ctx context.Context, key string) ([]Snapshot, error) {
	if !validKey(key) {
		return nil, ErrInvalidSandboxKey
	}
	return d.listSnapshots(ctx, key)
}

func (d *DockerEngine) listSnapshots(ctx context.Context, key string) ([]Snapshot, error) {
	labels := []string{snapshotLabel + "=true"}
	if key != "" {
		labels = append(labels, snapshotKeyLabel+"="+key)
	}
	filters, _ := json.Marshal(map[string][]string{"label": labels})
	var images []dockerImageSummary
	if err := d.json(ctx, http.MethodGet, "/images/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &images); err != nil {
		return nil, err
	}
	snapshots := make([]Snapshot, 0, len(images))
	for _, image := range images {
		id := image.Labels[snapshotIDLabel]
		if !validSnapshotID(id) {
			continue
		}
		imageName := image.ID
		if len(image.RepoTags) > 0 && image.RepoTags[0] != "<none>:<none>" {
			imageName = image.RepoTags[0]
		}
		snapshotKey := image.Labels[snapshotKeyLabel]
		snapshots = append(snapshots, Snapshot{
			ID: id, Key: snapshotKey, Name: image.Labels[snapshotNameLabel], Image: imageName,
			SourceImage: image.Labels[snapshotSourceLabel], CreatedAt: time.Unix(image.Created, 0).UTC(), SizeBytes: image.Size,
		})
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].CreatedAt.After(snapshots[j].CreatedAt) })
	return snapshots, nil
}

func (d *DockerEngine) RemoveAllSnapshots(ctx context.Context) ([]Snapshot, error) {
	snapshots, err := d.listSnapshots(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, snapshot := range snapshots {
		if err := d.empty(ctx, http.MethodDelete, "/images/"+url.PathEscape(snapshot.Image)+"?force=true"); err != nil {
			return nil, err
		}
	}
	return snapshots, nil
}

func (d *DockerEngine) RestoreSnapshot(ctx context.Context, key, id string, spec CreateSpec) (Container, error) {
	if !validKey(key) || !validSnapshotID(id) || spec.Key != key {
		return Container{}, ErrInvalidSandboxKey
	}
	snapshot, err := d.findSnapshot(ctx, key, id)
	if err != nil {
		return Container{}, err
	}
	old, err := d.find(ctx, key)
	if err != nil {
		return Container{}, err
	}
	temporaryName := spec.Name + "-restore-" + id[:6]
	container, _, err := d.createSandbox(ctx, spec, snapshot.Image, snapshot.SourceImage, temporaryName, true)
	if err != nil {
		return Container{}, err
	}
	if !container.Ready {
		_, _ = d.removeContainerByID(ctx, container.ID)
		return Container{}, fmt.Errorf("%w: restored container did not become ready", ErrSandboxNotReady)
	}
	if _, err := d.removeContainerByID(ctx, old.ID); err != nil {
		_, _ = d.removeContainerByID(ctx, container.ID)
		return Container{}, err
	}
	if err := d.empty(ctx, http.MethodPost, "/containers/"+url.PathEscape(container.ID)+"/rename?name="+url.QueryEscape(spec.Name)); err != nil {
		return Container{}, err
	}
	container.Name = spec.Name
	return container, nil
}

func (d *DockerEngine) RemoveSnapshot(ctx context.Context, key, id string) (bool, error) {
	snapshot, err := d.findSnapshot(ctx, key, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := d.empty(ctx, http.MethodDelete, "/images/"+url.PathEscape(snapshot.Image)+"?force=true"); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DockerEngine) findSnapshot(ctx context.Context, key, id string) (Snapshot, error) {
	if !validSnapshotID(id) {
		return Snapshot{}, ErrNotFound
	}
	snapshots, err := d.ListSnapshots(ctx, key)
	if err != nil {
		return Snapshot{}, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			return snapshot, nil
		}
	}
	return Snapshot{}, ErrNotFound
}

func (d *DockerEngine) copyContainerArchive(ctx context.Context, containerID, source, destination string) error {
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/containers/"+url.PathEscape(containerID)+"/archive?path="+url.QueryEscape(source), nil)
	if err != nil {
		return err
	}
	response, err := d.client.Do(get)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := dockerStatus(response); err != nil {
		return err
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, d.base+"/containers/"+url.PathEscape(containerID)+"/archive?path="+url.QueryEscape(destination), response.Body)
	if err != nil {
		return err
	}
	put.Header.Set("Content-Type", "application/x-tar")
	putResponse, err := d.client.Do(put)
	if err != nil {
		return err
	}
	defer putResponse.Body.Close()
	return dockerStatus(putResponse)
}

func (d *DockerEngine) removeContainerByID(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if err := d.empty(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=true"); err != nil {
		return false, err
	}
	return true, nil
}

func dockerLabelChange(key, value string) string {
	escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value)
	return fmt.Sprintf("LABEL %s=\"%s\"", key, escaped)
}

func (d *DockerEngine) Exec(ctx context.Context, session string, request ExecRequest, timeout time.Duration, maxOutput int) (ExecResult, error) {
	return d.ExecStream(ctx, session, request, timeout, maxOutput, nil)
}

func (d *DockerEngine) ExecStream(ctx context.Context, session string, request ExecRequest, timeout time.Duration, maxOutput int, onOutput func([]byte) error) (ExecResult, error) {
	item, err := d.requireReady(ctx, session)
	if err != nil {
		return ExecResult{}, err
	}
	seconds := int(timeout.Seconds())
	command := append([]string{"timeout", "-s", "KILL", strconv.Itoa(seconds) + "s"}, request.Command...)
	create := map[string]any{"AttachStdout": true, "AttachStderr": true, "Cmd": command, "WorkingDir": request.WorkDir}
	var execCreated struct {
		ID string `json:"Id"`
	}
	if err := d.json(ctx, http.MethodPost, "/containers/"+url.PathEscape(item.ID)+"/exec", create, &execCreated); err != nil {
		return ExecResult{}, err
	}
	body, err := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return ExecResult{}, err
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(execCtx, http.MethodPost, d.base+"/exec/"+url.PathEscape(execCreated.ID)+"/start", bytes.NewReader(body))
	if err != nil {
		return ExecResult{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(httpRequest)
	if err != nil {
		return ExecResult{}, err
	}
	defer response.Body.Close()
	if err := dockerStatus(response); err != nil {
		return ExecResult{}, err
	}
	output, truncated, err := readDockerStream(response.Body, maxOutput, onOutput)
	if err != nil {
		return ExecResult{}, err
	}
	var inspected struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := d.json(ctx, http.MethodGet, "/exec/"+url.PathEscape(execCreated.ID)+"/json", nil, &inspected); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: inspected.ExitCode, Output: string(output), Truncated: truncated, TimedOut: inspected.ExitCode == 124 || inspected.ExitCode == 137}, nil
}

func (d *DockerEngine) WriteFile(ctx context.Context, session, filePath string, payload []byte) error {
	item, err := d.requireReady(ctx, session)
	if err != nil {
		return err
	}
	parent := path.Dir(filePath)
	if parent != "/workspace" {
		result, err := d.Exec(ctx, session, ExecRequest{Command: []string{"mkdir", "-p", parent}, WorkDir: "/workspace"}, 30*time.Second, 4096)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("create parent directory: %s", result.Output)
		}
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: path.Base(filePath), Mode: 0o600, Size: int64(len(payload))}); err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, d.base+"/containers/"+url.PathEscape(item.ID)+"/archive?path="+url.QueryEscape(parent), &archive)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-tar")
	response, err := d.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return dockerStatus(response)
}

func (d *DockerEngine) ReadFile(ctx context.Context, session, filePath string, maxBytes int64) ([]byte, bool, error) {
	item, err := d.requireReady(ctx, session)
	if err != nil {
		return nil, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/containers/"+url.PathEscape(item.ID)+"/archive?path="+url.QueryEscape(filePath), nil)
	if err != nil {
		return nil, false, err
	}
	response, err := d.client.Do(request)
	if err != nil {
		return nil, false, err
	}
	defer response.Body.Close()
	if err := dockerStatus(response); err != nil {
		return nil, false, err
	}
	reader := tar.NewReader(response.Body)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, false, ErrNotFound
		}
		if err != nil {
			return nil, false, err
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			payload, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
			if err != nil {
				return nil, false, err
			}
			if int64(len(payload)) > maxBytes {
				return payload[:maxBytes], true, nil
			}
			return payload, false, nil
		}
	}
}

func (d *DockerEngine) VerifyControlledToken(ctx context.Context, session, token string) (bool, error) {
	item, err := d.find(ctx, session)
	if err != nil {
		return false, err
	}
	stored := item.Labels[tokenLabel]
	if stored == "" {
		stored = item.Labels[legacyTokenLabel]
	}
	return secureEqual(stored, controlledTokenHash(token)), nil
}

type dockerContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

type dockerImageSummary struct {
	ID       string            `json:"Id"`
	RepoTags []string          `json:"RepoTags"`
	Created  int64             `json:"Created"`
	Size     int64             `json:"Size"`
	Labels   map[string]string `json:"Labels"`
}

func (item dockerContainerSummary) container() Container {
	name := ""
	if len(item.Names) > 0 {
		name = strings.TrimPrefix(item.Names[0], "/")
	}
	key := item.Labels[keyLabel]
	if key == "" {
		key = item.Labels[legacyKeyLabel]
	}
	endpoint := item.Labels[endpointLabel]
	if endpoint == "" {
		endpoint = item.Labels[legacyEndpointLabel]
	}
	ready, readiness := summaryReadiness(item.State, item.Status)
	logicalImage := item.Labels[imageLabel]
	if logicalImage == "" {
		logicalImage = item.Image
	}
	return Container{ID: item.ID, Name: name, Key: key, Image: logicalImage, State: item.State, Ready: ready, Readiness: readiness, ControlledEndpoint: endpoint, CreatedAt: time.Unix(item.Created, 0).UTC()}
}

func summaryReadiness(state, status string) (bool, string) {
	if state != "running" {
		return false, "stopped"
	}
	if strings.Contains(status, "(unhealthy)") {
		return false, "unhealthy"
	}
	if strings.Contains(status, "(health: starting)") {
		return false, "starting"
	}
	return true, "ready"
}

func (d *DockerEngine) inspectReadiness(ctx context.Context, item dockerContainerSummary) (Container, error) {
	container := item.container()
	var details struct {
		State struct {
			Status  string `json:"Status"`
			Running bool   `json:"Running"`
			Health  *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := d.json(ctx, http.MethodGet, "/containers/"+url.PathEscape(item.ID)+"/json", nil, &details); err != nil {
		return Container{}, err
	}
	container.State = details.State.Status
	if !details.State.Running {
		container.Ready, container.Readiness = false, "stopped"
	} else if details.State.Health == nil {
		container.Ready, container.Readiness = true, "ready"
	} else {
		container.Ready = details.State.Health.Status == "healthy"
		if container.Ready {
			container.Readiness = "ready"
		} else {
			container.Readiness = details.State.Health.Status
		}
	}
	return container, nil
}

func (d *DockerEngine) waitForReady(ctx context.Context, item dockerContainerSummary, timeout time.Duration) (Container, error) {
	deadline := time.Now().Add(timeout)
	for {
		container, err := d.inspectReadiness(ctx, item)
		if err != nil {
			return Container{}, err
		}
		if container.Ready || time.Now().After(deadline) {
			return container, nil
		}
		select {
		case <-ctx.Done():
			return Container{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (d *DockerEngine) requireReady(ctx context.Context, key string) (dockerContainerSummary, error) {
	item, err := d.find(ctx, key)
	if err != nil {
		return dockerContainerSummary{}, err
	}
	container, err := d.inspectReadiness(ctx, item)
	if err != nil {
		return dockerContainerSummary{}, err
	}
	if !container.Ready {
		return dockerContainerSummary{}, fmt.Errorf("%w: %s (%s)", ErrSandboxNotReady, key, container.Readiness)
	}
	return item, nil
}

func (d *DockerEngine) find(ctx context.Context, key string) (dockerContainerSummary, error) {
	if !validKey(key) {
		return dockerContainerSummary{}, ErrNotFound
	}
	labelPairs := [][2]string{{managedLabel, keyLabel}, {legacyManagedLabel, legacyKeyLabel}}
	for _, labels := range labelPairs {
		filters, _ := json.Marshal(map[string][]string{"label": {labels[0] + "=true", labels[1] + "=" + key}})
		var items []dockerContainerSummary
		if err := d.json(ctx, http.MethodGet, "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &items); err != nil {
			return dockerContainerSummary{}, err
		}
		if len(items) > 0 {
			return items[0], nil
		}
	}
	return dockerContainerSummary{}, ErrNotFound
}

func (d *DockerEngine) PrepareImage(ctx context.Context, image string, pullMissing bool) (bool, error) {
	err := d.empty(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json")
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if !pullMissing {
		return false, fmt.Errorf("sandbox image is missing: %s", image)
	}
	d.pullMu.Lock()
	if state, exists := d.imagePull[image]; exists {
		select {
		case <-state.done:
			delete(d.imagePull, image)
			err := state.err
			d.pullMu.Unlock()
			if err != nil {
				return false, fmt.Errorf("pull sandbox image %s: %w", image, err)
			}
			return true, nil
		default:
			d.pullMu.Unlock()
			return false, nil
		}
	}
	state := &imagePullState{done: make(chan struct{})}
	d.imagePull[image] = state
	d.pullMu.Unlock()
	go func() {
		pullContext, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		err := d.pullImage(pullContext, image)
		d.pullMu.Lock()
		state.err = err
		close(state.done)
		d.pullMu.Unlock()
	}()
	return false, nil
}

func (d *DockerEngine) pullImage(ctx context.Context, image string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	response, err := d.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := dockerStatus(response); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func (d *DockerEngine) json(ctx context.Context, method, endpoint string, body, target any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, d.base+endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := d.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := dockerStatus(response); err != nil {
		return err
	}
	if target == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (d *DockerEngine) empty(ctx context.Context, method, endpoint string) error {
	return d.json(ctx, method, endpoint, nil, nil)
}

func dockerStatus(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	var message struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &message)
	if message.Message == "" {
		message.Message = strings.TrimSpace(string(payload))
	}
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, message.Message)
	}
	return fmt.Errorf("docker engine returned %s: %s", response.Status, message.Message)
}

func readDockerStream(reader io.Reader, limit int, onOutput func([]byte) error) ([]byte, bool, error) {
	var output bytes.Buffer
	truncated := false
	header := make([]byte, 8)
	for {
		_, err := io.ReadFull(reader, header)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
		size := int(binary.BigEndian.Uint32(header[4:8]))
		frame := make([]byte, size)
		if _, err := io.ReadFull(reader, frame); err != nil {
			return nil, false, err
		}
		remaining := limit - output.Len()
		if remaining > 0 {
			chunk := frame
			if len(frame) > remaining {
				chunk = frame[:remaining]
				output.Write(chunk)
				truncated = true
			} else {
				output.Write(chunk)
			}
			if onOutput != nil && len(chunk) > 0 {
				if err := onOutput(chunk); err != nil {
					return nil, false, err
				}
			}
		} else if len(frame) > 0 {
			truncated = true
		}
	}
	return output.Bytes(), truncated, nil
}

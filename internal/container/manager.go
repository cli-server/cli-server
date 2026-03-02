package container

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockermount "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/agentserver/agentserver/internal/process"
	"github.com/agentserver/agentserver/internal/sandbox"
)

const labelManagedBy = "managed-by"
const labelValue = "agentserver"

// Compile-time interface checks.
var (
	_ process.Process = (*containerProcess)(nil)
	_ process.Manager = (*Manager)(nil)
)

type containerProcess struct {
	containerID string
	cmd         *exec.Cmd
	ptyFile     *os.File
	done        chan struct{}
	once        sync.Once
}

func (p *containerProcess) Read(buf []byte) (int, error) {
	return p.ptyFile.Read(buf)
}

func (p *containerProcess) Write(data []byte) (int, error) {
	return p.ptyFile.Write(data)
}

func (p *containerProcess) Resize(rows, cols uint16) error {
	return pty.Setsize(p.ptyFile, &pty.Winsize{Rows: rows, Cols: cols})
}

func (p *containerProcess) Done() <-chan struct{} {
	return p.done
}

type Manager struct {
	cfg       Config
	cli       *client.Client
	mu        sync.RWMutex
	processes map[string]*containerProcess
}

func NewManager(cfg Config) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	ctx := context.Background()
	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("docker ping: %w", err)
	}

	m := &Manager{
		cfg:       cfg,
		cli:       cli,
		processes: make(map[string]*containerProcess),
	}

	return m, nil
}

// CleanOrphans removes containers labelled managed-by=agentserver that are NOT in the known set.
func (m *Manager) CleanOrphans(knownContainerNames []string) {
	ctx := context.Background()

	known := make(map[string]bool, len(knownContainerNames))
	for _, name := range knownContainerNames {
		known["/"+name] = true // Docker container names have a leading slash
	}

	f := filters.NewArgs(filters.Arg("label", labelManagedBy+"="+labelValue))
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		log.Printf("failed to list orphan containers: %v", err)
		return
	}
	for _, c := range containers {
		isKnown := false
		for _, name := range c.Names {
			if known[name] {
				isKnown = true
				break
			}
		}
		if isKnown {
			continue
		}
		log.Printf("cleaning orphan container %s", c.ID[:12])
		m.cli.ContainerStop(ctx, c.ID, container.StopOptions{})
		m.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}

func (m *Manager) Start(id, command string, args, env []string, opts process.StartOptions) (process.Process, error) {
	containerID, err := m.EnsureContainer(id, opts)
	if err != nil {
		return nil, err
	}
	return m.execInContainer(id, containerID, command, args, env)
}

// EnsureContainer creates and starts a container without exec-ing into it.
// The container's entrypoint (sleep infinity) keeps it alive.
// Returns the container ID.
func (m *Manager) EnsureContainer(id string, opts process.StartOptions) (string, error) {
	ctx := context.Background()

	containerName := "cli-sandbox-" + id

	// Check if container already exists.
	f := filters.NewArgs(
		filters.Arg("name", containerName),
		filters.Arg("label", labelManagedBy+"="+labelValue),
	)
	existing, _ := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if len(existing) > 0 {
		ctr := existing[0]
		if ctr.State != "running" {
			if err := m.cli.ContainerStart(ctx, ctr.ID, container.StartOptions{}); err != nil {
				return "", fmt.Errorf("container restart: %w", err)
			}
		}
		return ctr.ID, nil
	}

	// Build environment for the container
	containerEnv := []string{"TERM=xterm-256color"}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			containerEnv = append(containerEnv, key+"="+v)
		}
	}

	// Select image and set env vars based on sandbox type.
	containerImage := m.cfg.Image
	switch opts.SandboxType {
	case "openclaw":
		if m.cfg.OpenclawImage != "" {
			containerImage = m.cfg.OpenclawImage
		}
		if opts.OpenclawToken != "" {
			containerEnv = append(containerEnv, "OPENCLAW_GATEWAY_TOKEN="+opts.OpenclawToken)
		}
	default: // "opencode"
		if opts.OpencodeToken != "" {
			containerEnv = append(containerEnv, "OPENCODE_SERVER_PASSWORD="+opts.OpencodeToken)
		}
		if m.cfg.OpencodeConfigContent != "" {
			containerEnv = append(containerEnv, "OPENCODE_CONFIG_CONTENT="+m.cfg.OpencodeConfigContent)
		}
	}

	// Volume mounts for persistence.
	mounts := []dockermount.Mount{
		{
			Type:   dockermount.TypeVolume,
			Source: "cli-sandbox-" + id + "-data",
			Target: "/home/agent",
		},
	}
	for _, vol := range opts.WorkspaceVolumes {
		mounts = append(mounts, dockermount.Mount{
			Type:   dockermount.TypeVolume,
			Source: vol.PVCName,
			Target: vol.MountPath,
		})
	}

	pidsLimit := m.cfg.PidsLimit
	memoryLimit := m.cfg.MemoryLimit
	nanoCPUs := m.cfg.NanoCPUs
	if opts.Memory != 0 {
		memoryLimit = opts.Memory
	}
	if opts.CPU != 0 {
		nanoCPUs = int64(opts.CPU) * 1_000_000
	}
	containerConfig := &container.Config{
		Image:  containerImage,
		Env:    containerEnv,
		Labels: map[string]string{labelManagedBy: labelValue},
	}
	if opts.SandboxType == "openclaw" {
		openclawCfg := sandbox.BuildOpenclawConfig(os.Getenv("ANTHROPIC_BASE_URL"), os.Getenv("ANTHROPIC_API_KEY"))
		containerConfig.Cmd = []string{"sh", "-c", `mkdir -p ~/.openclaw && cat > ~/.openclaw/openclaw.json << 'CFGEOF'
` + openclawCfg + `
CFGEOF
exec node openclaw.mjs gateway --allow-unconfigured --bind lan`}
	}
	resp, err := m.cli.ContainerCreate(ctx,
		containerConfig,
		&container.HostConfig{
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			NetworkMode: container.NetworkMode(m.cfg.NetworkMode),
			Mounts:      mounts,
			Resources: container.Resources{
				Memory:    memoryLimit,
				NanoCPUs:  nanoCPUs,
				PidsLimit: &pidsLimit,
			},
		},
		nil, nil, containerName,
	)
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("container start: %w", err)
	}

	return resp.ID, nil
}

// StartContainer creates and starts a container without exec-ing into it.
func (m *Manager) StartContainer(id string, opts process.StartOptions) error {
	_, err := m.EnsureContainer(id, opts)
	return err
}

// Pause stops the exec process and Docker container, preserving volumes.
func (m *Manager) Pause(id string) error {
	m.mu.Lock()
	p, ok := m.processes[id]
	if ok {
		delete(m.processes, id)
	}
	m.mu.Unlock()

	if ok {
		// Clean up PTY process if one exists.
		p.ptyFile.Close()
		if p.cmd.Process != nil {
			p.cmd.Process.Signal(syscall.SIGTERM)
		}
		p.once.Do(func() { close(p.done) })

		ctx := context.Background()
		m.cli.ContainerStop(ctx, p.containerID, container.StopOptions{})
		return nil
	}

	// No PTY process — stop the container by name (chat mode).
	containerName := "cli-sandbox-" + id
	ctx := context.Background()
	f := filters.NewArgs(
		filters.Arg("name", containerName),
		filters.Arg("label", labelManagedBy+"="+labelValue),
	)
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil || len(containers) == 0 {
		return fmt.Errorf("session %s: container not found for pause", id)
	}
	m.cli.ContainerStop(ctx, containers[0].ID, container.StopOptions{})
	return nil
}

// Resume starts the stopped container and exec's into it.
func (m *Manager) Resume(id, containerName, command string, args []string) (process.Process, error) {
	ctx := context.Background()

	// Find the container by name.
	f := filters.NewArgs(
		filters.Arg("name", containerName),
		filters.Arg("label", labelManagedBy+"="+labelValue),
	)
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("find container for resume: %w", err)
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("container %s not found for resume", containerName)
	}

	containerID := containers[0].ID

	// Start the stopped container.
	if err := m.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("container start on resume: %w", err)
	}

	return m.execInContainer(id, containerID, command, args, nil)
}

// execInContainer runs docker exec -it with a PTY into the container.
func (m *Manager) execInContainer(id, containerID, command string, args, env []string) (process.Process, error) {
	execArgs := []string{"exec", "-it", containerID, command}
	execArgs = append(execArgs, args...)
	cmd := exec.Command("docker", execArgs...)
	cmd.Env = append(os.Environ(), env...)

	ptyFile, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}

	p := &containerProcess{
		containerID: containerID,
		cmd:         cmd,
		ptyFile:     ptyFile,
		done:        make(chan struct{}),
	}

	m.mu.Lock()
	m.processes[id] = p
	m.mu.Unlock()

	go func() {
		cmd.Wait()
		p.once.Do(func() { close(p.done) })
	}()

	return p, nil
}

func (m *Manager) Get(id string) (process.Process, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.processes[id]
	if !ok {
		return nil, false
	}
	return p, ok
}

func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	p, ok := m.processes[id]
	if ok {
		delete(m.processes, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}

	p.ptyFile.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Signal(syscall.SIGTERM)
	}
	p.once.Do(func() { close(p.done) })

	ctx := context.Background()
	m.cli.ContainerStop(ctx, p.containerID, container.StopOptions{})
	m.cli.ContainerRemove(ctx, p.containerID, container.RemoveOptions{Force: true})

	// Also remove the session data volume.
	m.cli.VolumeRemove(ctx, "cli-sandbox-"+id+"-data", true)
	return nil
}

// StopByContainerName stops and removes a container by name (for paused sessions).
func (m *Manager) StopByContainerName(containerName string) error {
	ctx := context.Background()
	f := filters.NewArgs(
		filters.Arg("name", containerName),
		filters.Arg("label", labelManagedBy+"="+labelValue),
	)
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return err
	}
	for _, c := range containers {
		m.cli.ContainerStop(ctx, c.ID, container.StopOptions{})
		m.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.processes))
	for id := range m.processes {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.Stop(id)
	}
}

func (m *Manager) Close() error {
	m.StopAll()
	return m.cli.Close()
}

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
	"github.com/docker/docker/client"
	"github.com/imryao/cli-server/internal/process"
)

const labelManagedBy = "managed-by"
const labelValue = "cli-server"

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

	m.cleanOrphans(ctx)
	return m, nil
}

func (m *Manager) cleanOrphans(ctx context.Context) {
	f := filters.NewArgs(filters.Arg("label", labelManagedBy+"="+labelValue))
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		log.Printf("failed to list orphan containers: %v", err)
		return
	}
	for _, c := range containers {
		log.Printf("cleaning orphan container %s", c.ID[:12])
		m.cli.ContainerStop(ctx, c.ID, container.StopOptions{})
		m.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}

func (m *Manager) Start(id, command string, args, env []string) (process.Process, error) {
	ctx := context.Background()

	containerName := "cli-session-" + id

	// Build environment for the container
	containerEnv := []string{"TERM=xterm-256color"}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			containerEnv = append(containerEnv, key+"="+v)
		}
	}

	pidsLimit := m.cfg.PidsLimit
	resp, err := m.cli.ContainerCreate(ctx,
		&container.Config{
			Image:  m.cfg.Image,
			Env:    containerEnv,
			Labels: map[string]string{labelManagedBy: labelValue},
		},
		&container.HostConfig{
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			NetworkMode:    container.NetworkMode(m.cfg.NetworkMode),
			Resources: container.Resources{
				Memory:   m.cfg.MemoryLimit,
				NanoCPUs: m.cfg.NanoCPUs,
				PidsLimit: &pidsLimit,
			},
		},
		nil, nil, containerName,
	)
	if err != nil {
		return nil, fmt.Errorf("container create: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("container start: %w", err)
	}

	// Use docker exec -it to run the command inside the container via a real PTY
	execArgs := []string{"exec", "-it", resp.ID, command}
	execArgs = append(execArgs, args...)
	cmd := exec.Command("docker", execArgs...)

	// Pass through env vars requested by caller
	cmd.Env = append(os.Environ(), env...)

	ptyFile, err := pty.Start(cmd)
	if err != nil {
		m.cli.ContainerStop(ctx, resp.ID, container.StopOptions{})
		m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("pty start: %w", err)
	}

	p := &containerProcess{
		containerID: resp.ID,
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

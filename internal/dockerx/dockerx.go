// Package dockerx wraps the Docker Engine API client with the small set of
// operations Drydock needs: per-card containers with persistent named
// volumes. Closing a card's container only detaches it; idle stop removes the
// container but keeps the volume — files persist, running processes don't.
package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

const (
	// NetworkName is the bridge network all Drydock containers join.
	NetworkName = "drydock"
	// LabelCard marks containers managed by Drydock with their card ID.
	LabelCard = "drydock.card"
	// LabelManaged marks containers managed by Drydock.
	LabelManaged = "drydock.managed"
	// WorkspaceDir is where the card's persistent volume is mounted.
	WorkspaceDir = "/workspace"
)

// Client wraps *client.Client.
type Client struct {
	cli *client.Client
}

// ResolveHost determines the container runtime endpoint:
// DRYDOCK_DOCKER_HOST wins, then DOCKER_HOST, then the default docker socket.
// A bare filesystem path (no "://" scheme, e.g. /var/run/podman/podman.sock)
// is treated as a unix socket.
func ResolveHost() string {
	h := os.Getenv("DRYDOCK_DOCKER_HOST")
	if h == "" {
		h = os.Getenv("DOCKER_HOST")
	}
	if h == "" {
		h = "unix:///var/run/docker.sock"
	}
	if !strings.Contains(h, "://") && (strings.HasPrefix(h, "/") || strings.HasPrefix(h, ".")) {
		h = "unix://" + h
	}
	return h
}

// CheckUnixSocket dials host when it is a unix:// socket and returns a
// precise, actionable error instead of a generic "not reachable".
func CheckUnixSocket(host string) error {
	if !strings.HasPrefix(host, "unix://") {
		return nil
	}
	path := strings.TrimPrefix(host, "unix://")
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("socket %s does not exist — is the runtime API service running? (Docker: dockerd; Podman: `sudo systemctl enable --now podman.socket` or `podman system service`)", path)
		case errors.Is(err, fs.ErrPermission):
			return fmt.Errorf("permission denied opening %s — run Drydock as a user with socket access (root, or member of the docker/podman group)", path)
		default:
			return fmt.Errorf("cannot connect to %s: %v", path, err)
		}
	}
	_ = conn.Close()
	return nil
}

// New creates a Docker client from the environment with API version
// negotiation.
func New() (*Client, error) {
	os.Setenv("DOCKER_HOST", ResolveHost())
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Ping verifies the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

// ContainerName returns the deterministic container name for a card.
func ContainerName(cardID string) string { return "drydock-" + shortID(cardID) }

// VolumeName returns the deterministic persistent volume name for a card.
func VolumeName(cardID string) string { return "drydock-card-" + shortID(cardID) }

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// EnsureNetwork creates the "drydock" bridge network if it does not exist.
func (c *Client) EnsureNetwork(ctx context.Context) error {
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", "^"+NetworkName+"$")),
	})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == NetworkName {
			return nil
		}
	}
	_, err = c.cli.NetworkCreate(ctx, NetworkName, network.CreateOptions{Driver: "bridge"})
	if err != nil {
		return fmt.Errorf("create network: %w", err)
	}
	return nil
}

// ImageExists reports whether the image is present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) bool {
	_, err := c.cli.ImageInspect(ctx, ref)
	return err == nil
}

// PullImage pulls ref, falling back to a local copy if the pull fails but the
// image exists locally.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	rc, err := c.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		if c.ImageExists(ctx, ref) {
			return nil // offline but usable
		}
		return fmt.Errorf("pull image %q: %w", ref, err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		if c.ImageExists(ctx, ref) {
			return nil
		}
		return fmt.Errorf("pull image %q: %w", ref, err)
	}
	return nil
}

// CreateOptions describes a task container to create.
type CreateOptions struct {
	CardID   string
	Image    string
	CPU      float64 // cores
	MemoryMB int
}

// CreateAndStart creates the card's persistent volume (if needed), creates an
// unprivileged, resource-limited container running "sleep infinity" as its
// main process, and starts it. Agents run later via exec, so no image
// modification or published ports are needed.
func (c *Client) CreateAndStart(ctx context.Context, opts CreateOptions) (string, error) {
	if err := c.EnsureNetwork(ctx); err != nil {
		return "", err
	}
	volName := VolumeName(opts.CardID)
	if _, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   volName,
		Labels: map[string]string{LabelCard: opts.CardID, LabelManaged: "true"},
	}); err != nil && !isAlreadyExists(err) {
		return "", fmt.Errorf("create volume: %w", err)
	}

	pidsLimit := int64(1024)
	hostCfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: volName, Target: WorkspaceDir},
		},
		Resources: container.Resources{
			NanoCPUs:  int64(opts.CPU * 1e9),
			Memory:    int64(opts.MemoryMB) << 20,
			PidsLimit: &pidsLimit,
		},
		RestartPolicy: container.RestartPolicy{Name: "no"},
		AutoRemove:    false,
	}
	cfg := &container.Config{
		Image:      opts.Image,
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: WorkspaceDir,
		Env:        []string{"TERM=xterm-256color"},
		Labels: map[string]string{
			LabelCard:    opts.CardID,
			LabelManaged: "true",
		},
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {},
		},
	}
	name := ContainerName(opts.CardID)
	resp, err := c.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	return resp.ID, nil
}

// Exec runs cmd in the container and returns its combined output.
func (c *Client) Exec(ctx context.Context, containerID string, cmd ...string) (string, error) {
	execResp, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   WorkspaceDir,
	})
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	attach, err := c.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()
	out, _ := io.ReadAll(attach.Reader)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		insp, err := c.cli.ContainerExecInspect(ctx, execResp.ID)
		if err == nil && !insp.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Strip Docker's multiplexed stream headers when present.
	return string(demux(out)), nil
}

// demux strips the 8-byte Docker stream headers from multiplexed output.
func demux(b []byte) []byte {
	var out bytes.Buffer
	for len(b) >= 8 {
		n := int(b[4])<<24 | int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		if n < 0 || 8+n > len(b) {
			break
		}
		out.Write(b[8 : 8+n])
		b = b[8+n:]
	}
	if out.Len() == 0 {
		return b
	}
	return out.Bytes()
}

// StopAndRemove stops (10s grace) and force-removes a container. The volume is
// kept so the card's /workspace survives restarts.
func (c *Client) StopAndRemove(ctx context.Context, containerID string) error {
	timeout := 10
	_ = c.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	if err := c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove container: %w", err)
	}
	return nil
}

// RemoveVolume removes a named volume.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	if err := c.cli.VolumeRemove(ctx, name, false); err != nil {
		return fmt.Errorf("remove volume: %w", err)
	}
	return nil
}

// ManagedVolume is a Drydock-managed Docker volume (label drydock.managed).
type ManagedVolume struct {
	Name      string
	CardID    string
	CreatedAt time.Time
}

// ListManagedVolumes returns all volumes carrying the drydock.managed label.
func (c *Client) ListManagedVolumes(ctx context.Context) ([]ManagedVolume, error) {
	resp, err := c.cli.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelManaged+"=true")),
	})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	out := make([]ManagedVolume, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		if v == nil {
			continue
		}
		mv := ManagedVolume{Name: v.Name}
		if v.Labels != nil {
			mv.CardID = v.Labels[LabelCard]
		}
		if v.CreatedAt != "" {
			if ts, err := time.Parse(time.RFC3339Nano, v.CreatedAt); err == nil {
				mv.CreatedAt = ts
			}
		}
		out = append(out, mv)
	}
	return out, nil
}

// FindCardContainer returns the container ID and running state for the
// container labeled with the given card ID, or "" if none exists.
func (c *Client) FindCardContainer(ctx context.Context, cardID string) (string, bool, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelCard+"="+cardID)),
	})
	if err != nil {
		return "", false, fmt.Errorf("list containers: %w", err)
	}
	for _, cntr := range containers {
		if cntr.State == "running" {
			return cntr.ID, true, nil
		}
	}
	if len(containers) > 0 {
		return containers[0].ID, false, nil
	}
	return "", false, nil
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

package buildkit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containerd/platforms"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	plugin_exec "github.com/thegeeklab/wp-plugin-go/v6/exec"
)

const (
	rootlesskitBin = "/usr/bin/rootlesskit"
	buildkitdBin   = "/usr/bin/buildkitd"
)

// Daemon defines Docker daemon parameters.
type Daemon struct {
	Registry           string
	Mirror             string
	Insecure           bool
	StorageDriver      string
	StoragePath        string
	Disabled           bool
	Debug              bool
	Bip                string
	DNS                []string
	DNSSearch          []string
	MTU                string
	IPv6               bool
	Experimental       bool
	BuildkitConfigFile string
	SocketPath         string
}

// helper function to create the docker daemon command.
func (d *Daemon) Start() error {
	// Determine socket path
	xdgRuntimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntimeDir == "" {
		xdgRuntimeDir = "/run/user/1000"
	}

	// Ensure the buildkit directory exists
	buildkitDir := filepath.Join(xdgRuntimeDir, "buildkit")
	if err := os.MkdirAll(buildkitDir, 0o755); err != nil {
		return fmt.Errorf("failed to create buildkit directory: %w", err)
	}

	d.SocketPath = filepath.Join(buildkitDir, "buildkitd.sock")

	args := []string{
		buildkitdBin,
		"--oci-worker-no-process-sandbox",
	}

	if d.BuildkitConfigFile != "" {
		args = append(args, "--config", d.BuildkitConfigFile)
	}

	cmd := plugin_exec.Command(rootlesskitBin, args...)

	// Discard stdout and stderr using io.Discard
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start buildkitd: %w", err)
	}

	return nil
}

// sortedKeys returns a sorted slice of map keys.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// joinPlatforms creates a comma-separated string of platform names.
func joinPlatforms(p []ocispecs.Platform) string {
	str := make([]string, 0, len(p))

	for _, pp := range p {
		str = append(str, platforms.Format(platforms.Normalize(pp)))
	}

	return strings.Join(str, ",")
}

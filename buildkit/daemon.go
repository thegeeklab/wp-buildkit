package buildkit

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/zerolog/log"
	plugin_exec "github.com/thegeeklab/wp-plugin-go/v6/exec"
	"golang.org/x/sync/errgroup"
)

const (
	rootlesskitBin = "/usr/bin/rootlesskit"
	buildkitdBin   = "/usr/bin/buildkitd"
)

// Daemon defines Docker daemon parameters.
type Daemon struct {
	Registry             string   // Docker registry
	Mirror               string   // Docker registry mirror
	Insecure             bool     // Docker daemon enable insecure registries
	StorageDriver        string   // Docker daemon storage driver
	StoragePath          string   // Docker daemon storage path
	Disabled             bool     // DOcker daemon is disabled (already running)
	Debug                bool     // Docker daemon started in debug mode
	Bip                  string   // Docker daemon network bridge IP address
	DNS                  []string // Docker daemon dns server
	DNSSearch            []string // Docker daemon dns search domain
	MTU                  string   // Docker daemon mtu setting
	IPv6                 bool     // Docker daemon IPv6 networking
	Experimental         bool     // Docker daemon enable experimental mode
	BuildkitConfigFile   string   // Docker buildkit config file
	MaxConcurrentUploads string   // Docker daemon max concurrent uploads

	client     *client.Client
	socketPath string
}

// helper function to create the docker daemon command.
func (d *Daemon) Start(ctx context.Context) error {
	var err error

	// Determine socket path
	xdgRuntimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if xdgRuntimeDir == "" {
		xdgRuntimeDir = "/run/user/1000"
		log.Warn().Msgf("XDG_RUNTIME_DIR not set, defaulting to %s", xdgRuntimeDir)
	}

	// Ensure the buildkit directory exists
	buildkitDir := filepath.Join(xdgRuntimeDir, "buildkit")
	if err := os.MkdirAll(buildkitDir, 0o755); err != nil {
		return fmt.Errorf("failed to create buildkit directory: %w", err)
	}

	d.socketPath = filepath.Join(buildkitDir, "buildkitd.sock")

	log.Info().Msg("Starting BuildKit daemon...")

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

	// Wait for daemon to be ready
	log.Info().Msg("Waiting for BuildKit daemon to be ready...")
	if err := d.waitForSocket(ctx); err != nil {
		return fmt.Errorf("buildkitd socket was not ready in time: %w", err)
	}

	buildkitHost := "unix://" + d.socketPath

	// Create BuildKit client
	d.client, err = client.New(ctx, buildkitHost)
	if err != nil {
		return fmt.Errorf("failed to create buildkit client: %w", err)
	}

	return nil
}

func (d *Daemon) ListBuilder(ctx context.Context) error {
	// List and display workers
	workers, err := d.client.ListWorkers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list workers: %w", err)
	}

	// Display worker information
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	for _, wi := range workers {
		fmt.Fprintln(tw)
		fmt.Fprintf(tw, "ID:\t%s\n", wi.ID)
		fmt.Fprintf(tw, "VERSION:\t%s rev:%s\n", wi.BuildkitVersion.Version, wi.BuildkitVersion.Revision)
		fmt.Fprintf(tw, "Platforms:\t%s\n", joinPlatforms(wi.Platforms))
		fmt.Fprintln(tw, "Labels:")
		for _, k := range sortedKeys(wi.Labels) {
			v := wi.Labels[k]
			fmt.Fprintf(tw, "\t%s:\t%s\n", k, v)
		}
		if d.Debug {
			for i, rule := range wi.GCPolicy {
				fmt.Fprintf(tw, "GC Policy rule#%d:\n", i)
				fmt.Fprintf(tw, "\tAll:\t%v\n", rule.All)
				if len(rule.Filter) > 0 {
					fmt.Fprintf(tw, "\tFilters:\t%s\n", strings.Join(rule.Filter, " "))
				}
				if rule.KeepDuration > 0 {
					fmt.Fprintf(tw, "\tKeep Duration:\t%s\n", rule.KeepDuration.String())
				}
			}
		}
	}
	fmt.Fprintln(tw)
	tw.Flush()

	if err := tw.Flush(); err != nil {
		log.Error().Err(err).Msg("Error flushing worker output")
	}

	return nil
}

func (d *Daemon) Build(ctx context.Context, opts *client.SolveOpt) error {
	log.Info().Msg("Connecting to BuildKit daemon and starting build...")

	// Set up build execution
	ch := make(chan *client.SolveStatus)
	eg, gCtx := errgroup.WithContext(ctx)

	// Goroutine 1: Run the build
	eg.Go(func() error {
		_, err := d.client.Solve(gCtx, nil, *opts, ch)
		return err
	})

	// Goroutine 2: Display progress
	eg.Go(func() error {
		dp, err := progressui.NewDisplay(os.Stdout, progressui.AutoMode)
		if err != nil {
			return fmt.Errorf("failed to create progress display: %w", err)
		}
		_, err = dp.UpdateFrom(gCtx, ch)
		return err
	})

	// Wait for completion
	if err := eg.Wait(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	log.Info().Msg("Build completed successfully!")

	return nil
}

// waitForSocket waits for a Unix socket to become available.
func (d *Daemon) waitForSocket(ctx context.Context) error {
	const dialTimeout = 1 * time.Second
	const daemonBackoffMaxRetries = 3
	const daemonBackoffInitialInterval = 2 * time.Second
	const daemonBackoffMultiplier = 3.5

	bf := backoff.NewExponentialBackOff()
	bf.InitialInterval = daemonBackoffInitialInterval
	bf.Multiplier = daemonBackoffMultiplier

	bfo := func() (any, error) {
		if _, err := os.Stat(d.socketPath); err != nil {
			return nil, err
		}

		// Check if the socket is connectable
		conn, err := net.DialTimeout("unix", d.socketPath, dialTimeout)
		if err != nil {
			return nil, err
		}

		conn.Close()
		return nil, nil
	}

	bfn := func(err error, delay time.Duration) {
		log.Warn().Msgf("Failed access socket: %v: retry in %s", err, delay.Truncate(time.Second))
	}

	_, err := backoff.Retry(ctx, bfo,
		backoff.WithBackOff(bf),
		backoff.WithMaxTries(daemonBackoffMaxRetries),
		backoff.WithNotify(bfn))
	if err != nil {
		return err
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

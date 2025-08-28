package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/zerolog/log"
	plugin_tag "github.com/thegeeklab/wp-plugin-go/v6/tag"
	"golang.org/x/sync/errgroup"
)

var ErrTypeAssertionFailed = errors.New("type assertion failed")

const (
	strictFilePerm               = 0o600
	daemonBackoffMaxRetries      = 3
	daemonBackoffInitialInterval = 2 * time.Second
	daemonBackoffMultiplier      = 3.5
)

func (p *Plugin) run(ctx context.Context) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	if err := p.Execute(ctx); err != nil {
		return fmt.Errorf("execution failed: %w", err)
	}

	return nil
}

// Validate handles the settings validation of the plugin.
func (p *Plugin) Validate() error {
	var err error

	p.Settings.Build.Time = time.Now().Format(time.RFC3339)
	p.Settings.Build.Branch = p.Metadata.Repository.Branch
	p.Settings.Build.Ref = p.Metadata.Curr.Ref
	p.Settings.Daemon.Registry = p.Settings.Registry.Address

	if p.Settings.Build.TagsAuto {
		// Check if tag event or default branch
		if plugin_tag.IsTaggable(p.Settings.Build.Ref, p.Settings.Build.Branch) {
			p.Settings.Build.Tags, err = plugin_tag.SemverTagSuffix(
				p.Settings.Build.Ref,
				p.Settings.Build.TagsSuffix,
				true,
			)
			if err != nil {
				return fmt.Errorf("cannot generate tags from %s, invalid semantic version: %w", p.Settings.Build.Ref, err)
			}
		} else {
			log.Info().Msgf("skip auto-tagging for %s, not on default branch or tag", p.Settings.Build.Ref)
			return nil
		}
	}

	if p.Settings.Build.LabelsAuto {
		p.Settings.Build.Labels = p.GenerateLabels()
	}

	return nil
}

// Execute provides the implementation of the plugin.
func (p *Plugin) Execute(ctx context.Context) error {
	log.Info().Msg("Starting BuildKit daemon...")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Configure and start the BuildKit daemon
	daemonCmd := exec.CommandContext(ctx, "rootlesskit", "buildkitd", "--oci-worker-no-process-sandbox")

	// Discard stdout and stderr using io.Discard
	daemonCmd.Stdout = io.Discard
	daemonCmd.Stderr = io.Discard

	if err := daemonCmd.Start(); err != nil {
		return fmt.Errorf("failed to start buildkitd: %w", err)
	}

	// Ensure the daemon is terminated when the function exits
	defer func() {
		log.Info().Msg("Shutting down BuildKit daemon...")
		cancel()
		if err := daemonCmd.Wait(); err != nil {
			log.Error().Err(err).Msg("Error waiting for daemon to exit")
		}
		log.Info().Msg("Daemon shut down successfully.")
	}()

	// Handle graceful shutdown on interrupt signals (Ctrl+C)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Info().Msg("Interrupt signal received, shutting down.")
		cancel()
	}()

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

	socketPath := filepath.Join(buildkitDir, "buildkitd.sock")
	buildkitHost := "unix://" + socketPath

	// Wait for daemon to be ready
	log.Info().Msg("Waiting for BuildKit daemon to be ready...")
	if err := waitForSocket(ctx, socketPath, 10); err != nil {
		return fmt.Errorf("buildkitd socket was not ready in time: %w", err)
	}
	log.Info().Msg("BuildKit daemon is ready.")

	// Prepare build options
	solveOpt, err := p.constructSolveOpts()
	if err != nil {
		return fmt.Errorf("failed to construct build options: %w", err)
	}

	// Create BuildKit client
	bkClient, err := client.New(ctx, buildkitHost)
	if err != nil {
		return fmt.Errorf("failed to create buildkit client: %w", err)
	}
	defer func() {
		if err := bkClient.Close(); err != nil {
			log.Error().Err(err).Msg("Error closing BuildKit client")
		}
	}()

	// List and display workers
	workers, err := bkClient.ListWorkers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list workers: %w", err)
	}

	// Display worker information
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	p.printWorkers(tw, workers)
	if err := tw.Flush(); err != nil {
		log.Error().Err(err).Msg("Error flushing worker output")
	}

	log.Info().Msg("Connecting to BuildKit daemon and starting build...")

	// Set up build execution
	ch := make(chan *client.SolveStatus)
	eg, gCtx := errgroup.WithContext(ctx)

	// Goroutine 1: Run the build
	eg.Go(func() error {
		_, err := bkClient.Solve(gCtx, nil, *solveOpt, ch)
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

	// // start the Docker daemon server
	// //nolint: nestif
	// if !p.Settings.Daemon.Disabled {
	// 	// If no custom DNS value set start internal DNS server
	// 	if len(p.Settings.Daemon.DNS) == 0 {
	// 		ip, err := GetContainerIP()
	// 		if err != nil {
	// 			log.Warn().Msgf("error detecting IP address: %v", err)
	// 		}

	// 		if ip != "" {
	// 			log.Debug().Msgf("discovered IP address: %v", ip)

	// 			cmd := p.Settings.Daemon.StartCoreDNS()

	// 			go func() {
	// 				_ = cmd.Run()
	// 			}()

	// 			p.Settings.Daemon.DNS = append(p.Settings.Daemon.DNS, ip)
	// 		}
	// 	}

	// 	cmd := p.Settings.Daemon.Start()

	// 	go func() {
	// 		_ = cmd.Run()
	// 	}()
	// }

	// // poll the docker daemon until it is started. This ensures the daemon is
	// // ready to accept connections before we proceed.
	// for i := 0; i < 15; i++ {
	// 	cmd := docker.Info()

	// 	err := cmd.Run()
	// 	if err == nil {
	// 		break
	// 	}

	// 	time.Sleep(time.Second * 1)
	// }

	// if p.Settings.Registry.Config != "" {
	// 	path := filepath.Join(homeDir, ".docker", "config.json")
	// 	if err := os.MkdirAll(filepath.Dir(path), strictFilePerm); err != nil {
	// 		return err
	// 	}

	// 	if err := WriteDockerConf(path, p.Settings.Registry.Config); err != nil {
	// 		return fmt.Errorf("error writing docker config: %w", err)
	// 	}
	// }

	// if p.Settings.Registry.Password != "" {
	// 	if err := p.Settings.Registry.Login().Run(); err != nil {
	// 		return fmt.Errorf("error authenticating: %w", err)
	// 	}
	// }

	// buildkitConf := p.Settings.BuildkitConfig
	// if buildkitConf != "" {
	// 	if p.Settings.Daemon.BuildkitConfigFile, err = plugin_file.WriteTmpFile("buildkit.toml", buildkitConf); err != nil {
	// 		return fmt.Errorf("error writing buildkit config: %w", err)
	// 	}

	// 	defer os.Remove(p.Settings.Daemon.BuildkitConfigFile)
	// }

	// switch {
	// case p.Settings.Registry.Password != "":
	// 	log.Info().Msgf("Detected registry credentials")
	// case p.Settings.Registry.Config != "":
	// 	log.Info().Msgf("Detected registry credentials file")
	// default:
	// 	log.Info().Msgf("Registry credentials or Docker config not provided. Guest mode enabled.")
	// }

	// p.Settings.Build.AddProxyBuildArgs()

	// bf := backoff.NewExponentialBackOff()
	// bf.InitialInterval = daemonBackoffInitialInterval
	// bf.Multiplier = daemonBackoffMultiplier

	// bfo := func() (any, error) {
	// 	return nil, docker.Version().Run()
	// }

	// bfn := func(err error, delay time.Duration) {
	// 	log.Error().Msgf("failed to run docker version command: %v: retry in %s", err, delay.Truncate(time.Second))
	// }

	// _, err = backoff.Retry(ctx, bfo,
	// 	backoff.WithBackOff(bf),
	// 	backoff.WithMaxTries(daemonBackoffMaxRetries),
	// 	backoff.WithNotify(bfn))
	// if err != nil {
	// 	return err
	// }

	// batchCmd = append(batchCmd, docker.Info())
	// batchCmd = append(batchCmd, p.Settings.Daemon.CreateBuilder())
	// batchCmd = append(batchCmd, p.Settings.Daemon.ListBuilder())
	// batchCmd = append(batchCmd, p.Settings.Build.Run(p.Environment.Value()))

	// for _, cmd := range batchCmd {
	// 	if cmd == nil {
	// 		continue
	// 	}

	// 	if err := cmd.Run(); err != nil {
	// 		return err
	// 	}
	// }

	// return nil
}

// constructSolveOpts creates the SolveOpt structure for BuildKit.
func (p *Plugin) constructSolveOpts() (*client.SolveOpt, error) {
	repo := p.Settings.Build.Repo
	tags := p.Settings.Build.Tags
	var imageNames []string

	for _, tag := range tags {
		imageNames = append(imageNames, fmt.Sprintf("%s:%s", repo, tag))
	}

	exports := []client.ExportEntry{
		{
			Type: client.ExporterImage,
			Attrs: map[string]string{
				"name": strings.Join(imageNames, ","),
				"push": fmt.Sprintf("%t", p.Settings.Build.Dryrun),
			},
		},
	}

	frontendAttrs := map[string]string{
		"filename": p.Settings.Build.Containerfile,
	}
	if len(p.Settings.Build.Platforms) > 0 {
		frontendAttrs["platform"] = strings.Join(p.Settings.Build.Platforms, ",")
	}

	opt := &client.SolveOpt{
		Exports: exports,
		LocalDirs: map[string]string{
			"context":    p.Settings.Build.Context,
			"dockerfile": p.Settings.Build.Context,
		},
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
	}

	return opt, nil
}

// waitForSocket waits for a Unix socket to become available.
func waitForSocket(ctx context.Context, path string, maxRetries int) error {
	const retryInterval = 500 * time.Millisecond
	const dialTimeout = 1 * time.Second

	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
			// Check if the file exists
			if _, err := os.Stat(path); err == nil {
				// Check if the socket is connectable
				conn, err := net.DialTimeout("unix", path, dialTimeout)
				if err == nil {
					conn.Close()
					return nil // Success!
				}
				log.Debug().Err(err).Msgf("Socket not yet connectable (attempt %d/%d)", i+1, maxRetries)
			} else {
				log.Debug().Err(err).Msgf("Socket file not found (attempt %d/%d)", i+1, maxRetries)
			}
		}
	}
	return fmt.Errorf("socket not available after %d retries", maxRetries)
}

// printWorkers displays information about BuildKit workers.
func (p *Plugin) printWorkers(tw *tabwriter.Writer, winfo []*client.WorkerInfo) {
	for _, wi := range winfo {
		fmt.Fprintln(tw)
		fmt.Fprintf(tw, "ID:\t%s\n", wi.ID)
		fmt.Fprintf(tw, "VERSION:\t%s rev:%s\n", wi.BuildkitVersion.Version, wi.BuildkitVersion.Revision)
		fmt.Fprintf(tw, "Platforms:\t%s\n", joinPlatforms(wi.Platforms))
		fmt.Fprintln(tw, "Labels:")
		for _, k := range sortedKeys(wi.Labels) {
			v := wi.Labels[k]
			fmt.Fprintf(tw, "\t%s:\t%s\n", k, v)
		}
		if p.Settings.Daemon.Debug {
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

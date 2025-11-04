package buildkit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	"golang.org/x/sync/errgroup"
)

type Client struct {
	BuildData BuildData

	debug  bool
	client *client.Client
}

// Build defines Docker build parameters.
type BuildData struct {
	Ref           string            // Git commit ref
	Branch        string            // Git repository branch
	Containerfile string            // Docker build Containerfile
	Context       string            // Docker build context
	TagsAuto      bool              // Docker build auto tag
	TagsSuffix    string            // Docker build tags with suffix
	Tags          []string          // Docker build tags
	ExtraTags     []string          // Docker build tags including registry
	Platforms     []string          // Docker build target platforms
	Args          map[string]string // Docker build args
	ArgsEnv       []string          // Docker build args from env
	Target        string            // Docker build target
	Pull          bool              // Docker build pull
	CacheFrom     []string          // Docker build cache-from
	CacheTo       string            // Docker build cache-to
	Compress      bool              // Docker build compress
	Repo          string            // Docker build repository
	NoCache       bool              // Docker build no-cache
	AddHost       []string          // Docker build add-host
	Quiet         bool              // Docker build quiet
	Output        string            // Docker build output folder
	NamedContext  []string          // Docker build named context
	Labels        []string          // Docker build labels
	LabelsAuto    bool              // Docker build labels auto
	Provenance    string            // Docker build provenance attestation
	SBOM          string            // Docker build sbom attestation
	Secrets       []string          // Docker build secrets
	Dryrun        bool              // Docker build dryrun
	Time          string            // Docker build time
}

func (c *Client) Info(ctx context.Context) error {
	// List and display workers
	workers, err := c.client.ListWorkers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list workers: %w", err)
	}

	// Display worker information
	//nolint:mnd
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

		if c.debug {
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
		return fmt.Errorf("failed to show info: %w", err)
	}

	return nil
}

func New(ctx context.Context, data BuildData, socketPath string, debug bool) (*Client, error) {
	buildkitHost := "unix://" + socketPath

	// Create BuildKit client
	c, err := client.New(ctx, buildkitHost)
	if err != nil {
		return nil, fmt.Errorf("failed to create buildkit client: %w", err)
	}

	b := &Client{
		BuildData: data,
		debug:     debug,
		client:    c,
	}

	return b, nil
}

func (c *Client) Build(ctx context.Context) error {
	// Prepare build options
	opts, err := c.GetSolveOpts()
	if err != nil {
		return fmt.Errorf("failed to construct build options: %w", err)
	}

	// Set up build execution
	ch := make(chan *client.SolveStatus)
	eg, gCtx := errgroup.WithContext(ctx)

	// Goroutine 1: Run the build
	eg.Go(func() error {
		_, err := c.client.Solve(gCtx, nil, *opts, ch)

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

	return nil
}

// constructSolveOpts creates the SolveOpt structure for BuildKit.
func (c *Client) GetSolveOpts() (*client.SolveOpt, error) {
	repo := c.BuildData.Repo
	tags := c.BuildData.Tags
	imageNames := make([]string, 0)

	for _, tag := range tags {
		imageNames = append(imageNames, fmt.Sprintf("%s:%s", repo, tag))
	}

	exports := []client.ExportEntry{
		{
			Type: client.ExporterImage,
			Attrs: map[string]string{
				"name": strings.Join(imageNames, ","),
				"push": fmt.Sprintf("%t", c.BuildData.Dryrun),
			},
		},
	}

	frontendAttrs := map[string]string{
		"filename": c.BuildData.Containerfile,
	}
	if len(c.BuildData.Platforms) > 0 {
		frontendAttrs["platform"] = strings.Join(c.BuildData.Platforms, ",")
	}

	opt := &client.SolveOpt{
		Exports: exports,
		LocalDirs: map[string]string{
			"context":    c.BuildData.Context,
			"dockerfile": c.BuildData.Context,
		},
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
	}

	return opt, nil
}

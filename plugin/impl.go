package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/rs/zerolog/log"
	plugin_file "github.com/thegeeklab/wp-plugin-go/v6/file"
	plugin_tag "github.com/thegeeklab/wp-plugin-go/v6/tag"
)

var ErrTypeAssertionFailed = errors.New("type assertion failed")

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

	if p.Settings.BuildkitConfig != "" {
		if p.Settings.Daemon.BuildkitConfigFile, err = plugin_file.WriteTmpFile("buildkit.toml", p.Settings.BuildkitConfig); err != nil {
			return fmt.Errorf("error writing buildkit config: %w", err)
		}
	}

	if p.Settings.Build.LabelsAuto {
		p.Settings.Build.Labels = p.GenerateLabels()
	}

	return nil
}

// Execute provides the implementation of the plugin.
func (p *Plugin) Execute(ctx context.Context) error {
	var err error

	if err := p.Settings.Daemon.Start(ctx); err != nil {
		return err
	}
	if err := p.Settings.Daemon.ListBuilder(ctx); err != nil {
		return err
	}
	// if err := p.Settings.Registry.Login(); err != nil {
	// 	return err
	// }

	// Prepare build options
	solveOpt, err := p.constructSolveOpts()
	if err != nil {
		return fmt.Errorf("failed to construct build options: %w", err)
	}

	if err := p.Settings.Daemon.Build(ctx, solveOpt); err != nil {
		return err
	}

	return nil
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

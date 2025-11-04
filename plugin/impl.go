package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/thegeeklab/wp-buildkit/buildkit"
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
		p.Settings.Daemon.BuildkitConfigFile, err = plugin_file.WriteTmpFile("buildkit.toml", p.Settings.BuildkitConfig)
		if err != nil {
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

	log.Info().Msg("Starting BuildKit daemon...")

	if err := p.Settings.Daemon.Start(); err != nil {
		return err
	}

	// Wait for daemon to be ready
	log.Info().Msg("Waiting for BuildKit daemon to be ready...")

	bfn := func(err error, delay time.Duration) {
		log.Warn().Msgf("Failed access socket: %v: retry in %s", err, delay.Truncate(time.Second))
	}
	if err := WaitForSocket(ctx, p.Settings.Daemon.SocketPath, bfn); err != nil {
		return fmt.Errorf("buildkitd socket was not ready in time: %w", err)
	}

	bk, err := buildkit.New(ctx, p.Settings.Build, p.Settings.Daemon.SocketPath, p.Settings.Daemon.Debug)
	if err != nil {
		return err
	}

	if err := bk.Info(ctx); err != nil {
		return err
	}

	if err := p.Settings.Registry.Login(); err != nil {
		return err
	}

	log.Info().Msg("Connecting to BuildKit daemon and starting build...")

	if err := bk.Build(ctx); err != nil {
		return err
	}

	log.Info().Msg("Build completed successfully!")

	return nil
}

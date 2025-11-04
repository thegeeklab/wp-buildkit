package buildkit

import (
	"fmt"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/types"
)

// Login defines Docker login parameters.
type Registry struct {
	Address  string // Docker registry address
	Username string // Docker registry username
	Password string // Docker registry password
	Email    string // Docker registry email
	Config   string // Docker Auth Config
}

// helper function to create the docker login command.
func (r *Registry) Login() error {
	cfg, err := config.Load(r.Config)
	if err != nil {
		return fmt.Errorf("failed to load docker config: %w", err)
	}

	authConfig := types.AuthConfig{
		Username: r.Username,
		Password: r.Password,
	}

	cfg.AuthConfigs[r.Address] = authConfig

	return cfg.Save()
}

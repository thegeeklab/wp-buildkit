package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/cenkalti/backoff/v5"
)

var errInvalidDockerConfig = fmt.Errorf("invalid docker config")

const (
	dialTimeout                  = 1 * time.Second
	daemonBackoffMaxRetries      = 3
	daemonBackoffInitialInterval = 2 * time.Second
	daemonBackoffMultiplier      = 3.5
)

// waitForSocket waits for a Unix socket to become available.
func WaitForSocket(ctx context.Context, path string, bfn func(err error, delay time.Duration)) error {
	// Add initial delay before first attempt
	select {
	case <-time.After(daemonBackoffInitialInterval):
	case <-ctx.Done():
		return ctx.Err()
	}

	bf := backoff.NewExponentialBackOff()
	bf.InitialInterval = daemonBackoffInitialInterval
	bf.Multiplier = daemonBackoffMultiplier

	bfo := func() (any, error) {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}

		// Check if the socket is connectable
		dialer := &net.Dialer{Timeout: dialTimeout}

		conn, err := dialer.DialContext(ctx, "unix", path)
		if err != nil {
			return nil, err
		}

		conn.Close()
		//nolint:nilnil
		return nil, nil
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

func GetContainerIP() (string, error) {
	netInterfaceAddrList, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, netInterfaceAddr := range netInterfaceAddrList {
		netIP, ok := netInterfaceAddr.(*net.IPNet)
		if ok && !netIP.IP.IsLoopback() && netIP.IP.To4() != nil {
			return netIP.IP.String(), nil
		}
	}

	return "", nil
}

func WriteDockerConf(path, conf string) error {
	var jsonData interface{}
	if err := json.Unmarshal([]byte(conf), &jsonData); err != nil {
		return fmt.Errorf("%w: %w", errInvalidDockerConfig, err)
	}

	jsonBytes, err := json.Marshal(jsonData)
	if err != nil {
		return fmt.Errorf("%w: %w", errInvalidDockerConfig, err)
	}

	err = os.WriteFile(path, jsonBytes, 0o600)
	if err != nil {
		return err
	}

	return nil
}

func (p *Plugin) GenerateLabels() []string {
	l := make([]string, 0)

	// As described in https://github.com/opencontainers/image-spec/blob/main/annotations.md
	l = append(l, fmt.Sprintf("org.opencontainers.image.created=%s", p.Settings.Build.Time))

	if p.Settings != nil {
		if tags := p.Settings.Build.Tags; len(tags) > 0 {
			l = append(l, fmt.Sprintf("org.opencontainers.image.version=%s", tags[len(tags)-1]))
		}
	}

	if p.Repository != nil && p.Repository.URL != "" {
		l = append(l, fmt.Sprintf("org.opencontainers.image.source=%s", p.Repository.URL))
		l = append(l, fmt.Sprintf("org.opencontainers.image.url=%s", p.Repository.URL))
	}

	if p.Commit != nil && p.Commit.SHA != "" {
		l = append(l, fmt.Sprintf("org.opencontainers.image.revision=%s", p.Commit.SHA))
	}

	return l
}

package backends

import (
	"fmt"
	"os"
	"strings"

	"github.com/stretchr/testify/assert/yaml"
)

type Config struct {
	PrivateKeyPath string                     `yaml:"private_key_path"`
	Coordinators   []BackendCoordinatorConfig `yaml:"coordinators"`
}

// BackendCoordinatorConfig identifies one backend coordinator instance.
type BackendCoordinatorConfig struct {
	BackendID       uint32 `yaml:"backend_id"`
	LedgerID        uint64 `yaml:"ledger_id"`
	ChainURL        string `yaml:"chainURL"`
	AdjudicatorAddr string `yaml:"adjudicator_addr"`
}

func LoadConfig(path string) (Config, error) {

	if strings.TrimSpace(path) == "" {
		return Config{}, fmt.Errorf("load config: empty path")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("load config: read %s: %w", path, err)
	}

	cfg := Config{
		PrivateKeyPath: "",
		Coordinators:   []BackendCoordinatorConfig{},
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("load config: unmarshal %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("load config: validate %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks required config fields and key uniqueness constraints.
func (c Config) Validate() error {
	if strings.TrimSpace(c.PrivateKeyPath) == "" {
		return fmt.Errorf("load config: private_key_path is required")
	}

	if len(c.Coordinators) == 0 {
		return fmt.Errorf("load config: coordinators is required")
	}
	seen := make(map[string]struct{}, len(c.Coordinators))
	for i, coord := range c.Coordinators {
		k := fmt.Sprintf("%d/%d", coord.BackendID, coord.LedgerID)
		if _, ok := seen[k]; ok {
			return fmt.Errorf("load config: duplicate coordinator key %q", k)
		}
		seen[k] = struct{}{}

		// chainURL must use a WebSocket transport. SubscribeNewHead (used by
		// BlockTimeout.Wait and confirmNTimes in the ETH backend) only works
		// over ws:// or wss://; HTTP transports silently fail at first use.
		url := strings.TrimSpace(coord.ChainURL)
		if url == "" {
			return fmt.Errorf("load config: coordinators[%d]: chainURL is required", i)
		}
		if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
			return fmt.Errorf("load config: coordinators[%d]: chainURL must use ws:// or wss:// (got %q)", i, url)
		}
		if strings.TrimSpace(coord.AdjudicatorAddr) == "" {
			return fmt.Errorf("load config: coordinators[%d]: adjudicator_addr is required", i)
		}
	}
	return nil
}

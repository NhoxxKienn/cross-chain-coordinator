package backends

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

func TestLoadConfig_EmptyPath(t *testing.T) {
	_, err := LoadConfig("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty path")
}

func TestLoadConfig_WhitespacePath(t *testing.T) {
	_, err := LoadConfig("   ")
	assert.Error(t, err)
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	assert.Error(t, err)
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	path := writeTemp(t, ":::not valid yaml:::")
	_, err := LoadConfig(path)
	assert.Error(t, err)
}

func TestLoadConfig_Valid(t *testing.T) {
	path := writeTemp(t, `
private_key_path: ./key.hex
coordinators:
  - backend_id: 1
    ledger_id: 1337
    chainURL: "ws://127.0.0.1:8545"
    adjudicator_addr: "0xABCD"
  - backend_id: 1
    ledger_id: 1338
    chainURL: "ws://127.0.0.1:8546"
    adjudicator_addr: "0xEF01"
`)
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "./key.hex", cfg.PrivateKeyPath)
	assert.Len(t, cfg.Coordinators, 2)
	assert.Equal(t, uint32(1), cfg.Coordinators[0].BackendID)
	assert.Equal(t, uint64(1337), cfg.Coordinators[0].LedgerID)
	assert.Equal(t, "ws://127.0.0.1:8545", cfg.Coordinators[0].ChainURL)
	assert.Equal(t, "0xABCD", cfg.Coordinators[0].AdjudicatorAddr)
}

func TestValidate_MissingPrivateKeyPath(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "",
		Coordinators:   []BackendCoordinatorConfig{{BackendID: 1, LedgerID: 1337}},
	}
	assert.Error(t, cfg.Validate())
}

func TestValidate_WhitespacePrivateKeyPath(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "   ",
		Coordinators:   []BackendCoordinatorConfig{{BackendID: 1, LedgerID: 1337}},
	}
	assert.Error(t, cfg.Validate())
}

func TestValidate_EmptyCoordinators(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators:   []BackendCoordinatorConfig{},
	}
	assert.Error(t, cfg.Validate())
}

func TestValidate_DuplicateKey(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators: []BackendCoordinatorConfig{
			{BackendID: 1, LedgerID: 1337},
			{BackendID: 1, LedgerID: 1337}, // duplicate
		},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestValidate_SameBackendDifferentLedger(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators: []BackendCoordinatorConfig{
			{BackendID: 1, LedgerID: 1337},
			{BackendID: 1, LedgerID: 1338},
		},
	}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_SameLedgerDifferentBackend(t *testing.T) {
	cfg := Config{
		PrivateKeyPath: "./key.hex",
		Coordinators: []BackendCoordinatorConfig{
			{BackendID: 1, LedgerID: 1337},
			{BackendID: 2, LedgerID: 1337},
		},
	}
	assert.NoError(t, cfg.Validate())
}

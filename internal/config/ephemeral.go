package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EphemeralState is what `capydb ephemeral create` leaves behind so `status`
// and `claim` work without arguments. ClaimToken is the database's only
// credential and cannot be recovered from the API, so the file is written 0600
// inside .capydb/, which the CLI keeps git-ignored.
type EphemeralState struct {
	APIURL     string    `json:"api_url"`
	ClaimToken string    `json:"claim_token"`
	ClaimURL   string    `json:"claim_url"`
	EnvFile    string    `json:"env_file,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	Name       string    `json:"name"`
	ProjectID  string    `json:"project_id"`
}

func EphemeralStatePath(cwd string) string {
	return filepath.Join(cwd, ".capydb", "ephemeral.json")
}

// LoadEphemeralState returns os.ErrNotExist when this directory has no
// ephemeral database on record.
func LoadEphemeralState(cwd string) (EphemeralState, error) {
	data, err := os.ReadFile(EphemeralStatePath(cwd))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return EphemeralState{}, os.ErrNotExist
		}
		return EphemeralState{}, fmt.Errorf("read ephemeral state: %w", err)
	}

	var state EphemeralState
	if err := json.Unmarshal(data, &state); err != nil {
		return EphemeralState{}, fmt.Errorf("decode ephemeral state: %w", err)
	}
	state.APIURL = trimURL(firstNonEmpty(state.APIURL, DefaultAPIURL()))
	return state, nil
}

func SaveEphemeralState(cwd string, state EphemeralState) error {
	path := EphemeralStatePath(cwd)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create .capydb directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ephemeral state: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write ephemeral state: %w", err)
	}
	return nil
}

// RemoveEphemeralState forgets the recorded ephemeral database. Removing a file
// that is already gone is not an error.
func RemoveEphemeralState(cwd string) error {
	if err := os.Remove(EphemeralStatePath(cwd)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove ephemeral state: %w", err)
	}
	return nil
}

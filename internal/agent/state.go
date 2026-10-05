package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/rene-roid/kanshi/internal/statefile"
)

const stateFile = "state.json"

// State is what the agent remembers between runs, next to the spool.
type State struct {
	// Boot the agent last ran in; a different one on start means the
	// machine rebooted in between (a `boot` event).
	BootID   string    `json:"boot_id,omitempty"`
	BootTime time.Time `json:"boot_time,omitempty"`
	// Version that last ran, for the `agent_update` event.
	AgentVersion string `json:"agent_version,omitempty"`
	// Last record sequence number handed out.
	Seq uint64 `json:"seq"`

	// For `status`.
	LastPushAt     time.Time `json:"last_push_at,omitempty"`
	LastPushStatus string    `json:"last_push_status,omitempty"` // Outcome.String(): ok, retrying, rejected, ...
	LastPushCode   int       `json:"last_push_code,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastSuccessAt  time.Time `json:"last_success_at,omitempty"`
}

// LoadState reads dir/state.json; a missing or unreadable file is a fresh state.
func LoadState(dir string) State {
	var s State
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// Save writes the state through a rename, so a crash leaves the old one.
func (s State) Save(dir string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return statefile.Write(filepath.Join(dir, stateFile), raw, 0o600)
}

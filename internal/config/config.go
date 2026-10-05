// Package config holds the agent's runtime configuration.
//
// Every setting is an IA_AGENT_* environment variable. The same KEY=VALUE pairs
// can live in an agent.env file, which is where `enroll` saves the server id
// and the agent key, so a service needs no environment at all. Precedence is
// environment, then file, then the built-in default.
//
// Defaults are deliberately conservative: the agent runs on client servers
// that exist to do something else, so it should be invisible in `top`.
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rene-roid/kanshi/internal/statefile"
)

// Setting names written by `enroll`.
const (
	KeyURL      = "IA_AGENT_URL"
	KeyServerID = "IA_AGENT_SERVER_ID"
	KeyAgentKey = "IA_AGENT_KEY"
)

// Module switches. Host vitals are always sent; these two can be turned off.
const (
	KeyDocker = "IA_AGENT_DOCKER"
	KeyDisks  = "IA_AGENT_DISKS"
)

// Config is read once at startup.
type Config struct {
	// Base URL of the ingestion API, e.g. https://api.analytics.infini.es.
	URL string
	// Identity handed out by the backend on enrollment.
	ServerID string
	AgentKey string

	// Where the spool and the agent's own state live. Defaults to the folder
	// holding agent.env.
	StateDir string

	// How often vitals are read. Ten-second windows are built from these.
	SampleInterval time.Duration
	// The window length, and how often a batch is pushed. The backend can
	// ask for a different push cadence (next_push_s).
	WindowInterval time.Duration
	// How often filesystems are measured.
	FilesystemInterval time.Duration

	// Disk space: filesystem readings and mount / unmount events.
	DisksEnabled bool

	// Spool caps: whatever could not be pushed is kept on disk up to both.
	SpoolMaxAge   time.Duration
	SpoolMaxBytes int64

	// Containers: off, or the busiest N (by CPU + memory) per window.
	DockerEnabled  bool
	ContainerLimit int

	// Max concurrent /stats requests against the Docker daemon per window.
	DockerConcurrency int
	// A DOCKER_HOST-style address: unix://, npipe:// or tcp://.
	DockerHost string

	// Filesystems to report. Entries are "auto", "label=path" or just
	// "path"; "auto" is / plus every drive under /mnt on Linux, every fixed
	// drive on Windows.
	FilesystemRoots []string
	// Where the host's root filesystem is mounted when the agent itself runs
	// in a container. Empty on a normal install.
	HostRoot string

	// Overrides the machine id `enroll` reports, for machines cloned from
	// one image. Empty = the machine's own.
	MachineID string

	// The agent.env that was loaded, or where `enroll` writes one.
	File       string
	FileLoaded bool
	// Why File exists but could not be read, nil if it could (or is not
	// there). Its settings are then missing from everything above.
	FileErr error
}

// Enrolled reports whether the agent has an identity to push with.
func (c Config) Enrolled() bool {
	return c.URL != "" && c.ServerID != "" && c.AgentKey != ""
}

// Load reads the environment and agent.env (explicit is a path from the
// command line, "" for the default). Unparseable values fall back to the
// default rather than refusing to start - a typo in one knob should not stop
// the monitoring.
func Load(explicit string) Config {
	path, loaded := findFile(explicit)
	var file map[string]string
	var fileErr error
	if loaded {
		if file, fileErr = ReadFile(path); fileErr != nil {
			fileErr = statefile.ReadError(path, fileErr)
		}
	} else if _, err := os.Stat(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fileErr = statefile.ReadError(path, err)
	}
	l := lookup{file: file}

	stateDir := l.string("IA_AGENT_STATE_DIR", "")
	if stateDir == "" && path != "" {
		stateDir = filepath.Dir(path)
	}

	return Config{
		URL:                strings.TrimRight(l.string(KeyURL, ""), "/"),
		ServerID:           l.string(KeyServerID, ""),
		AgentKey:           l.string(KeyAgentKey, ""),
		StateDir:           stateDir,
		SampleInterval:     l.seconds("IA_AGENT_SAMPLE_INTERVAL", 2*time.Second),
		WindowInterval:     l.seconds("IA_AGENT_WINDOW", 10*time.Second),
		FilesystemInterval: l.seconds("IA_AGENT_FS_INTERVAL", time.Minute),
		SpoolMaxAge:        l.seconds("IA_AGENT_SPOOL_MAX_AGE", 48*time.Hour),
		SpoolMaxBytes:      int64(l.int("IA_AGENT_SPOOL_MAX_MB", 65)) << 20,
		DisksEnabled:       l.bool(KeyDisks, true),
		DockerEnabled:      l.bool(KeyDocker, true),
		ContainerLimit:     l.int("IA_AGENT_CONTAINER_LIMIT", 50),
		DockerConcurrency:  l.int("IA_AGENT_DOCKER_CONCURRENCY", 4),
		DockerHost:         l.dockerHost(),
		FilesystemRoots:    l.list("IA_AGENT_FS_ROOTS", "auto"),
		HostRoot:           strings.TrimRight(l.string("IA_AGENT_HOST_ROOT", ""), `/\`),
		MachineID:          l.string(KeyMachineID, ""),
		File:               path,
		FileLoaded:         loaded,
		FileErr:            fileErr,
	}
}

// lookup resolves one setting against the environment, then the file.
type lookup struct {
	file map[string]string
}

func (l lookup) get(name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return strings.TrimSpace(l.file[name])
}

// dockerHost honours DOCKER_HOST like the docker CLI does, then the platform
// default.
func (l lookup) dockerHost() string {
	if v := l.get("IA_AGENT_DOCKER_HOST"); v != "" {
		return v
	}
	if v := l.get("DOCKER_HOST"); v != "" {
		return v
	}
	return defaultDockerHost
}

func (l lookup) string(name, def string) string {
	if v := l.get(name); v != "" {
		return v
	}
	return def
}

func (l lookup) int(name string, def int) int {
	n, err := strconv.Atoi(l.get(name))
	if err != nil {
		return def
	}
	return n
}

func (l lookup) bool(name string, def bool) bool {
	b, ok := ParseSwitch(l.get(name))
	if !ok {
		return def
	}
	return b
}

// ParseSwitch reads an on/off value: true/false, 1/0, on/off or yes/no.
func ParseSwitch(s string) (on, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "on", "yes", "y":
		return true, true
	case "0", "f", "false", "off", "no", "n":
		return false, true
	}
	return false, false
}

// seconds accepts a bare number of seconds.
func (l lookup) seconds(name string, def time.Duration) time.Duration {
	n, err := strconv.ParseFloat(l.get(name), 64)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n * float64(time.Second))
}

func (l lookup) list(name, def string) []string {
	raw := l.get(name)
	if raw == "" {
		raw = def
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DockerHostIsDefault reports whether DockerHost is the platform's own
// socket / pipe rather than an address someone configured.
func (c Config) DockerHostIsDefault() bool { return c.DockerHost == defaultDockerHost }

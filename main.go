// Command infinianalytics-agent reports this server's health to InfiniAnalytics:
// CPU, memory, disks, network and Docker containers every 10 seconds, plus
// why the machine went away when it does (reboot, shutdown, crash).
//
//	infinianalytics-agent enroll <code> [--url URL]   link this server (code from the dashboard)
//	infinianalytics-agent install [--set KEY=VALUE]   run it as a service (systemd / Windows)
//	infinianalytics-agent run                         run in the foreground (what the service runs)
//	infinianalytics-agent status                      last push, spool size
//	infinianalytics-agent uninstall | start | stop | version
//
// Host vitals are always sent. Disk space and Docker are modules, on by
// default: --disks off / --docker off (or IA_AGENT_DISKS / IA_AGENT_DOCKER).
// The other adjustable settings (config.Settings) go through --set KEY=VALUE;
// --reset puts every one not given back to its default.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/rene-roid/kanshi/internal/agent"
	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/service"
	"github.com/rene-roid/kanshi/internal/statefile"
)

// version is stamped by the release build with -ldflags "-X main.version=…".
var version = "dev"

const defaultURL = "https://api.analytics.infini.es"

func main() {
	if service.IsService() {
		// Started by the Windows service control manager.
		if err := service.Run(runAgent("")); err != nil {
			logf("%v", err)
			os.Exit(1)
		}
		return
	}

	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "", "settings file (default: "+config.FileName+" next to the program, then "+filepath.Join(config.DefaultDir(), config.FileName)+")")
	url := fs.String("url", "", "ingestion API base URL (enroll only; default "+defaultURL+")")
	settings := map[string]string{}
	fs.Var(moduleFlag{config.KeyDisks, settings}, "disks", "disk space readings: on or off")
	fs.Var(moduleFlag{config.KeyDocker, settings}, "docker", "Docker containers and events: on or off")
	fs.Var(settingFlag(settings), "set", "adjustable setting KEY=VALUE, repeatable (an empty VALUE means the default)")
	reset := fs.Bool("reset", false, "enroll / install: every adjustable setting not given goes back to its default")
	machineID := fs.String("machine-id", "", "enroll only: machine id to report, for servers cloned from one image")
	fs.Usage = usage

	switch cmd {
	case "run":
		fs.Parse(args)
		// For this process only. The environment beats agent.env, so the
		// flags win over both.
		for key, value := range settings {
			if value != "" {
				os.Setenv(key, value)
			}
		}
		resourceDefaults()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runAgent(*cfgPath)(ctx, func() string { return "" }); err != nil {
			fail(err)
		}
	case "enroll":
		code, rest := firstArg(args)
		fs.Parse(rest)
		if code == "" && fs.NArg() > 0 {
			code = fs.Arg(0)
		}
		if code == "" {
			fail(fmt.Errorf("usage: %s enroll <code> [--url URL] [--machine-id ID] [--set KEY=VALUE]...", exeName()))
		}
		enroll(code, *url, *cfgPath, *machineID, settings, *reset)
	case "install":
		fs.Parse(args)
		cfg := config.Load(*cfgPath)
		must(cfg.FileErr)
		if !cfg.Enrolled() {
			fail(fmt.Errorf("not enrolled yet: run `%s enroll <code>` first", exeName()))
		}
		repairOwnership(cfg)
		saveSettings(cfg.File, settings, *reset)
		exe, err := os.Executable()
		if err != nil {
			fail(err)
		}
		if err := service.Install(exe, cfg.File); err != nil {
			fail(err)
		}
		fmt.Printf("Installed and started the %s service.\n", service.Name)
	case "uninstall":
		must(service.Uninstall())
		fmt.Printf("Removed the %s service. Its settings and spool stay in %s.\n", service.Name, config.DefaultDir())
	case "start":
		must(service.Start())
	case "stop":
		must(service.Stop())
	case "note-stop":
		// The systemd unit's ExecStop, not for people.
		fs.Parse(args)
		dir := config.Load(*cfgPath).StateDir
		if dir == "" {
			dir = config.DefaultDir()
		}
		reason, err := agent.RecordStopReason(dir)
		must(err)
		logf("stopping (%s)", reason)
	case "status":
		fs.Parse(args)
		status(config.Load(*cfgPath))
	case "version", "--version", "-version":
		fmt.Println("infinianalytics-agent", version, runtime.GOOS+"/"+runtime.GOARCH)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

// runAgent is the service body: load settings, run until ctx ends.
func runAgent(cfgPath string) service.RunFunc {
	return func(ctx context.Context, stopReason func() string) error {
		cfg := config.Load(cfgPath)
		if cfg.FileErr != nil {
			if errors.Is(cfg.FileErr, fs.ErrPermission) {
				return fmt.Errorf("%w; `sudo %s install` gives it back to the service", cfg.FileErr, exeName())
			}
			return cfg.FileErr
		}
		// The container image has no interactive step: a start with an
		// IA_AGENT_ENROLL_CODE it has not enrolled with yet enrolls, in place
		// - a reinstall keeps the state volume, and with it the spool. Later
		// starts find the code already used and skip it.
		if code := strings.TrimSpace(os.Getenv("IA_AGENT_ENROLL_CODE")); code != "" && !agent.EnrolledWith(cfg, code) {
			url := cfg.URL
			if url == "" {
				url = defaultURL
			}
			res, err := agent.Enroll(ctx, cfg, url, code, version)
			switch {
			case err == nil:
				logf("enrolled as server %s", res.ServerID)
				if res.DroppedSpool {
					logf("dropped the windows not pushed yet: they belong to server %s", cfg.ServerID)
				}
				cfg = config.Load(cfgPath)
			case !cfg.Enrolled():
				return err
			default:
				// Most likely the code it enrolled with before this
				// version recorded codes, long expired.
				logf("IA_AGENT_ENROLL_CODE not used (%v); staying enrolled as server %s", err, cfg.ServerID)
				if errors.Is(err, agent.ErrEnrollRefused) {
					_ = agent.RecordEnrollCode(cfg, code)
				}
			}
		}
		a, err := agent.New(cfg, version, logf)
		if err != nil {
			return err
		}
		a.StopReason = stopReason
		return a.Run(ctx)
	}
}

// moduleFlag is --disks / --docker. It takes a value, so both `--docker off`
// and `--docker=off` work, and only switches given on the command line are
// recorded.
type moduleFlag struct {
	key string
	set map[string]string
}

func (f moduleFlag) String() string { return "" }

func (f moduleFlag) Set(s string) error {
	on, ok := config.ParseSwitch(s)
	if !ok {
		return fmt.Errorf("want on or off, got %q", s)
	}
	f.set[f.key] = fmt.Sprint(on)
	return nil
}

// settingFlag is --set KEY=VALUE. Repeatable; each one is checked against
// config.Settings as it is parsed, so a typo fails before anything changes.
type settingFlag map[string]string

func (f settingFlag) String() string { return "" }

func (f settingFlag) Set(s string) error {
	key, value, err := config.ParseSetting(s)
	if err != nil {
		return err
	}
	f[key] = value
	return nil
}

// settingsToSave is what saveSettings writes: the given settings and, with
// reset, every other adjustable one emptied (removed, so its default applies).
func settingsToSave(settings map[string]string, reset bool) map[string]string {
	out := map[string]string{}
	if reset {
		for _, key := range config.SettingKeys() {
			out[key] = ""
		}
	}
	for key, value := range settings {
		out[key] = value
	}
	return out
}

// saveSettings writes the settings given on the command line into agent.env,
// where the service finds them.
func saveSettings(path string, settings map[string]string, reset bool) {
	values := settingsToSave(settings, reset)
	if len(values) == 0 {
		return
	}
	if err := config.SaveValues(path, values, config.SettingKeys()); err != nil {
		fail(fmt.Errorf("could not save the settings to %s: %w", path, err))
	}
}

// repairOwnership gives agent.env and the agent's state back to the owner of
// the directory holding them - the service user - where an earlier version,
// run as root, left them root's and unreadable to the service. Done before
// anything is written, as a rewrite keeps a file's owner.
func repairOwnership(cfg config.Config) {
	paths := []string{cfg.File}
	if cfg.StateDir != "" {
		paths = append(paths, agent.StateFiles(cfg.StateDir)...)
	}
	for _, p := range paths {
		fixed, err := statefile.Repair(p)
		if err != nil {
			fail(fmt.Errorf("could not give %s back to the service user: %w", p, err))
		}
		if len(fixed) > 0 {
			fmt.Printf("Gave %s back to the service user (it was unreadable to the service).\n", p)
		}
	}
}

func enroll(code, url, cfgPath, machineID string, settings map[string]string, reset bool) {
	cfg := config.Load(cfgPath)
	must(cfg.FileErr) // before the code is spent on an identity that could not be saved
	repairOwnership(cfg)
	if url == "" {
		url = cfg.URL
	}
	if url == "" {
		url = defaultURL
	}
	if machineID = strings.TrimSpace(machineID); machineID != "" {
		cfg.MachineID = machineID
	}
	res, err := agent.Enroll(context.Background(), cfg, url, code, version)
	if err != nil {
		fail(err)
	}
	if machineID != "" {
		// Kept, so a later re-enroll of this clone reports the same id.
		if err := config.SaveValues(cfg.File, map[string]string{config.KeyMachineID: machineID}, []string{config.KeyMachineID}); err != nil {
			fail(fmt.Errorf("could not save the machine id to %s: %w", cfg.File, err))
		}
	}
	saveSettings(cfg.File, settings, reset)
	fmt.Printf("Enrolled as server %s.\nSettings saved to %s.\n", res.ServerID, cfg.File)
	if res.DroppedSpool {
		fmt.Printf("Dropped the windows not pushed yet: they belong to server %s.\n", cfg.ServerID)
	}
	fmt.Printf("Next: `%s install` to run it as a service (or `%s run` to try it in the foreground).\n", exeName(), exeName())
}

func status(cfg config.Config) {
	fmt.Printf("config:     %s", cfg.File)
	if !cfg.FileLoaded {
		fmt.Print(" (not found)")
	}
	fmt.Println()
	if cfg.FileErr != nil {
		fmt.Printf("problem:    %v\n", cfg.FileErr)
	}
	if !cfg.Enrolled() {
		fmt.Println("enrolled:   no")
		return
	}
	fmt.Printf("server:     %s\nbackend:    %s\nmodules:    %s\n", cfg.ServerID, cfg.URL, agent.Modules(cfg))
	fmt.Printf("settings:   %s\n", describeSettings(cfg))
	dir := cfg.StateDir
	if dir == "" {
		dir = config.DefaultDir()
	}
	st := agent.LoadState(dir)
	if st.LastPushAt.IsZero() {
		fmt.Println("last push:  never")
	} else {
		fmt.Printf("last push:  %s ago (%s", time.Since(st.LastPushAt).Round(time.Second), st.LastPushStatus)
		if st.LastPushCode != 0 {
			fmt.Printf(", HTTP %d", st.LastPushCode)
		}
		fmt.Println(")")
		if st.LastError != "" {
			fmt.Printf("last error: %s\n", st.LastError)
		}
		if !st.LastSuccessAt.IsZero() {
			fmt.Printf("last ok:    %s ago\n", time.Since(st.LastSuccessAt).Round(time.Second))
		}
	}
	// Not opened when missing: OpenSpool would create it, and `status` usually
	// runs as root - a root-owned spool the service could not write to.
	spool := agent.SpoolDir(dir)
	if _, err := os.Stat(spool); err != nil {
		fmt.Println("spool:      empty")
	} else if sp, err := agent.OpenSpool(spool, cfg.SpoolMaxAge, cfg.SpoolMaxBytes); err == nil {
		records, size, _ := sp.Pending()
		sp.Close()
		fmt.Printf("spool:      %d window(s) pending, %.1f KB\n", records, float64(size)/1024)
	}
}

// describeSettings is the adjustable settings in effect, on one line.
func describeSettings(cfg config.Config) string {
	parts := []string{}
	if cfg.DisksEnabled {
		parts = append(parts, fmt.Sprintf("filesystems %s every %s", strings.Join(cfg.FilesystemRoots, ","), cfg.FilesystemInterval))
	}
	if cfg.DockerEnabled {
		limit := "all"
		if cfg.ContainerLimit > 0 {
			limit = fmt.Sprint(cfg.ContainerLimit)
		}
		parts = append(parts, fmt.Sprintf("containers %s via %s (%d in parallel)", limit, cfg.DockerHost, cfg.DockerConcurrency))
	}
	parts = append(parts, fmt.Sprintf("spool %s / %d MB", cfg.SpoolMaxAge, cfg.SpoolMaxBytes>>20))
	return strings.Join(parts, "; ")
}

func firstArg(args []string) (string, []string) {
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		return args[0], args[1:]
	}
	return "", args
}

func usage() {
	n := exeName()
	fmt.Fprintf(os.Stderr, `%s %s - InfiniAnalytics server agent

Usage:
  %s enroll <code> [--url URL] [--machine-id ID]
                                 link this server using a code from the dashboard
  %s install                     install and start the service (root / administrator);
                                 run it again to apply new settings
  %s run                         run in the foreground
  %s status                      last push and pending spool
  %s uninstall | start | stop    manage the service
  %s version

Every command takes --config PATH (default: %s).

Modules (host vitals are always sent; these are on by default):
  --disks on|off                 disk space per filesystem
  --docker on|off                Docker containers and events
The same switches are IA_AGENT_DISKS and IA_AGENT_DOCKER.

Settings:
  --set KEY=VALUE                repeatable; an empty VALUE means the default.
                                 KEY is one of:
                                   %s
  --reset                        enroll / install: every setting not given goes
                                 back to its default
On enroll and install, modules and settings are saved to the settings file so
the service keeps them; on run they apply to that run only.
`, n, version, n, n, n, n, n, n, filepath.Join(config.DefaultDir(), config.FileName), strings.Join(config.SettingKeys(), "\n                                   "))
}

func exeName() string { return filepath.Base(os.Args[0]) }

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "infinianalytics-agent: "+format+"\n", args...)
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	logf("%v", err)
	os.Exit(1)
}

// resourceDefaults keeps a bare binary as small as the systemd unit makes it:
// one scheduler thread is plenty for a sample every 2 s, and a soft memory
// limit makes the GC work harder rather than grow. GOMAXPROCS / GOMEMLIMIT
// override either.
func resourceDefaults() {
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(1)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(40 << 20)
	}
}

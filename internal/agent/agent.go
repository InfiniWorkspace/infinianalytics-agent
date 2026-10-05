package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/dockerstats"
	"github.com/rene-roid/kanshi/internal/roots"
	"github.com/rene-roid/kanshi/internal/vitals"
)

const (
	// Records per push: one hour of 10 s windows, the backend's cap.
	maxRecordsPerPush = 360
	// The backend's other caps on one push (MetricsBatch in
	// app/schemas/servers.py). A backlog of busy windows can reach them
	// well before 360 records.
	maxContainerRowsPerPush  = 20_000
	maxFilesystemRowsPerPush = 5_000
	maxEventsPerPush         = 1_000
	// While draining a backlog, pause this long between pushes.
	drainPause = time.Second
	// Host identity is re-sent at least this often even when unchanged.
	hostEvery = time.Hour
	// The single push attempted on the way out.
	finalPushTimeout = 3 * time.Second
	// While the key is revoked, look for a new one (a fresh `enroll`) this often.
	revokedRecheck = time.Minute
)

// Agent samples the host every SampleInterval, closes a window every
// WindowInterval into the spool, and pushes the spool to the backend.
type Agent struct {
	cfg     config.Config
	version string
	logf    func(string, ...any)

	reader     *vitals.Reader
	spool      *Spool
	containers *containerSampler
	state      State
	bootID     string

	mu         sync.Mutex
	events     []Event
	cores      []float64
	host       HostInfo
	hostSent   HostInfo
	hostSentAt time.Time
	mounts     map[string]bool
	lastFS     time.Time

	// StopReason, when set, says why the agent is stopping once Run's ctx is
	// done ("" = unknown, ask the OS). The Windows service sets it.
	StopReason func() string
}

// New prepares an agent; Run starts it.
func New(cfg config.Config, version string, logf func(string, ...any)) (*Agent, error) {
	if !cfg.Enrolled() {
		return nil, errors.New("not enrolled: run `infinianalytics-agent enroll <code>` first")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = config.DefaultDir()
	}
	a := &Agent{
		cfg:     cfg,
		version: version,
		logf:    logf,
		reader:  vitals.New(roots.NewResolver(cfg.FilesystemRoots, cfg.HostRoot)),
	}
	if cfg.DockerEnabled {
		a.containers = newContainerSampler(dockerstats.New(cfg), cfg.ContainerLimit)
	}
	return a, nil
}

func (a *Agent) emit(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A daemon spewing events while the backend is down must not grow this
	// without bound between two windows.
	if len(a.events) < 1000 {
		a.events = append(a.events, ev)
	}
}

func (a *Agent) drainEvents() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.events
	a.events = nil
	return out
}

// Run blocks until ctx is cancelled, then stops cleanly: the stop reason and
// the last partial window go to the spool first, then one short push.
func (a *Agent) Run(ctx context.Context) error {
	spool, err := OpenSpool(a.spoolDir(), a.cfg.SpoolMaxAge, a.cfg.SpoolMaxBytes)
	if err != nil {
		return err
	}
	a.spool = spool
	defer spool.Close()

	a.state = LoadState(a.cfg.StateDir)
	takeRecordedStopReason(a.cfg.StateDir) // a leftover is from a stop this run did not see
	a.reader.Prime()
	first := a.reader.Sample()
	a.host = HostIdentity(first.Memory.Total, a.cfg.HostRoot)
	a.bootID = bootID(a.state)
	a.announceStart(first)

	loops, stopLoops := context.WithCancel(context.Background())
	defer stopLoops()
	var wg sync.WaitGroup
	if a.cfg.DockerEnabled {
		if ev, err := newDockerEvents(a.cfg.DockerHost, a.emit, a.logf); err == nil {
			wg.Add(1)
			go func() { defer wg.Done(); ev.run(loops) }()
		}
	}
	wg.Add(1)
	go func() { defer wg.Done(); a.pushLoop(loops) }()

	a.logf("pushing to %s every %s as server %s (modules: %s)", a.cfg.URL, a.cfg.WindowInterval, a.cfg.ServerID, Modules(a.cfg))
	last := a.sampleLoop(ctx)

	// Stopping: say why, durably, before anything else can go wrong.
	stopLoops()
	wg.Wait()
	reason := a.reasonForStop()
	a.emit(Event{TS: time.Now().UTC(), Type: EventStopping, Data: map[string]any{"reason": reason, "boot_id": a.bootID}})
	if err := a.closeWindow(last, true); err != nil {
		a.logf("could not spool the final window: %v", err)
	}
	pushCtx, cancel := context.WithTimeout(context.Background(), finalPushTimeout)
	defer cancel()
	a.pushOnce(pushCtx, NewPusher(a.cfg.URL, a.cfg.AgentKey, a.version))
	a.logf("stopped (%s)", reason)
	a.saveState()
	return nil
}

// Modules lists what this configuration sends, e.g. "host, disks, docker".
func Modules(cfg config.Config) string {
	out := "host"
	if cfg.DisksEnabled {
		out += ", disks"
	}
	if cfg.DockerEnabled {
		out += ", docker"
	}
	return out
}

func (a *Agent) spoolDir() string { return SpoolDir(a.cfg.StateDir) }

// SpoolDir is where the spool lives in the state directory dir.
func SpoolDir(dir string) string { return filepath.Join(dir, "spool") }

// StateFiles is everything the agent keeps in the state directory dir besides
// agent.env, for `install` to give back to the service user (see
// statefile.Repair).
func StateFiles(dir string) []string {
	return []string{
		filepath.Join(dir, stateFile),
		filepath.Join(dir, stopReasonFile),
		filepath.Join(dir, machineIDFile),
		filepath.Join(dir, enrollCodeFile),
		SpoolDir(dir),
	}
}

// saveState persists a snapshot of the state taken under the lock.
func (a *Agent) saveState() {
	a.mu.Lock()
	snapshot := a.state
	a.mu.Unlock()
	if err := snapshot.Save(a.cfg.StateDir); err != nil {
		a.logf("could not save state: %v", err)
	}
}

func (a *Agent) reasonForStop() string {
	if a.StopReason != nil {
		if reason := a.StopReason(); reason != "" {
			return reason
		}
	}
	if reason := takeRecordedStopReason(a.cfg.StateDir); reason != "" {
		return reason
	}
	return detectStopReason()
}

// announceStart queues the boot and agent_update events a restart implies.
func (a *Agent) announceStart(first vitals.Sample) {
	now := time.Now().UTC()
	if a.state.BootID != "" && a.bootID != "" && a.state.BootID != a.bootID {
		ts := now
		if first.Uptime > 0 {
			ts = now.Add(-time.Duration(first.Uptime * float64(time.Second))).Round(time.Second)
		}
		a.emit(Event{TS: ts, Type: EventBoot, Data: map[string]any{
			"boot_id": a.bootID, "previous_boot_id": a.state.BootID,
		}})
	}
	if a.state.AgentVersion != "" && a.state.AgentVersion != a.version {
		a.emit(Event{TS: now, Type: EventAgentUpdate, Data: map[string]any{
			"from": a.state.AgentVersion, "to": a.version,
		}})
	}
	a.state.BootID = a.bootID
	if first.Uptime > 0 {
		a.state.BootTime = now.Add(-time.Duration(first.Uptime * float64(time.Second))).Round(time.Second)
	}
	a.state.AgentVersion = a.version
	a.saveState()
}

// window is the open window: host samples plus the containers read for it.
type window struct {
	host *hostWindow

	mu         sync.Mutex
	containers []ContainerRow
}

func (a *Agent) openWindow(ctx context.Context, start time.Time) *window {
	w := &window{host: newHostWindow(start, a.cfg.WindowInterval)}
	if a.containers != nil {
		// Read containers once per window, in the background: a busy daemon
		// must not delay the host samples.
		go func() {
			cctx, cancel := context.WithTimeout(ctx, a.cfg.WindowInterval)
			defer cancel()
			rows, ok := a.containers.sample(cctx, start)
			if ok {
				w.mu.Lock()
				w.containers = rows
				w.mu.Unlock()
			}
		}()
	}
	return w
}

// sampleLoop reads vitals on the SampleInterval grid and rolls windows over
// until ctx ends. It returns the window still open at that point.
func (a *Agent) sampleLoop(ctx context.Context) *window {
	step := a.cfg.SampleInterval
	cur := a.openWindow(ctx, windowStart(time.Now(), a.cfg.WindowInterval))
	for {
		now := time.Now()
		// Sleep to the next point of the sampling grid, so every window
		// gets the same number of samples.
		wait := now.Truncate(step).Add(step).Sub(now)
		select {
		case <-ctx.Done():
			return cur
		case <-time.After(wait):
		}
		t := time.Now()
		s := a.reader.Sample()
		a.mu.Lock()
		a.cores = append(a.cores[:0], s.CPU.Cores...)
		a.host.MemTotal = s.Memory.Total
		a.mu.Unlock()

		if ws := windowStart(t, a.cfg.WindowInterval); !ws.Equal(cur.host.start) {
			if err := a.closeWindow(cur, false); err != nil {
				a.logf("spool: %v", err)
			}
			cur = a.openWindow(ctx, ws)
		}
		cur.host.add(s)
	}
}

// closeWindow turns a window into a spool record.
func (a *Agent) closeWindow(w *window, final bool) error {
	rec := Record{}
	if hs, ok := w.host.sample(); ok {
		rec.Samples = []HostSample{hs}
	}
	w.mu.Lock()
	rec.Containers = w.containers
	w.mu.Unlock()
	if a.cfg.DisksEnabled && !final && time.Since(a.lastFS) >= a.cfg.FilesystemInterval {
		rec.Filesystems = a.readFilesystems(w.host.start)
		a.lastFS = time.Now()
	}
	rec.Events = a.drainEvents()
	if len(rec.Samples) == 0 && len(rec.Events) == 0 && len(rec.Containers) == 0 && len(rec.Filesystems) == 0 {
		return nil
	}
	a.mu.Lock()
	a.state.Seq++
	rec.Seq = a.state.Seq
	a.mu.Unlock()
	return a.spool.Append(rec)
}

// readFilesystems measures every root, and reports mounts that came or went
// since the last reading.
func (a *Agent) readFilesystems(ts time.Time) []FilesystemRow {
	list := a.reader.Filesystems()
	rows := make([]FilesystemRow, 0, len(list))
	now := map[string]bool{}
	for _, fs := range list {
		// An image filesystem (an ISO, a squashfs snap) is always "full"
		// by construction: reporting it would keep the server degraded and
		// hide the disks that can actually fill up.
		if readOnlyMount(fs.Path) {
			continue
		}
		now[fs.Label] = true
		rows = append(rows, FilesystemRow{TS: ts, Mount: fs.Label, Label: fs.Label, Total: fs.Total, Used: fs.Used})
	}
	if a.mounts != nil {
		for m := range now {
			if !a.mounts[m] {
				a.emit(Event{TS: ts, Type: EventFSMount, Data: map[string]any{"mount": m}})
			}
		}
		for m := range a.mounts {
			if !now[m] {
				a.emit(Event{TS: ts, Type: EventFSUnmount, Data: map[string]any{"mount": m}})
			}
		}
	}
	a.mounts = now
	return rows
}

// pushLoop drains the spool to the backend for as long as ctx lives.
func (a *Agent) pushLoop(ctx context.Context) {
	pusher := NewPusher(a.cfg.URL, a.cfg.AgentKey, a.version)
	b := newBackoff(a.cfg.WindowInterval, 5*time.Minute)
	limit := maxRecordsPerPush
	wait := a.cfg.WindowInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		recs, mark, err := a.peekPush(limit)
		if err != nil {
			a.logf("spool: %v", err)
			wait = a.cfg.WindowInterval
			continue
		}
		if len(recs) == 0 {
			wait = a.cfg.WindowInterval
			continue
		}
		batch := a.buildBatch(recs)
		resp, outcome, err := pusher.Push(ctx, batch)
		a.recordPush(outcome, err)
		switch outcome {
		case OutcomeOK:
			if err := a.spool.Ack(mark); err != nil {
				a.logf("spool: %v", err)
			}
			a.markHostSent(batch)
			b.reset()
			backlog := len(recs) == limit
			limit = maxRecordsPerPush
			wait = time.Duration(resp.NextPushS) * time.Second
			if wait <= 0 {
				wait = a.cfg.WindowInterval
			}
			if backlog {
				wait = drainPause // more is queued: keep going
			}
		case OutcomeRetry:
			wait = b.next()
			a.logf("push failed (%v); retrying in %s", err, wait.Round(time.Second))
		case OutcomeTooLarge, OutcomeInvalid:
			// Halve until the window the backend refuses is pushed alone,
			// then drop just that one.
			if len(recs) == 1 {
				a.logf("dropping one window the backend refuses: %v", err)
				_ = a.spool.Ack(mark)
				limit = maxRecordsPerPush
			} else {
				limit = max(1, len(recs)/2)
			}
			wait = drainPause
		case OutcomeDrop:
			a.logf("backend rejected %d window(s), dropping them: %v", len(recs), err)
			_ = a.spool.Ack(mark)
			wait = a.cfg.WindowInterval
		case OutcomeRevoked:
			a.logf("the backend no longer accepts this agent (%v): the server was deleted or its key "+
				"replaced. Pushing stops; enroll again with a new code to resume.", err)
			if !a.waitForNewKey(ctx, pusher) {
				return
			}
			wait = drainPause
		}
		a.saveState()
	}
}

// waitForNewKey idles until agent.env holds a different key (someone ran
// `enroll` again), then switches the pusher to it. False when ctx ends first.
func (a *Agent) waitForNewKey(ctx context.Context, p *Pusher) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(revokedRecheck):
		}
		fresh := config.Load(a.cfg.File)
		if fresh.Enrolled() && fresh.AgentKey != p.Key {
			a.logf("new agent key found in %s; resuming as server %s", a.cfg.File, fresh.ServerID)
			a.mu.Lock()
			a.cfg.URL, a.cfg.ServerID, a.cfg.AgentKey = fresh.URL, fresh.ServerID, fresh.AgentKey
			a.mu.Unlock()
			*p = *NewPusher(fresh.URL, fresh.AgentKey, a.version)
			return true
		}
	}
}

// pushOnce tries to deliver everything pending once, without retries. Used
// on the way out, so the stop reason reaches the backend before the machine
// goes down whenever the network allows it.
func (a *Agent) pushOnce(ctx context.Context, p *Pusher) {
	recs, mark, err := a.peekPush(maxRecordsPerPush)
	if err != nil || len(recs) == 0 {
		return
	}
	batch := a.buildBatch(recs)
	_, outcome, err := p.Push(ctx, batch)
	a.recordPush(outcome, err)
	if outcome == OutcomeOK {
		_ = a.spool.Ack(mark)
		a.markHostSent(batch)
	}
}

// peekPush is the next push: up to limit pending records, as many as fit.
func (a *Agent) peekPush(limit int) ([]Record, Mark, error) {
	recs, mark, err := a.spool.Peek(limit)
	if err != nil {
		return nil, mark, err
	}
	if n := fitPush(recs); n < len(recs) {
		return a.spool.Peek(n)
	}
	return recs, mark, nil
}

// fitPush is how many of recs, from the oldest, one push can carry within the
// backend's caps. At least one: a record alone is always tried.
func fitPush(recs []Record) int {
	var containers, filesystems, events int
	for i, r := range recs {
		containers += len(r.Containers)
		filesystems += len(r.Filesystems)
		events += len(r.Events)
		if i > 0 && (containers > maxContainerRowsPerPush ||
			filesystems > maxFilesystemRowsPerPush || events > maxEventsPerPush) {
			return i
		}
	}
	return len(recs)
}

// buildBatch merges spooled records into one request.
func (a *Agent) buildBatch(recs []Record) Batch {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := Batch{
		SchemaVersion: SchemaVersion,
		AgentVersion:  a.version,
		ServerID:      a.cfg.ServerID,
		BootID:        a.bootID,
		SentAt:        time.Now().UTC(),
		Cores:         append([]float64(nil), a.cores...),
		Samples:       []HostSample{},
	}
	for _, r := range recs {
		b.Seq = max(b.Seq, r.Seq)
		b.Samples = append(b.Samples, r.Samples...)
		b.Filesystems = append(b.Filesystems, r.Filesystems...)
		b.Containers = append(b.Containers, r.Containers...)
		b.Events = append(b.Events, r.Events...)
	}
	if a.host != a.hostSent || time.Since(a.hostSentAt) >= hostEvery {
		host := a.host
		b.Host = &host
		// Fixed for the life of the process, so it rides along with the
		// identity: on the first batch after a (re)start and hourly.
		conf := Effective(a.cfg)
		b.Config = &conf
	}
	return b
}

// markHostSent records that the identity in a delivered batch reached the
// backend; until then every batch keeps carrying it.
func (a *Agent) markHostSent(b Batch) {
	if b.Host == nil {
		return
	}
	a.mu.Lock()
	a.hostSent, a.hostSentAt = *b.Host, time.Now()
	a.mu.Unlock()
}

func (a *Agent) recordPush(outcome Outcome, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().UTC()
	a.state.LastPushAt = now
	a.state.LastPushStatus = outcome.String()
	a.state.LastError = ""
	a.state.LastPushCode = 0
	var pe *PushError
	if errors.As(err, &pe) {
		a.state.LastPushCode = pe.Status
	}
	if err != nil {
		a.state.LastError = err.Error()
	}
	if outcome == OutcomeOK {
		a.state.LastSuccessAt = now
		a.state.LastPushCode = 200
	}
}

// Effective is the configuration the agent runs with, as reported to the
// backend. Durations are whole seconds.
func Effective(cfg config.Config) AgentConfig {
	out := AgentConfig{
		Disks:             cfg.DisksEnabled,
		Docker:            cfg.DockerEnabled,
		FSRoots:           append([]string{}, cfg.FilesystemRoots...),
		FSIntervalS:       int(cfg.FilesystemInterval.Round(time.Second) / time.Second),
		ContainerLimit:    cfg.ContainerLimit,
		DockerConcurrency: cfg.DockerConcurrency,
		SpoolMaxAgeS:      int(cfg.SpoolMaxAge.Round(time.Second) / time.Second),
		SpoolMaxMB:        int(cfg.SpoolMaxBytes >> 20),
		Container:         cfg.HostRoot != "",
	}
	if !cfg.DockerHostIsDefault() {
		out.DockerHost = cfg.DockerHost
	}
	return out
}

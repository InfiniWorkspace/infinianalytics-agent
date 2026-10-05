package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/dockerstats"
	"github.com/rene-roid/kanshi/internal/roots"
	"github.com/rene-roid/kanshi/internal/vitals"
)

func TestContainerKeys(t *testing.T) {
	list := []dockerstats.Container{
		{Name: "api", FullName: "infini-api-1", Project: "infini"},
		{Name: "worker", FullName: "infini-worker-1", Project: "infini"},
		{Name: "worker", FullName: "infini-worker-2", Project: "infini"},
		{Name: "loose", FullName: "loose"},
	}
	got := containerKeys(list)
	want := []string{"infini/api", "infini/infini-worker-1", "infini/infini-worker-2", "loose"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPickKeepsBusiestRunningThenStopped(t *testing.T) {
	list := []dockerstats.Container{
		{FullName: "idle", State: "running", CPU: 1},
		{FullName: "dead", State: "exited"},
		{FullName: "hot", State: "running", CPU: 90},
		{FullName: "fat", State: "running", CPU: 5, MemPercent: 70},
	}
	keys := containerKeys(list)
	got, gotKeys := pick(list, keys, 3)
	if len(got) != 3 || gotKeys[0] != "hot" || gotKeys[1] != "fat" || gotKeys[2] != "idle" {
		t.Fatalf("picked %v", gotKeys)
	}
	_, all := pick(list, keys, 10)
	if all[3] != "dead" {
		t.Fatalf("stopped containers fill the room left: %v", all)
	}
}

func TestParseStopReason(t *testing.T) {
	cases := map[string]string{
		"":                                  StopService,
		"123 myapp.service stop running\n":  StopService,
		"1 reboot.target start waiting\n":   StopReboot,
		"7 kexec.target start waiting\n":    StopReboot,
		"2 poweroff.target start waiting\n": StopShutdown,
		"3 halt.target start waiting\n":     StopShutdown,
		"9 shutdown.target start waiting\n1 reboot.target start waiting": StopReboot,
		"4 reboot.target stop waiting\n":                                 StopService,
	}
	for in, want := range cases {
		if got := ParseStopReason(in); got != want {
			t.Errorf("ParseStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecordedStopReasonIsTakenOnce(t *testing.T) {
	dir := t.TempDir()
	if got := takeRecordedStopReason(dir); got != "" {
		t.Fatalf("nothing recorded, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, stopReasonFile), []byte(StopReboot+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := takeRecordedStopReason(dir); got != StopReboot {
		t.Fatalf("got %q, want %q", got, StopReboot)
	}
	if got := takeRecordedStopReason(dir); got != "" {
		t.Fatalf("a reason belongs to one stop, got %q again", got)
	}
}

func TestBackoffDoublesToTheCap(t *testing.T) {
	b := newBackoff(10*time.Second, time.Minute)
	b.jitter = func() float64 { return 0.5 } // no jitter
	var got []time.Duration
	for range 5 {
		got = append(got, b.next())
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	}
	b.reset()
	if d := b.next(); d != 10*time.Second {
		t.Fatalf("after reset = %v", d)
	}
}

func TestClassify(t *testing.T) {
	cases := map[int]Outcome{
		200: OutcomeOK, 0: OutcomeRetry, 500: OutcomeRetry, 503: OutcomeRetry, 429: OutcomeRetry,
		408: OutcomeRetry, 401: OutcomeRevoked, 410: OutcomeRevoked, 413: OutcomeTooLarge,
		400: OutcomeDrop, 422: OutcomeInvalid, 404: OutcomeDrop,
	}
	for status, want := range cases {
		if got := Classify(status); got != want {
			t.Errorf("Classify(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestFitPushStaysWithinTheBackendCaps(t *testing.T) {
	busy := func(containers, events int) Record {
		return Record{Containers: make([]ContainerRow, containers), Events: make([]Event, events)}
	}
	cases := []struct {
		name string
		recs []Record
		want int
	}{
		{"quiet hour", []Record{busy(50, 0), busy(50, 1), busy(50, 0)}, 3},
		{"containers", []Record{busy(8000, 0), busy(8000, 0), busy(8000, 0)}, 2},
		{"events", []Record{busy(0, 600), busy(0, 400), busy(0, 1)}, 2},
		{"one record over a cap still goes", []Record{busy(0, 1500), busy(0, 1)}, 1},
	}
	for _, c := range cases {
		if got := fitPush(c.recs); got != c.want {
			t.Errorf("%s: fitPush = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPushSendsGzipWithKeyAndParsesTheAnswer(t *testing.T) {
	var got Batch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/servers/metrics/" || r.Header.Get("X-Agent-Key") != "iak_k" ||
			r.Header.Get("Content-Encoding") != "gzip" {
			http.Error(w, "bad request shape", 400)
			return
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewDecoder(zr).Decode(&got)
		w.Write([]byte(`{"accepted_seq": 7, "next_push_s": 15}`))
	}))
	defer srv.Close()

	p := NewPusher(srv.URL+"/", "iak_k", "test")
	resp, outcome, err := p.Push(context.Background(), Batch{SchemaVersion: 1, Seq: 7, ServerID: "s", Samples: []HostSample{{N: 5}}})
	if err != nil || outcome != OutcomeOK {
		t.Fatalf("push = %v, %v", outcome, err)
	}
	if resp.NextPushS != 15 || got.Seq != 7 || len(got.Samples) != 1 {
		t.Fatalf("resp = %+v, got = %+v", resp, got)
	}
}

func TestPushSurfacesTheBackendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`{"detail": "This server was deleted from the dashboard.", "code": "server_deleted"}`))
	}))
	defer srv.Close()
	_, outcome, err := NewPusher(srv.URL, "k", "test").Push(context.Background(), Batch{})
	if outcome != OutcomeRevoked || err == nil || err.Error() != "HTTP 410: server_deleted: This server was deleted from the dashboard." {
		t.Fatalf("outcome = %v, err = %v", outcome, err)
	}
}

func TestDockerEventTranslation(t *testing.T) {
	d := &dockerEvents{images: map[string]string{"shop/web": "web:1"}}
	msg := func(action string, attrs map[string]string) dockerMessage {
		m := dockerMessage{Type: "container", Action: action, TimeNano: 1_700_000_000_000_000_000}
		m.Actor.Attributes = attrs
		return m
	}
	labels := func(extra map[string]string) map[string]string {
		out := map[string]string{"name": "shop-web-1", "com.docker.compose.project": "shop", "com.docker.compose.service": "web"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	ev, ok := d.translate(msg("die", labels(map[string]string{"exitCode": "137", "image": "web:1"})))
	if !ok || ev.Type != EventContainerDie || ev.Data["exit_code"] != 137 || ev.Data["key"] != "shop/web" {
		t.Fatalf("die = %+v", ev)
	}
	if ev, _ := d.translate(msg("health_status: unhealthy", labels(nil))); ev.Data["status"] != "unhealthy" {
		t.Fatalf("health = %+v", ev)
	}
	// Same image: a plain start. New image: a deploy marker.
	if ev, _ := d.translate(msg("start", labels(map[string]string{"image": "web:1"}))); ev.Type != EventContainerStart {
		t.Fatalf("start = %+v", ev)
	}
	ev, _ = d.translate(msg("start", labels(map[string]string{"image": "web:2"})))
	if ev.Type != EventContainerImage || ev.Data["from"] != "web:1" || ev.Data["to"] != "web:2" {
		t.Fatalf("deploy = %+v", ev)
	}
	if _, ok := d.translate(msg("exec_start", labels(nil))); ok {
		t.Fatal("exec events are not lifecycle events")
	}
}

func TestDisksModuleGatesFilesystemReadings(t *testing.T) {
	for _, disks := range []bool{false, true} {
		spool, err := OpenSpool(t.TempDir(), time.Hour, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		a := &Agent{
			cfg:    config.Config{DisksEnabled: disks, FilesystemInterval: time.Minute},
			reader: vitals.New(roots.NewResolver([]string{"auto"}, "")),
			spool:  spool,
		}
		w := &window{host: newHostWindow(time.Now(), 10*time.Second)}
		if err := a.closeWindow(w, false); err != nil {
			t.Fatal(err)
		}
		recs, _, err := spool.Peek(10)
		spool.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, r := range recs {
			got += len(r.Filesystems)
		}
		if disks && got == 0 {
			t.Error("disks on: the window should carry filesystem rows")
		}
		if !disks && got != 0 {
			t.Errorf("disks off: %d filesystem rows sent", got)
		}
	}
}

func TestModules(t *testing.T) {
	if got := Modules(config.Config{DisksEnabled: true, DockerEnabled: true}); got != "host, disks, docker" {
		t.Errorf("all on = %q", got)
	}
	if got := Modules(config.Config{}); got != "host" {
		t.Errorf("all off = %q", got)
	}
}

func TestBuildBatchMergesRecordsAndCarriesHostUntilDelivered(t *testing.T) {
	a := &Agent{version: "1.0", bootID: "b", host: HostInfo{Hostname: "h"}, cores: []float64{1, 2}}
	a.cfg.ServerID = "srv"
	recs := []Record{
		{Seq: 3, Samples: []HostSample{{N: 5}}, Events: []Event{{Type: EventBoot}}},
		{Seq: 4, Samples: []HostSample{{N: 5}}, Containers: []ContainerRow{{Key: "k"}}},
	}
	b := a.buildBatch(recs)
	if b.Seq != 4 || len(b.Samples) != 2 || len(b.Events) != 1 || len(b.Containers) != 1 || b.BootID != "b" {
		t.Fatalf("batch = %+v", b)
	}
	if b.Host == nil {
		t.Fatal("first batch carries the host identity")
	}
	// Not delivered yet: the next batch still carries it.
	if a.buildBatch(recs).Host == nil {
		t.Fatal("undelivered host identity must be sent again")
	}
	a.markHostSent(b)
	if a.buildBatch(recs).Host != nil {
		t.Fatal("delivered and unchanged: no host identity")
	}
	a.host.Hostname = "renamed"
	if a.buildBatch(recs).Host == nil {
		t.Fatal("a change is sent right away")
	}
}

func TestEffective(t *testing.T) {
	cfg := config.Load(filepath.Join(t.TempDir(), "missing.env"))
	got := Effective(cfg)
	if !got.Disks || !got.Docker || got.ContainerLimit != 50 || got.DockerConcurrency != 4 ||
		got.FSIntervalS != 60 || got.SpoolMaxAgeS != 172800 || got.SpoolMaxMB != 50 ||
		got.DockerHost != "" || got.Container || len(got.FSRoots) != 1 || got.FSRoots[0] != "auto" {
		t.Errorf("defaults = %+v", got)
	}
	t.Setenv("IA_AGENT_DOCKER_HOST", "tcp://10.0.0.5:2375")
	t.Setenv("IA_AGENT_FS_INTERVAL", "90")
	got = Effective(config.Load(filepath.Join(t.TempDir(), "missing.env")))
	if got.DockerHost != "tcp://10.0.0.5:2375" || got.FSIntervalS != 90 {
		t.Errorf("overrides = %+v", got)
	}
}

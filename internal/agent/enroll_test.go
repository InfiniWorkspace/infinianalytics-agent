package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rene-roid/kanshi/internal/config"
)

// fakeEnroll answers every enrollment with serverID, or refuses with 400.
func fakeEnroll(t *testing.T, serverID string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serverID == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"detail":"Invalid or expired enrollment code."}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"server_id":%q,"agent_key":"iak_new"}`, serverID)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// enrolledWithPending is a state directory enrolled as server-a, with
// windows the old key never delivered.
func enrolledWithPending(t *testing.T) config.Config {
	dir := t.TempDir()
	cfg := config.Config{File: filepath.Join(dir, "agent.env"), StateDir: dir, ServerID: "server-a", MachineID: "m1"}
	s, err := OpenSpool(SpoolDir(dir), time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(rec(1))
	s.Append(rec(2))
	s.Close()
	return cfg
}

func pending(t *testing.T, cfg config.Config) int {
	s, err := OpenSpool(SpoolDir(cfg.StateDir), time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, _, _ := s.Pending()
	return n
}

func TestReenrollingKeepsTheSpool(t *testing.T) {
	cfg := enrolledWithPending(t)
	res, err := Enroll(context.Background(), cfg, fakeEnroll(t, "server-a"), "AAAA-BBBB-CCCC", "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedSpool || pending(t, cfg) != 2 {
		t.Fatalf("dropped=%v pending=%d, want the 2 windows kept", res.DroppedSpool, pending(t, cfg))
	}
	if !EnrolledWith(cfg, "aaaa-bbbb-cccc ") || EnrolledWith(cfg, "AAAA-BBBB-DDDD") {
		t.Fatal("the code enrolled with is not recorded")
	}
}

func TestEnrollingAsAnotherServerDropsTheSpool(t *testing.T) {
	cfg := enrolledWithPending(t)
	res, err := Enroll(context.Background(), cfg, fakeEnroll(t, "server-b"), "AAAA-BBBB-CCCC", "test")
	if err != nil {
		t.Fatal(err)
	}
	if !res.DroppedSpool || pending(t, cfg) != 0 {
		t.Fatalf("dropped=%v pending=%d, want server-a's windows gone", res.DroppedSpool, pending(t, cfg))
	}
}

func TestARefusedCodeIsTold(t *testing.T) {
	cfg := enrolledWithPending(t)
	_, err := Enroll(context.Background(), cfg, fakeEnroll(t, ""), "AAAA-BBBB-CCCC", "test")
	if !errors.Is(err, ErrEnrollRefused) {
		t.Fatalf("err = %v, want ErrEnrollRefused", err)
	}
	if pending(t, cfg) != 2 || EnrolledWith(cfg, "AAAA-BBBB-CCCC") {
		t.Fatal("a refused enrollment changed the state")
	}
}

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/credentials"
	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/session"
	"github.com/raskrebs/sonar/internal/share"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/tunnel"
)

// The rule with the sharpest consequence in this whole feature: a bare
// `sonar share 3000` must be an error and not a default.
func TestABareShareIsAnErrorAndNamesBothReaches(t *testing.T) {
	withReachFlags(t, false, false)
	_, err := reachFrom()
	if err == nil {
		t.Fatal("a share with no reach was accepted")
	}
	for _, want := range []string{"--lan", "--public"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the message does not offer %s:\n%s", want, err)
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "defaulting") {
		t.Fatal("the message implies a default")
	}
}

func TestBothReachesAtOnceIsRefused(t *testing.T) {
	withReachFlags(t, true, true)
	if _, err := reachFrom(); err == nil {
		t.Fatal("--public --lan was accepted")
	}
}

func TestEachReachMapsToItsWireValue(t *testing.T) {
	withReachFlags(t, true, false)
	if got, err := reachFrom(); err != nil || got != "public" {
		t.Fatalf("--public = %q, %v", got, err)
	}
	withReachFlags(t, false, true)
	if got, err := reachFrom(); err != nil || got != "lan" {
		t.Fatalf("--lan = %q, %v", got, err)
	}
}

func TestTTLTakesTheThreeChoicesAndRefusesTheRest(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"while-it-runs": "while_it_runs",
		"1h":            "1h",
		"24h":           "24h",
		"1d":            "24h",
	}
	for in, want := range cases {
		got, err := ttlFrom(in)
		if err != nil || got != want {
			t.Fatalf("ttlFrom(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ttlFrom("3 weeks"); err == nil {
		t.Fatal("an arbitrary duration was accepted; there are three choices")
	}
}

// The limit is an offer, and a non-interactive caller is told how to accept it
// rather than being left with a refusal.
func TestTheLimitTellsANonInteractiveCallerAboutReplace(t *testing.T) {
	prevReplace := shareReplace
	shareReplace = false
	prevTTY := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() {
		shareReplace = prevReplace
		stdinIsTerminal = prevTTY
	})

	live := state.Share{
		URL:  "https://k7m2q9x4rt8vwz3b.sonarpreview.test",
		Repo: "acme", TargetService: ptrTo("web"),
	}
	err := rpc.ShareLimitError("already sharing web (acme)", "", live)

	stderr := captureStderr(t, func() {
		agreed, out := offerToMove(err)
		if agreed {
			t.Error("a non-interactive caller was taken to have agreed")
		}
		if !errors.Is(out, errSilent) {
			t.Errorf("err = %v, want errSilent — the message is already printed", out)
		}
	})
	if !strings.Contains(stderr, "web") || !strings.Contains(stderr, live.URL) {
		t.Fatalf("the offer does not name the live share:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--replace") {
		t.Fatalf("the offer does not say how to accept it:\n%s", stderr)
	}
}

func TestDescribeShareNamesTheServiceAndTheURL(t *testing.T) {
	got := describeShare(state.Share{
		Repo: "acme", TargetService: ptrTo("api"), URL: "https://x.test"})
	for _, want := range []string{"api", "acme", "https://x.test"} {
		if !strings.Contains(got, want) {
			t.Fatalf("describeShare = %q, missing %q", got, want)
		}
	}
}

func TestExpiryLineSaysWhenItEnds(t *testing.T) {
	at := time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)
	got := expiryLine(state.Share{ExpiresAt: &at})
	if !strings.Contains(got, "1h") {
		t.Fatalf("expiryLine = %q", got)
	}
	if got := expiryLine(state.Share{}); !strings.Contains(got, "service stops") {
		t.Fatalf("with no expiry, expiryLine = %q", got)
	}
}

// End to end through a real daemon: a machine with no session asks to share,
// gets 1110, is taken through the device flow in this terminal, and ends up
// with the URL it asked for. This is the headless-box path, and it is the only
// way in — there is no `sonar login`.
func TestNotSignedInSignsInHereAndThenShares(t *testing.T) {
	relay := newShareRelay()
	srv := httptest.NewServer(relay.handler())
	t.Cleanup(srv.Close)

	// A real dev server on a real port: the daemon asks a port what it is
	// before it shares it, and a port with nothing behind it is refused.
	appPort := servingHTTP(t)
	c := shareDaemonFor(t, []ports.ListeningPort{{Port: appPort, PID: 4242, Process: "node", Cwd: "/src/demo"}})

	// After the daemon, never before: its own OnStart hooks build a session and
	// a share manager pointed at the configured relay, and they would replace
	// these.
	store := credentials.New(filepath.Join(t.TempDir(), "credentials.json"), nil)
	session.SetManager(session.New(session.Options{Relay: srv.URL, Store: store}))
	t.Cleanup(func() { session.SetManager(nil) })

	shareMgr := share.New(share.Options{
		Dial: func(ctx context.Context, cfg tunnel.Config) error {
			if cfg.OnStatus != nil {
				cfg.OnStatus(tunnel.Status{State: tunnel.StateConnected})
			}
			<-ctx.Done()
			return nil
		},
		ConnectTimeout: 2 * time.Second,
		InstallID:      "install-test",
	})
	share.SetManager(shareMgr)
	t.Cleanup(func() { shareMgr.StopAll(); share.SetManager(nil) })

	port := appPort
	params := rpc.ShareCreateParams{Target: rpc.Selector{Port: &port}, Reach: "public"}

	var res rpc.ShareCreateResult
	var runErr error
	stderr := captureStderr(t, func() {
		res, runErr = shareCreate(context.Background(), c, params)
	})
	if runErr != nil {
		t.Fatalf("share: %v", runErr)
	}
	if !strings.Contains(stderr, relay.userCode()) {
		t.Fatalf("the code was never shown to the person:\n%s", stderr)
	}
	if !strings.Contains(stderr, "/device") {
		t.Fatalf("the URL to open was never shown:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Signed in as") {
		t.Fatalf("the sign-in was never confirmed:\n%s", stderr)
	}
	if res.Share.URL == "" || !strings.Contains(res.Share.URL, "sonarpreview.test") {
		t.Fatalf("share url = %q", res.Share.URL)
	}
	if res.Share.Status != "ready" {
		t.Fatalf("status = %q", res.Share.Status)
	}
}

// --lan is refused by the daemon with a plain sentence, and needs no account:
// it never reaches the relay at all.
func TestLANSaysItIsNotBuiltWithoutAskingForAnAccount(t *testing.T) {
	c := shareDaemonFor(t, []ports.ListeningPort{{Port: 3000, PID: 1, Process: "node", Cwd: "/src/demo"}})

	session.SetManager(session.New(session.Options{
		Relay: "http://127.0.0.1:1",
		Store: credentials.New(filepath.Join(t.TempDir(), "credentials.json"), nil),
	}))
	t.Cleanup(func() { session.SetManager(nil) })
	mgr := share.New(share.Options{InstallID: "x"})
	share.SetManager(mgr)
	t.Cleanup(func() { share.SetManager(nil) })
	port := 3000
	_, err := shareCreate(context.Background(), c,
		rpc.ShareCreateParams{Target: rpc.Selector{Port: &port}, Reach: "lan"})
	if err == nil {
		t.Fatal("--lan was accepted")
	}
	if !strings.Contains(err.Error(), "not built") {
		t.Fatalf("err = %v", err)
	}
	var e *rpc.Error
	if errors.As(err, &e) && e.Code == rpc.CodeNotSignedIn {
		t.Fatal("--lan asked for an account; it is a socket on this machine")
	}
}

// ------------------------------------------------------------- helpers ---

func ptrTo[T any](v T) *T { return &v }

func withReachFlags(t *testing.T, public, lan bool) {
	t.Helper()
	prevPublic, prevLAN := sharePublic, shareLAN
	sharePublic, shareLAN = public, lan
	t.Cleanup(func() { sharePublic, shareLAN = prevPublic, prevLAN })
}

// servingHTTP is an application on a real loopback port, for the tests that go
// through the daemon's own check that a share's target speaks HTTP.
func servingHTTP(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>app</title>"))
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().(*net.TCPAddr)
	return addr.Port
}

// shareDaemonFor starts a daemon over a fixed port table and hands back a
// connected client. The share and session managers are already installed by the
// caller, so the daemon's own start hooks do not get to replace them.
func shareDaemonFor(t *testing.T, rows []ports.ListeningPort) *client.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")

	srv := daemon.New(daemon.Options{
		Socket:  socket,
		Version: "test",
		Scanner: scanner.New(scanner.Options{
			DaemonVersion: "test",
			Scan: func(scanner.Include) ([]ports.ListeningPort, error) {
				return append([]ports.ListeningPort{}, rows...), nil
			},
		}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-srv.Done()
	})
	if err := client.WaitForSocket(ctx, socket, 5*time.Second); err != nil {
		t.Fatalf("daemon did not come up: %v", err)
	}
	c, err := client.Dial(context.Background(),
		client.ClientInfo{Name: "cli", Version: "test", Socket: socket})
	if err != nil {
		t.Fatalf("dialling the daemon: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// shareRelay is a relay that approves the device flow on the first poll and
// then serves the share control plane.
type shareRelay struct {
	mu   sync.Mutex
	code string
	n    int
}

func newShareRelay() *shareRelay { return &shareRelay{code: "WXYZ-1234"} }

func (s *shareRelay) userCode() string { return s.code }

func (s *shareRelay) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{
			"device_code":      "dev-1",
			"user_code":        s.code,
			"verification_uri": "https://relay.test/device",
			"expires_in":       900,
			"interval":         1,
		})
	})
	mux.HandleFunc("POST /v1/device/token", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{
			"access_token": "token-1",
			"account":      map[string]any{"id": "acct-1", "email": "owner@example.test"},
		})
	})
	mux.HandleFunc("POST /v1/shares", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.n++
		n := s.n
		s.mu.Unlock()
		slug := "aaaaaaaaaaaaaaa" + string(rune('a'+n))
		writeTestJSON(w, http.StatusOK, map[string]any{
			"id": "share-1", "slug": slug,
			"url":    "https://" + slug + ".sonarpreview.test",
			"status": "connecting", "ttl": "while_it_runs",
		})
	})
	mux.HandleFunc("DELETE /v1/shares/{slug}", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, map[string]any{"slug": r.PathValue("slug"), "status": "stopped"})
	})
	return mux
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

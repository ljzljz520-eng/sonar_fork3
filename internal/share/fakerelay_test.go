package share

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/raskrebs/sonar/internal/tunnel"
)

// fakeRelay is the share control plane, near enough. It implements the rules
// this package has to get right — a slug per key, the same slug on a re-publish,
// one live share per account with the live one returned on the refusal, a stop
// that keeps the reservation, and an extend — and nothing else.
//
// It is the relay's behaviour rather than its code: the real one is in another
// repository, is deployed, and is the source of truth. Where this and that
// disagree, the real-relay run at the end of the branch is what catches it.
type fakeRelay struct {
	mu sync.Mutex
	// bySlug and byKey are the reservation: a key always gets its slug back.
	bySlug map[string]*fakeShare
	byKey  map[string]*fakeShare
	next   int

	// limit is accounts.max_active_shares.
	limit int
	// signedIn is false to make every call answer 401, which is how the
	// session layer learns a token is dead.
	signedIn bool

	// calls records what was asked, for the assertions that care about how
	// many round trips something took.
	calls []string
}

type fakeShare struct {
	view shareView
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{
		bySlug:   map[string]*fakeShare{},
		byKey:    map[string]*fakeShare{},
		limit:    1,
		signedIn: true,
	}
}

func keyOf(req publishRequest) string {
	if strings.TrimSpace(req.Repo) != "" {
		return "committed:" + req.Repo + "\x00" + req.Worktree + "\x00" + req.ServiceName
	}
	return "fallback:" + req.InstallID + "\x00" + req.ProjectRoot + "\x00" + itoa(req.Port)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (f *fakeRelay) record(what string) {
	f.calls = append(f.calls, what)
	_ = f.calls
}

func (f *fakeRelay) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/shares", f.create)
	mux.HandleFunc("GET /v1/shares", f.list)
	mux.HandleFunc("DELETE /v1/shares/{slug}", f.stop)
	mux.HandleFunc("POST /v1/shares/{slug}/extend", f.extend)
	return mux
}

func (f *fakeRelay) authed(w http.ResponseWriter, r *http.Request) bool {
	if !f.signedIn || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthorized", "reason": "sign in first"})
		return false
	}
	return true
}

func (f *fakeRelay) create(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create")
	if !f.authed(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req publishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json"})
		return
	}
	if req.TTL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_ttl"})
		return
	}

	key := keyOf(req)
	existing, reserved := f.byKey[key]

	// The limit counts live shares only. A reservation is not one.
	var live *fakeShare
	liveCount := 0
	for _, sh := range f.bySlug {
		if !isLive(sh.view.Status) || (reserved && sh == existing) {
			continue
		}
		liveCount++
		if live == nil {
			live = sh
		}
	}
	if liveCount >= f.limit {
		if !req.Replace {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "share_limit_reached",
				"reason": "this account is already sharing as much as its plan allows",
				"limit":  f.limit,
				"live":   live.view,
			})
			return
		}
		live.view.Status = "stopped"
	}

	if !reserved {
		f.next++
		slug := "slug" + itoa(f.next) + "aaaaaaaaaaaa"
		existing = &fakeShare{view: shareView{
			ID:          "share-" + itoa(f.next),
			Slug:        slug,
			URL:         "https://" + slug + ".sonarpreview.test",
			Repo:        req.Repo,
			Worktree:    req.Worktree,
			ServiceName: req.ServiceName,
			InstallID:   req.InstallID,
			ProjectRoot: req.ProjectRoot,
			Port:        req.Port,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}}
		f.byKey[key] = existing
		f.bySlug[slug] = existing
	}
	existing.view.Status = "connecting"
	existing.view.TTL = req.TTL
	existing.view.ExpiresAt = time.Now().Add(ttlWindow(req.TTL)).UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, existing.view)
}

func ttlWindow(ttl string) time.Duration {
	switch ttl {
	case TTLOneHour:
		return time.Hour
	case TTLOneDay:
		return 24 * time.Hour
	}
	return 8 * time.Hour
}

func (f *fakeRelay) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authed(w, r) {
		return
	}
	out := []shareView{}
	for _, sh := range f.bySlug {
		out = append(out, sh.view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": out})
}

func (f *fakeRelay) stop(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop")
	if !f.authed(w, r) {
		return
	}
	sh, ok := f.bySlug[r.PathValue("slug")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no_such_share"})
		return
	}
	// The reservation survives: the row stays, only the status changes.
	sh.view.Status = "stopped"
	writeJSON(w, http.StatusOK, sh.view)
}

func (f *fakeRelay) extend(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authed(w, r) {
		return
	}
	sh, ok := f.bySlug[r.PathValue("slug")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no_such_share"})
		return
	}
	if !isLive(sh.view.Status) {
		writeJSON(w, http.StatusGone, map[string]any{"error": "share_expired"})
		return
	}
	var body struct {
		TTL string `json:"ttl"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	sh.view.TTL = body.TTL
	sh.view.ExpiresAt = time.Now().Add(ttlWindow(body.TTL)).UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, sh.view)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ------------------------------------------------------------- the seam ---

// httpCaller is the session as this package sees it, pointed at a fake relay.
type httpCaller struct {
	base      string
	token     string
	notSigned bool
}

func (h *httpCaller) Relay() string { return h.base }

func (h *httpCaller) Token() (string, error) {
	if h.notSigned {
		return "", rpc.NewError(rpc.CodeNotSignedIn, "not signed in to the relay", "")
	}
	return h.token, nil
}

func (h *httpCaller) Call(ctx context.Context, method, path string, payload any) (sessionResponse, error) {
	if h.notSigned {
		return sessionResponse{}, rpc.NewError(rpc.CodeNotSignedIn, "not signed in to the relay", "")
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return sessionResponse{}, err
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return sessionResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return sessionResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// The one session rule this stands in for: a 401 is not reported, it is
	// "not signed in".
	if resp.StatusCode == http.StatusUnauthorized {
		h.notSigned = true
		return sessionResponse{}, rpc.NewError(rpc.CodeNotSignedIn,
			"the relay no longer knows this session", "")
	}
	return sessionResponse{Status: resp.StatusCode, Body: raw}, nil
}

// ------------------------------------------------------- the environment ---

// fakeTunnel stands in for the transport. Every share's dial blocks until its
// context is cancelled, which is what a healthy tunnel does; a test that wants
// the service to vanish returns tunnel.ErrServiceGone instead.
type fakeTunnel struct {
	mu   sync.Mutex
	seen []tunnel.Config
	// end, when set, is what a dial returns instead of blocking.
	end error
	// connected controls whether the dial reports StateConnected. False leaves
	// the share `connecting`, which is what a slow relay looks like.
	connected bool
}

func (f *fakeTunnel) run(ctx context.Context, cfg tunnel.Config) error {
	f.mu.Lock()
	f.seen = append(f.seen, cfg)
	end, connected := f.end, f.connected
	f.mu.Unlock()

	if connected && cfg.OnStatus != nil {
		cfg.OnStatus(tunnel.Status{State: tunnel.StateConnected, URL: "https://x.test"})
	}
	if end != nil {
		return end
	}
	<-ctx.Done()
	return nil
}

func (f *fakeTunnel) configs() []tunnel.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tunnel.Config(nil), f.seen...)
}

type env struct {
	relay  *fakeRelay
	call   *httpCaller
	dialer *fakeTunnel
	m      *Manager
}

func newEnv(t *testing.T) *env {
	t.Helper()
	relay := newFakeRelay()
	srv := httptest.NewServer(relay.handler())
	t.Cleanup(srv.Close)

	caller := &httpCaller{base: srv.URL, token: "session-token"}
	dialer := &fakeTunnel{connected: true}
	m := New(Options{
		Session:        caller,
		Dial:           dialer.run,
		ConnectTimeout: 2 * time.Second,
		InstallID:      "install-aaaa",
	})
	m.probeFn = func(context.Context, string) (probeResult, error) {
		return probeResult{verdict: speaksHTTP, contentType: "text/html"}, nil
	}
	t.Cleanup(m.StopAll)
	return &env{relay: relay, call: caller, dialer: dialer, m: m}
}

// snapWith builds a port table. A group with a repo, a worktree and a service
// is the committed key; a bare port is the fallback.
func snapWith(ports ...state.Port) state.Snapshot {
	for i := range ports {
		if ports[i].Host == "" {
			ports[i].Host = state.LocalhostName
		}
	}
	return state.Snapshot{Ports: ports}
}

func committedSnapshot(port int, repo, worktree, service string) state.Snapshot {
	group := repo
	if worktree != "" {
		group = repo + "@" + worktree
	}
	root := "/src/" + repo
	p := state.Port{
		Host: state.LocalhostName, Port: port, PID: 4242,
		Group: &group, ProjectRoot: &root,
	}
	snap := snapWith(p)
	snap.Groups = []state.Group{{
		Host: state.LocalhostName, Name: group, Repo: repo, Worktree: worktree,
		RootDir:  &root,
		Services: []state.Service{{Name: service, PortActual: &port}},
	}}
	return snap
}

func fallbackSnapshot(port int, root string) state.Snapshot {
	p := state.Port{Host: state.LocalhostName, Port: port, PID: 77, Cwd: root}
	return snapWith(p)
}

func ptr[T any](v T) *T { return &v }

func createParams(port int, reach string) rpc.ShareCreateParams {
	return rpc.ShareCreateParams{Target: rpc.Selector{Port: &port}, Reach: reach}
}

package share

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/tunnel"
)

func codeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		t.Fatal("wanted an error, got none")
	}
	var e *rpc.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not an rpc.Error: %v", err)
	}
	return e.Code
}

func TestPublishGivesAURLAndDialsTheSlug(t *testing.T) {
	e := newEnv(t)
	snap := committedSnapshot(3000, "acme", "", "web")

	share, _, err := e.m.Create(context.Background(), snap, createParams(3000, ReachPublic))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if share.URL == "" || !strings.Contains(share.URL, ".sonarpreview.test") {
		t.Fatalf("url = %q", share.URL)
	}
	if share.Status != "ready" {
		t.Fatalf("status = %q, want ready", share.Status)
	}
	if share.Reach != ReachPublic {
		t.Fatalf("reach = %q", share.Reach)
	}
	if share.TargetPort != 3000 {
		t.Fatalf("target port = %d", share.TargetPort)
	}

	cfgs := e.dialer.configs()
	if len(cfgs) != 1 {
		t.Fatalf("dialled %d tunnels, want 1", len(cfgs))
	}
	// The tunnel must name the slug — that is what the relay routes on — and
	// carry the session, not a tunnel key.
	if cfgs[0].Share == "" || !strings.Contains(share.URL, cfgs[0].Share) {
		t.Fatalf("the tunnel named share %q for url %q", cfgs[0].Share, share.URL)
	}
	if cfgs[0].Key != "session-token" {
		t.Fatalf("the tunnel authenticated with %q", cfgs[0].Key)
	}
	if cfgs[0].LocalPort != 3000 {
		t.Fatalf("the tunnel points at port %d", cfgs[0].LocalPort)
	}
}

// The owner's requirement: publish the same thing twice, get the same URL.
func TestRepublishingTheSameServiceReturnsTheSameSlug(t *testing.T) {
	e := newEnv(t)
	snap := committedSnapshot(3000, "acme", "", "web")
	ctx := context.Background()

	first, _, err := e.m.Create(ctx, snap, createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	// Stop it, so the second publish is a reclaim rather than the idempotent
	// "this daemon is already holding it" path.
	if _, err := e.m.Stop(ctx, rpc.ShareStopParams{All: true}); err != nil {
		t.Fatal(err)
	}
	second, _, err := e.m.Create(ctx, snap, createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	if first.URL != second.URL {
		t.Fatalf("re-publishing changed the URL: %q then %q", first.URL, second.URL)
	}
}

// The port moving must not change the URL: the key is the service, not 3000.
func TestTheSameServiceOnAnotherPortKeepsItsURL(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	first, _, err := e.m.Create(ctx, committedSnapshot(3000, "acme", "", "web"), createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Stop(ctx, rpc.ShareStopParams{All: true}); err != nil {
		t.Fatal(err)
	}
	second, _, err := e.m.Create(ctx, committedSnapshot(3001, "acme", "", "web"), createParams(3001, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	if first.URL != second.URL {
		t.Fatalf("the dev server moving to 3001 changed the URL: %q then %q", first.URL, second.URL)
	}
}

// Two worktrees of one repository are two previews and get two URLs.
func TestTwoWorktreesGetTwoURLs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.relay.limit = 99

	main, _, err := e.m.Create(ctx, committedSnapshot(3000, "acme", "", "web"), createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	wt, _, err := e.m.Create(ctx, committedSnapshot(3001, "acme", "feature", "web"), createParams(3001, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	if main.URL == wt.URL {
		t.Fatalf("two checkouts shared one URL: %q", main.URL)
	}
}

func TestTheLimitIsAnOfferWithTheLiveShareAttached(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	first, _, err := e.m.Create(ctx, committedSnapshot(3000, "acme", "", "web"), createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = e.m.Create(ctx, committedSnapshot(4000, "acme", "", "api"), createParams(4000, ReachPublic))
	if got := codeOf(t, err); got != rpc.CodeShareLimitReached {
		t.Fatalf("code = %d, want %d", got, rpc.CodeShareLimitReached)
	}
	var e2 *rpc.Error
	_ = errors.As(err, &e2)
	if e2.Data.Share == nil {
		t.Fatal("the refusal did not name the live share, so no client can make the offer")
	}
	if e2.Data.Share.URL != first.URL {
		t.Fatalf("the refusal named %q, want the live share %q", e2.Data.Share.URL, first.URL)
	}
}

func TestReplaceMovesTheShare(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	first, _, err := e.m.Create(ctx, committedSnapshot(3000, "acme", "", "web"), createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	params := createParams(4000, ReachPublic)
	params.Replace = true
	second, _, err := e.m.Create(ctx, committedSnapshot(4000, "acme", "", "api"), params)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if second.URL == first.URL {
		t.Fatal("replacing gave the new service the old service's URL")
	}
	if second.Status != "ready" {
		t.Fatalf("status = %q", second.Status)
	}
}

func TestStopEndsTheShareAndKeepsTheReservation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	share, _, err := e.m.Create(ctx, committedSnapshot(3000, "acme", "", "web"), createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := e.m.Stop(ctx, rpc.ShareStopParams{ID: ptr(share.ID)})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != share.ID {
		t.Fatalf("stopped = %v, want [%s]", stopped, share.ID)
	}
	if got := e.m.List(); len(got) != 0 {
		t.Fatalf("%d shares still listed after the stop", len(got))
	}

	e.relay.mu.Lock()
	row := e.relay.bySlug[strings.TrimPrefix(share.URL, "https://")[:len("slug1aaaaaaaaaaaa")]]
	e.relay.mu.Unlock()
	if row == nil {
		t.Fatal("the reservation is gone from the relay; the slug can never be re-used")
	}
	if row.view.Status != "stopped" {
		t.Fatalf("the relay's row is %q, want stopped", row.view.Status)
	}
}

func TestExtendKeepsTheURLAndMovesTheExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	hour := TTLOneHour
	params := createParams(3000, ReachPublic)
	params.TTL = &hour
	share, _, err := e.m.Create(ctx, committedSnapshot(3000, "acme", "", "web"), params)
	if err != nil {
		t.Fatal(err)
	}
	if share.ExpiresAt == nil {
		t.Fatal("a 1h share has no expiry")
	}
	before, _ := time.Parse(time.RFC3339, *share.ExpiresAt)

	extended, err := e.m.Extend(ctx, rpc.ShareExtendParams{ID: share.ID, TTL: TTLOneDay})
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if extended.URL != share.URL {
		t.Fatalf("extending changed the URL: %q then %q", share.URL, extended.URL)
	}
	after, _ := time.Parse(time.RFC3339, *extended.ExpiresAt)
	if !after.After(before) {
		t.Fatalf("expiry did not move: %s then %s", before, after)
	}
}

func TestNotSignedInIs1110AndNothingIsPublished(t *testing.T) {
	e := newEnv(t)
	e.call.notSigned = true

	_, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"),
		createParams(3000, ReachPublic))
	if got := codeOf(t, err); got != rpc.CodeNotSignedIn {
		t.Fatalf("code = %d, want %d (not_signed_in)", got, rpc.CodeNotSignedIn)
	}
	if len(e.dialer.configs()) != 0 {
		t.Fatal("a tunnel was dialled for a machine that is not signed in")
	}
}

// A session the relay has forgotten is the same answer, so the CLI starts the
// same inline sign-in.
func TestARevokedSessionIsAlso1110(t *testing.T) {
	e := newEnv(t)
	e.relay.signedIn = false

	_, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"),
		createParams(3000, ReachPublic))
	if got := codeOf(t, err); got != rpc.CodeNotSignedIn {
		t.Fatalf("code = %d, want %d", got, rpc.CodeNotSignedIn)
	}
}

func TestABareReachIsRefused(t *testing.T) {
	e := newEnv(t)
	_, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"),
		createParams(3000, ""))
	if got := codeOf(t, err); got != rpc.CodeInvalidParams {
		t.Fatalf("code = %d, want invalid_params", got)
	}
	if len(e.dialer.configs()) != 0 {
		t.Fatal("something was shared without a reach")
	}
}

func TestLANSaysItIsNotBuilt(t *testing.T) {
	e := newEnv(t)
	_, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"),
		createParams(3000, ReachLAN))
	if got := codeOf(t, err); got != rpc.CodeUnsupported {
		t.Fatalf("code = %d, want unsupported", got)
	}
	if !strings.Contains(err.Error(), "not built") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestNothingListeningIs1100(t *testing.T) {
	e := newEnv(t)
	_, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"),
		createParams(9999, ReachPublic))
	if got := codeOf(t, err); got != rpc.CodeTargetNotListening {
		t.Fatalf("code = %d, want %d", got, rpc.CodeTargetNotListening)
	}
}

func TestABadTTLNamesTheThreeChoices(t *testing.T) {
	e := newEnv(t)
	params := createParams(3000, ReachPublic)
	params.TTL = ptr("a fortnight")
	_, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"), params)
	if got := codeOf(t, err); got != rpc.CodeInvalidParams {
		t.Fatalf("code = %d", got)
	}
	for _, want := range []string{TTLWhileItRuns, TTLOneHour, TTLOneDay} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the message does not name %q: %s", want, err.Error())
		}
	}
}

// A project with no sonar.yaml still shares, on the fallback key.
func TestAProjectWithNoConfigUsesTheFallbackKey(t *testing.T) {
	e := newEnv(t)
	share, _, err := e.m.Create(context.Background(), fallbackSnapshot(8080, "/tmp/scratch"),
		createParams(8080, ReachPublic))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if share.Repo != "" {
		t.Fatalf("repo = %q, want empty — this is the fallback key", share.Repo)
	}
	e.relay.mu.Lock()
	defer e.relay.mu.Unlock()
	if _, ok := e.relay.byKey["fallback:install-aaaa\x00/tmp/scratch\x008080"]; !ok {
		t.Fatalf("the relay was not sent the fallback key; it has %v", keys(e.relay.byKey))
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The share ends when the service stays gone, and it does not resume.
func TestAServiceThatStaysGoneEndsTheShare(t *testing.T) {
	e := newEnv(t)
	e.dialer.end = tunnel.ErrServiceGone

	share, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"),
		createParams(3000, ReachPublic))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The dial returned before Create did, so by the time we look the manager
	// has already reaped it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.m.List()) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.m.List(); len(got) != 0 {
		t.Fatalf("the share is still listed as %q after its service stopped", got[0].Status)
	}

	e.relay.mu.Lock()
	defer e.relay.mu.Unlock()
	for _, sh := range e.relay.bySlug {
		if isLive(sh.view.Status) {
			t.Fatalf("the relay still thinks %s is live", sh.view.Slug)
		}
	}
	_ = share
}

// Asking twice while this daemon is already holding the share is one relay call
// and one tunnel, not two.
func TestAskingTwiceWhileLiveIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	snap := committedSnapshot(3000, "acme", "", "web")

	first, _, err := e.m.Create(ctx, snap, createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := e.m.Create(ctx, snap, createParams(3000, ReachPublic))
	if err != nil {
		t.Fatalf("the second ask failed: %v", err)
	}
	if first.URL != second.URL {
		t.Fatalf("%q then %q", first.URL, second.URL)
	}
	if n := len(e.dialer.configs()); n != 1 {
		t.Fatalf("%d tunnels for one share", n)
	}
}

func TestListAndLogs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	share, _, err := e.m.Create(ctx, committedSnapshot(3000, "acme", "", "web"), createParams(3000, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.m.List(); len(got) != 1 || got[0].ID != share.ID {
		t.Fatalf("list = %v", got)
	}

	lines, err := e.m.Logs(rpc.ShareLogsParams{ID: share.ID})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "connected") {
		t.Fatalf("logs = %v, want the connection recorded", lines)
	}

	if _, err := e.m.Logs(rpc.ShareLogsParams{ID: "nope"}); codeOf(t, err) != rpc.CodeNotFound {
		t.Fatal("an unknown id should be not_found")
	}
}

// The tunnel taking a moment is not an error: the slug is reserved and the URL
// is the one that will work.
func TestASlowConnectionStillAnswersWithTheURL(t *testing.T) {
	e := newEnv(t)
	e.dialer.connected = false
	e.m.connectTimeout = 100 * time.Millisecond

	share, _, err := e.m.Create(context.Background(), committedSnapshot(3000, "acme", "", "web"),
		createParams(3000, ReachPublic))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if share.URL == "" {
		t.Fatal("no URL was printed for a share that is still connecting")
	}
	if share.Status != "connecting" {
		t.Fatalf("status = %q, want connecting", share.Status)
	}
}

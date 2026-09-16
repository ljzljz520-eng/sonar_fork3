package share

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// The probe against real listeners rather than stubs: the whole point of it is
// what a socket does, and a fake socket would only prove that the fake agrees
// with the code.

// serveRaw runs a listener that hands each connection to f, and returns its
// address.
func serveRaw(t *testing.T, f func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				f(c)
			}()
		}
	}()
	return ln.Addr().String()
}

func probeAddr(t *testing.T, addr string) probeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := probe(ctx, addr)
	if err != nil {
		t.Fatalf("probing %s: %v", addr, err)
	}
	return res
}

func TestTheProbeRecognisesAWebServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}))
	t.Cleanup(srv.Close)

	got := probeAddr(t, srv.Listener.Addr().String())
	if got.verdict != speaksHTTP {
		t.Fatalf("a dev server was read as %v", got.verdict)
	}
	if got.contentType != "text/html" {
		t.Errorf("content type is %q, want text/html without its parameters", got.contentType)
	}
	if got.looksLikeAPI() {
		t.Error("a page was taken for an API")
	}
}

// The line in decision 6: an HTTP port that answers with data rather than a
// page, inside a project that has something else serving the pages.
func TestTheProbeNoticesAnAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
	}))
	t.Cleanup(srv.Close)

	got := probeAddr(t, srv.Listener.Addr().String())
	if got.verdict != speaksHTTP {
		t.Fatalf("an API was read as %v", got.verdict)
	}
	if !got.looksLikeAPI() {
		t.Error("an API answering JSON was not noticed")
	}
}

// A dev server on https://. The forwarder dials the local port in plain HTTP,
// so this cannot be shared — and being told it "does not speak HTTP" next to a
// Postgres image is a confusing thing to read when you are running Vite.
//
// The two shapes a TLS port answers a plaintext request in, because they are
// genuinely different and only one of them is a TLS record.
func TestTheProbeTellsHTTPSApartFromNotHTTP(t *testing.T) {
	// Go's net/http, and nginx: a 400 that says so in words.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	if got := probeAddr(t, srv.Listener.Addr().String()); got.verdict != speaksTLS {
		t.Fatalf("a Go HTTPS server was read as %v, want speaksTLS", got.verdict)
	}

	// OpenSSL, and therefore Node and Vite: a TLS alert record.
	alert := serveRaw(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 512))
		_, _ = c.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x46})
	})
	if got := probeAddr(t, alert); got.verdict != speaksTLS {
		t.Fatalf("a TLS alert was read as %v, want speaksTLS", got.verdict)
	}

	// Node, and therefore a Vite dev server on https://: it closes a plaintext
	// request without a word, exactly as Postgres and Redis do. Measured
	// against a real one — the bytes cannot tell them apart, so the second
	// connection has to.
	likeNode := serveTLSOrClose(t)
	if got := probeAddr(t, likeNode); got.verdict != speaksTLS {
		t.Fatalf("a Node HTTPS server was read as %v, want speaksTLS", got.verdict)
	}

	// And a plain 400 that is nothing to do with TLS is still an HTTP server.
	plain := serveRaw(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 512))
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Type: text/html\r\n\r\n"))
	})
	if got := probeAddr(t, plain); got.verdict != speaksHTTP {
		t.Fatalf("an ordinary 400 was read as %v, want speaksHTTP", got.verdict)
	}
}

// SSH, SMTP, and a few databases announce themselves before they are asked
// anything. No HTTP server does.
func TestTheProbeRecognisesSomethingThatSpeaksFirst(t *testing.T) {
	addr := serveRaw(t, func(c net.Conn) {
		_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
		time.Sleep(time.Second)
	})
	if got := probeAddr(t, addr); got.verdict != spokeFirst {
		t.Fatalf("a banner was read as %v, want spokeFirst", got.verdict)
	}
}

// Postgres and Redis read the request, fail to parse it, and answer with a
// frame of their own or hang up.
func TestTheProbeRecognisesAnErrorFrame(t *testing.T) {
	addr := serveRaw(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 512))
		// Postgres answers an unparseable startup packet with an 'E' message.
		_, _ = c.Write([]byte{'E', 0, 0, 0, 22, 'S', 'F', 'A', 'T', 'A', 'L', 0, 0})
	})
	if got := probeAddr(t, addr); got.verdict != spokeFirst {
		t.Fatalf("an error frame was read as %v", got.verdict)
	}

	closed := serveRaw(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 512))
	})
	if got := probeAddr(t, closed); got.verdict != refusedIt {
		t.Fatalf("a port that hung up was read as %v, want refusedIt", got.verdict)
	}
}

// Held open and silent: something waiting to be spoken to in another language.
func TestTheProbeGivesUpOnASilentPort(t *testing.T) {
	addr := serveRaw(t, func(net.Conn) { time.Sleep(10 * time.Second) })
	start := time.Now()
	got := probeAddr(t, addr)
	if got.verdict != saidNothing {
		t.Fatalf("a silent port was read as %v, want saidNothing", got.verdict)
	}
	// It has to give up, and it has to do it while somebody is still watching
	// the terminal.
	// probeBanner waiting for a banner, probeAnswer waiting for an answer, and
	// probeDial for the TLS handshake that settles what a silent port is.
	if elapsed := time.Since(start); elapsed > probeBanner+probeAnswer+probeDial+2*time.Second {
		t.Errorf("the probe took %s to give up", elapsed)
	}
}

func TestNothingListeningIsADialError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if _, err := probe(context.Background(), addr); err == nil {
		t.Fatal("probing a closed port succeeded")
	}
}

// The refusal has to name what was found and point at the thing that does
// work, because it is the only chance to save the person a support question.
func TestTheRefusalSaysWhatToDoInstead(t *testing.T) {
	err := notHTTP(target{Port: 7132, Image: "postgres:17"}, refusedIt)
	var re *rpc.Error
	if !asRPC(err, &re) {
		t.Fatalf("the refusal is not an rpc error: %T", err)
	}
	if re.Code != rpc.CodeTargetNotHTTP {
		t.Errorf("code is %d, want %d", re.Code, rpc.CodeTargetNotHTTP)
	}
	if !strings.Contains(re.Data.Detail, "postgres:17") {
		t.Errorf("the refusal does not name the image: %q", re.Data.Detail)
	}
	if !strings.Contains(re.Data.Detail, "localhost:7132") {
		t.Errorf("the refusal does not name the port: %q", re.Data.Detail)
	}
	if !strings.Contains(re.Data.Hint, "ssh -L 7132:localhost:7132") {
		t.Errorf("the refusal does not point at SSH: %q", re.Data.Hint)
	}

	// A native process has no image, and the sentence still has to read.
	bare := notHTTP(target{Port: 5432}, saidNothing)
	if !asRPC(bare, &re) {
		t.Fatal("not an rpc error")
	}
	if strings.Contains(re.Data.Detail, "()") {
		t.Errorf("an empty image left brackets behind: %q", re.Data.Detail)
	}

	tlsErr := notHTTP(target{Port: 5173}, speaksTLS)
	if !asRPC(tlsErr, &re) {
		t.Fatal("not an rpc error")
	}
	if !strings.Contains(re.Data.Detail, "HTTPS") {
		t.Errorf("the HTTPS refusal does not say HTTPS: %q", re.Data.Detail)
	}
	if strings.Contains(re.Data.Hint, "ssh") {
		t.Errorf("a dev server on https:// was pointed at SSH: %q", re.Data.Hint)
	}
}

// A share is refused before anything is reserved, so the slug space is not
// spent on a share that was never going to work.
func TestADatabaseIsRefusedWithoutReservingASlug(t *testing.T) {
	e := newEnv(t)
	e.m.probeFn = func(context.Context, string) (probeResult, error) {
		return probeResult{verdict: spokeFirst}, nil
	}
	snap := committedSnapshot(7132, "acme", "", "db")

	_, _, err := e.m.Create(context.Background(), snap, createParams(7132, ReachPublic))
	if err == nil {
		t.Fatal("a port that does not speak HTTP was shared")
	}
	var re *rpc.Error
	if !asRPC(err, &re) || re.Code != rpc.CodeTargetNotHTTP {
		t.Fatalf("the refusal was %v, want target_not_http", err)
	}
	e.relay.mu.Lock()
	calls := append([]string(nil), e.relay.calls...)
	e.relay.mu.Unlock()
	for _, c := range calls {
		if c == "create" {
			t.Errorf("the relay was asked to reserve a slug: %v", calls)
			break
		}
	}
	if len(e.m.List()) != 0 {
		t.Errorf("a refused share is in the list: %v", e.m.List())
	}
}

// The API line is a nudge on the happy path, and only where there is a sibling
// that probably serves the pages.
func TestTheAPILineIsOnlyForAServiceInAProject(t *testing.T) {
	e := newEnv(t)
	e.m.probeFn = func(context.Context, string) (probeResult, error) {
		return probeResult{verdict: speaksHTTP, contentType: "application/json"}, nil
	}

	_, notes, err := e.m.Create(context.Background(),
		committedSnapshot(9700, "acme", "", "api"), createParams(9700, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "API") {
		t.Fatalf("no line about the API: %v", notes)
	}
	if _, err := e.m.Stop(context.Background(), rpc.ShareStopParams{All: true}); err != nil {
		t.Fatal(err)
	}

	// The same service with no project around it is just as likely to be the
	// whole application, so it gets nothing.
	_, notes, err = e.m.Create(context.Background(),
		snapWith(state.Port{Port: 9700, PID: 1, Cwd: "/src/lone"}), createParams(9700, ReachPublic))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("a lone service was told it looks like an API: %v", notes)
	}
}

// serveTLSOrClose is Node's observed behaviour: a plaintext request is closed
// on without a word, and a TLS handshake is completed.
func serveTLSOrClose(t *testing.T) string {
	t.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}
	return serveRaw(t, func(c net.Conn) {
		first := make([]byte, 1)
		if _, err := c.Read(first); err != nil {
			return
		}
		if first[0] != 0x16 {
			// Not a TLS record. Hang up, saying nothing at all.
			return
		}
		tc := tls.Server(&replayConn{Conn: c, first: first}, cfg)
		_ = tc.Handshake()
		_ = tc.Close()
	})
}

// replayConn gives back the byte that was read to decide what this is.
type replayConn struct {
	net.Conn
	first []byte
}

func (r *replayConn) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	return r.Conn.Read(p)
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// asRPC is errors.As without importing errors into every assertion.
func asRPC(err error, out **rpc.Error) bool {
	for err != nil {
		if e, ok := err.(*rpc.Error); ok {
			*out = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

package share

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
)

// Does this port speak HTTP?
//
// A share is an HTTP proxy. Pointed at a database it produces a URL that will
// never work, and the person finds out from a blank page rather than from us —
// after the slug has been spent and the link has been pasted somewhere. So the
// question is asked once, at share time, and a "no" is a refusal with a
// sentence rather than a share.
//
// Two signals. The container image is a hint: `postgres:17` on the port row is
// a good guess and a useless verdict, because the same image can run anything
// and a native process has no image at all. So the verdict is an active probe:
// dial the port and find out. The daemon already dials this address for every
// request a share forwards, so nothing new reaches the machine.

// verdict is what the probe concluded about a port.
type verdict int

const (
	// speaksHTTP: the port answered with a status line. Whatever is behind it,
	// a share will work.
	speaksHTTP verdict = iota
	// speaksTLS: the port wants a TLS handshake. A dev server on https://,
	// almost always — which a share cannot reach, because the forwarder dials
	// the local port in plain HTTP. Worth its own message: "not HTTP" next to
	// `postgres:17` is a confusing thing to tell someone running Vite.
	speaksTLS
	// spokeFirst: something sent a banner before it was asked anything. SSH,
	// SMTP, and a few databases. No HTTP server does this.
	spokeFirst
	// refusedIt: the port read the request and closed, or answered with
	// something that is not a status line. Postgres and Redis land here.
	refusedIt
	// saidNothing: connected, and then silence until the deadline. A port held
	// open by something that is waiting to be spoken to in another language.
	saidNothing
)

func (v verdict) ok() bool { return v == speaksHTTP }

// probeResult is the verdict plus what is worth saying about it.
type probeResult struct {
	verdict verdict
	// contentType is the response's, when there was one. It answers a
	// different question — whether this looks like a page or an API — which is
	// only asked once the port has already proved it speaks HTTP.
	contentType string
}

const (
	// probeDial bounds the connection. Local, so anything slow is wrong.
	probeDial = 2 * time.Second
	// probeBanner is how long a port gets to speak first. Everything that
	// opens with a banner does it immediately; this is not a wait for a slow
	// server, it is a wait for a fast one to prove it has nothing to say.
	probeBanner = 250 * time.Millisecond
	// probeAnswer is how long a port gets to answer the request. Generous,
	// because a dev server's first request can compile the application.
	probeAnswer = 2 * time.Second
)

// probe asks the port what it is. Errors are dialling errors only: a port that
// answers strangely is a verdict, not a failure.
func probe(ctx context.Context, addr string) (probeResult, error) {
	d := net.Dialer{Timeout: probeDial}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return probeResult{}, err
	}
	defer func() { _ = conn.Close() }()

	// First: does it speak unprompted? Read before writing, because after the
	// request has gone out there is no way to tell a banner from an answer.
	_ = conn.SetReadDeadline(time.Now().Add(probeBanner))
	buf := make([]byte, 1024)
	switch n, err := conn.Read(buf); {
	case n > 0:
		return probeResult{verdict: classify(buf[:n])}, nil
	case err == nil:
		// Zero bytes and no error: nothing to read yet, which is the normal
		// case and the one every HTTP server takes.
	case isTimeout(err):
		// Same, said differently.
	default:
		// Closed on us without a word.
		return settle(ctx, addr, probeResult{verdict: refusedIt}), nil
	}

	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = "localhost"
	}
	req := fmt.Sprintf("HEAD / HTTP/1.1\r\nHost: %s\r\nUser-Agent: sonar-probe\r\nConnection: close\r\n\r\n", host)
	_ = conn.SetWriteDeadline(time.Now().Add(probeAnswer))
	if _, err := conn.Write([]byte(req)); err != nil {
		return settle(ctx, addr, probeResult{verdict: refusedIt}), nil
	}

	_ = conn.SetReadDeadline(time.Now().Add(probeAnswer))
	n, err := conn.Read(buf)
	if n == 0 {
		if err != nil && isTimeout(err) {
			return settle(ctx, addr, probeResult{verdict: saidNothing}), nil
		}
		return settle(ctx, addr, probeResult{verdict: refusedIt}), nil
	}
	got := buf[:n]
	out := probeResult{verdict: classify(got)}
	if out.verdict == speaksHTTP {
		out.contentType = contentTypeOf(got)
	}
	return out, nil
}

// settle asks the one question the first connection could not answer.
//
// A port that hangs up without a word is ambiguous in the worst way: Postgres
// does it, Redis does it, and so does Node — which means so does a Vite dev
// server on https://. Measured, not assumed: all three close on a plaintext
// HEAD with nothing at all, so the bytes cannot tell them apart. A second
// connection offering a TLS handshake can, and it only ever runs on the path
// that was about to refuse the share anyway.
func settle(ctx context.Context, addr string, in probeResult) probeResult {
	if in.verdict != refusedIt && in.verdict != saidNothing {
		return in
	}
	if completesTLSHandshake(ctx, addr) {
		return probeResult{verdict: speaksTLS}
	}
	return in
}

// completesTLSHandshake reports whether the port is a TLS server.
//
// The certificate is not checked, and must not be: this is a dev server on
// localhost, its certificate is self-signed or from mkcert, and the question
// being asked is "what protocol is this" rather than "should I trust it".
// Nothing is sent over the connection and it is closed immediately.
func completesTLSHandshake(ctx context.Context, addr string) bool {
	// Its own deadline, not the caller's: the handshake is bounded by the
	// context rather than by NetDialer.Timeout, and a port that accepts and
	// then says nothing would otherwise hold the whole command open.
	ctx, cancel := context.WithTimeout(ctx, probeDial)
	defer cancel()
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: probeDial},
		Config:    &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see above
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// classify reads the first thing a port said.
func classify(b []byte) verdict {
	if len(b) == 0 {
		return saidNothing
	}
	// A TLS record: 0x16 is a handshake, 0x15 an alert — which is what a TLS
	// server sends when a plaintext request arrives, and therefore the one we
	// actually see here.
	if b[0] == 0x15 || b[0] == 0x16 {
		return speaksTLS
	}
	if s := string(b); strings.HasPrefix(s, "HTTP/") {
		if plaintextToTLSPort(s) {
			return speaksTLS
		}
		return speaksHTTP
	}
	return spokeFirst
}

// plaintextToTLSPort spots the answer a TLS server gives when a plain HTTP
// request lands on it in words rather than in a TLS alert.
//
// Go's net/http and nginx both do this, and both say so unmistakably; an
// OpenSSL-backed server — which is Node, and therefore Vite — sends an alert
// record instead and is caught by the byte above. Both halves are needed
// because a dev server on https:// is the one case here that a real person
// hits, and telling them their Vite "does not speak HTTP" would be worse than
// saying nothing.
//
// A 400 and one of two fixed phrases: the cost of a false positive is refusing
// a share that would have worked, so this is deliberately narrow.
func plaintextToTLSPort(s string) bool {
	if !strings.HasPrefix(s, "HTTP/1.0 400") && !strings.HasPrefix(s, "HTTP/1.1 400") {
		return false
	}
	lower := strings.ToLower(s)
	return strings.Contains(lower, "https server") || strings.Contains(lower, "https port")
}

// contentTypeOf pulls the header out of a response head, lowercased and
// without its parameters. Empty when there is not one.
func contentTypeOf(b []byte) string {
	for _, line := range strings.Split(string(b), "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "content-type") {
			continue
		}
		value, _, _ = strings.Cut(value, ";")
		return strings.ToLower(strings.TrimSpace(value))
	}
	return ""
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// looksLikeAPI reports whether an HTTP port answered with something other than
// a page. It is a nudge, never a refusal: plenty of good reasons exist for a
// root path that is not HTML, and the only cost of being wrong is one line.
func (r probeResult) looksLikeAPI() bool {
	if !r.verdict.ok() || r.contentType == "" {
		return false
	}
	return !strings.Contains(r.contentType, "html")
}

// checkTarget is the probe as the share path uses it: a refusal, a note, or
// nothing to say. It runs before anything is reserved on the relay, so a
// refusal does not spend a slug on a share that was never going to work.
func (m *Manager) checkTarget(ctx context.Context, t target) ([]string, error) {
	addr := net.JoinHostPort("localhost", strconv.Itoa(t.Port))
	res, err := m.probeFn(ctx, addr)
	if err != nil {
		// It was listening when the snapshot was taken and is not now. The
		// tunnel would discover the same thing a second later and say it worse.
		return nil, rpc.Errorf(rpc.CodeTargetNotListening,
			"nothing is listening on localhost:%d any more", t.Port)
	}
	if !res.verdict.ok() {
		return nil, notHTTP(t, res.verdict)
	}
	if res.looksLikeAPI() && t.Group != "" {
		// Only inside a group, where there is a sibling that probably serves
		// the pages and this one probably serves the data. A lone service that
		// answers with JSON is just as likely to be the whole application.
		return []string{
			"This looks like an API rather than a page. Anyone with the link " +
				"can call it, so keep the link close and stop the share when you are done.",
		}, nil
	}
	return nil, nil
}

// notHTTP is the refusal. It names what was found, says what a share is for,
// and points at the thing that does work — which for a database is SSH, and
// for a dev server on https:// is turning TLS off.
func notHTTP(t target, v verdict) error {
	if v == speaksTLS {
		return rpc.NewError(rpc.CodeTargetNotHTTP,
			fmt.Sprintf("localhost:%d speaks HTTPS, and a share forwards plain HTTP to the local port", t.Port),
			"Run the dev server without TLS — a share is HTTPS from the outside either way.")
	}
	what := fmt.Sprintf("localhost:%d does not speak HTTP", t.Port)
	if t.Image != "" {
		what += " (" + t.Image + ")"
	}
	return rpc.NewError(rpc.CodeTargetNotHTTP,
		what+". A share carries web traffic only",
		fmt.Sprintf("To reach it from another machine, forward the port over SSH:\n"+
			"  ssh -L %d:localhost:%d <this machine>", t.Port, t.Port))
}

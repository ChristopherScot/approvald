package main

// Approval requests, their one-time tokens, and the pages a person taps.
//
// This is the service's own logic, kept apart from server.go, which
// adapts it to the generated api.Handler interface. Nothing here knows
// about HTTP routing or about ogen.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/ChristopherScot/approvald/api"
	"time"
)

const (
	// Bounds how long a leaked tap URL stays useful.
	requestTTL = 10 * time.Minute

	// Retained past the decision so replays are rejected, not re-published.
	retainAfterDecision = 30 * time.Minute

	maxNonceLen = 128

	// Bounded and stripped before it reaches an ntfy header.
	maxDetailLen = 256
)

type decision string

const (
	decisionNone    decision = ""
	decisionApprove decision = "approve"
	decisionDeny    decision = "deny"
)

type request struct {
	nonce     string
	token     string
	createdAt time.Time
	decided   decision
	decidedAt time.Time
	// publishing marks an in-flight publish attempt so concurrent taps do
	// not all try at once; published marks one that succeeded.
	publishing bool
	published  bool
	detail     string
}

// decidable reports whether a tap may still set a decision.
func (r *request) decidable(now time.Time) bool {
	return r.decided == decisionNone && now.Sub(r.createdAt) <= requestTTL
}

// collectable reports whether the record can be dropped. Decided requests
// outlive undecided ones so that replays are recognized rather than looking
// like a nonce that never existed.
func (r *request) collectable(now time.Time) bool {
	if r.decided != decisionNone {
		return now.Sub(r.decidedAt) > retainAfterDecision
	}
	return now.Sub(r.createdAt) > requestTTL
}

type store struct {
	mu sync.Mutex
	m  map[string]*request
}

func newStore() *store { return &store{m: make(map[string]*request)} }

// register records a new pending request, reporting false if the nonce is
// already in flight. Re-issuing would mint a fresh tap URL for someone
// else's pending request.
func (s *store) register(r *request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.m[r.nonce]; exists {
		return false
	}
	s.m[r.nonce] = r
	return true
}

// decide verifies the capability token and ensures the request has a
// decision, reporting the one now in force. Token checking lives here so
// that the whole rule is applied under one lock: a caller that read the
// request first would race another tap writing it.
//
// The server is authoritative. The Mac also treats the first reply as
// final, but the two halves agreeing should not depend on the client
// behaving.
func (s *store) decide(nonce, token string, d decision) (result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[nonce]
	if !ok {
		return result{}, errUnknown
	}
	if subtle.ConstantTimeCompare([]byte(r.token), []byte(token)) != 1 {
		// Someone has a valid nonce but the wrong token - worth seeing.
		slog.Warn("tap with bad token", "nonce", nonce)
		return result{}, errBadToken
	}
	now := time.Now()
	if r.decided != decisionNone {
		// Re-tap of a decided request. It owes a publish only if the
		// previous attempt is finished and failed - claiming the attempt
		// here stops concurrent taps from all publishing at once.
		if !r.published && !r.publishing {
			r.publishing = true
			return result{decision: r.decided, publish: true}, nil
		}
		return result{decision: r.decided}, nil
	}
	if !r.decidable(now) {
		return result{}, errExpired
	}
	r.decided = d
	r.decidedAt = now
	r.publishing = true
	return result{decision: d, publish: true}, nil
}

// markPublished records that the decision reached the response topic, so
// later taps stop retrying.
func (s *store) markPublished(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[nonce]; ok {
		r.published = true
		r.publishing = false
	}
}

// releasePublish returns a failed attempt to the pool so a later tap can
// retry it.
func (s *store) releasePublish(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[nonce]; ok {
		r.publishing = false
	}
}

// peek reports a request's state without deciding it, for rendering the
// confirmation page. Verifies the token so an invalid link is rejected
// before the user taps anything.
func (s *store) peek(nonce, token string) (request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[nonce]
	if !ok {
		return request{}, errUnknown
	}
	if subtle.ConstantTimeCompare([]byte(r.token), []byte(token)) != 1 {
		return request{}, errUnknown
	}
	if r.decided == decisionNone && !r.decidable(time.Now()) {
		return request{}, errExpired
	}
	return *r, nil
}

// detailFor returns the registered detail string for a decided request.
func (s *store) detailFor(nonce string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[nonce]; ok {
		return r.detail
	}
	return ""
}

// reapLoop drops collectable records until ctx is cancelled.
func (s *store) reapLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reap()
		}
	}
}

func (s *store) reap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, r := range s.m {
		if r.collectable(now) {
			delete(s.m, k)
		}
	}
}

// userError pairs a failure with how it should be shown. Rendering lives
// next to the definition so every path reports a given outcome identically.
type userError struct {
	code  int
	title string
	msg   string
}

func (e *userError) Error() string { return e.title }

var (
	// Unknown nonce and bad token render identically so a public caller
	// cannot use the response to enumerate which nonces exist.
	errUnknown  = &userError{http.StatusNotFound, "Not found", "No pending request, or that link is not valid for it."}
	errExpired  = &userError{http.StatusGone, "Expired", "This request expired. Re-run the command to ask again."}
	errBadToken = &userError{http.StatusNotFound, "Not found", "No pending request, or that link is not valid for it."}
)

// result is the outcome of a tap: the decision now in force, and whether
// this tap set it. A replay is a normal outcome, not an error - only the
// tap that set the decision publishes.
type result struct {
	decision decision
	// publish is true when this tap owes a publish - either it set the
	// decision, or an earlier tap set it but the publish failed. A decided
	// request whose publish never landed must stay retriable, or the user
	// taps again and is told "already decided" while the Mac never hears.
	publish bool
}

type server struct {
	store         *store
	baseURL       string
	ntfyURL       string
	ntfyToken     string
	respTopic     string
	registerToken string
	client        *http.Client
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// register is called by the Mac before it publishes a request, so the
// service only ever approves nonces it issued a token for. Reaching the
// endpoint is not by itself enough to manufacture an approval.
// Sentinels for the two ways registering can be refused, so server.go
// can map them to the statuses the spec documents without matching on
// error strings.
var (
	errBadNonce  = errors.New("invalid nonce")
	errDuplicate = errors.New("nonce already registered")
)

// register mints the tap URLs for a nonce.
//
// It takes the values rather than a request: ogen has already parsed the
// form body and checked the bearer token by the time this is called, so
// the parsing, the size limit and the auth check that used to live here
// are the generated code's job now.
func (s *server) register(nonce, detail string) (*api.Registration, error) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" || len(nonce) > maxNonceLen || !isSafeNonce(nonce) {
		return nil, errBadNonce
	}
	tok, err := randHex(32)
	if err != nil {
		return nil, fmt.Errorf("minting token: %w", err)
	}

	if len(detail) > maxDetailLen {
		detail = detail[:maxDetailLen]
	}
	// Control characters would corrupt the notification and the page.
	detail = strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, detail)

	if !s.store.register(&request{nonce: nonce, token: tok, createdAt: time.Now(), detail: detail}) {
		return nil, errDuplicate
	}

	return &api.Registration{
		ApproveURL:       fmt.Sprintf("%s/d/%s/%s/approve", s.baseURL, url.PathEscape(nonce), tok),
		DenyURL:          fmt.Sprintf("%s/d/%s/%s/deny", s.baseURL, url.PathEscape(nonce), tok),
		ExpiresInSeconds: int(requestTTL.Seconds()),
	}, nil
}

// registerAuthorized compares the presented bearer token with the
// configured one. ogen has already stripped the "Bearer " prefix.
//
// Constant time, so a wrong token cannot be found a byte at a time by
// measuring how long the comparison takes.
func (s *server) registerAuthorized(presented string) bool {
	if s.registerToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.registerToken)) == 1
}

func isSafeNonce(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// decide handles the tap. Path: /d/<nonce>/<token>/<approve|deny>
// handleTapConfirm renders a confirmation button. The tap link is opened by
// a browser navigation, which is a GET, and a GET must not change anything:
// a link preview, scanner, or speculative prefetch touching the URL would
// otherwise approve a production credential request on the user's behalf.
// Cache-Control alone does not prevent that - the request still arrives.
// So the decision is made by the POST this page submits.
// confirmPage is the page shown before a decision is recorded. It takes
// the parameters rather than a request, and returns the page rather than
// writing it, so server.go can adapt it to the generated handler types
// and so it is testable without an http.ResponseWriter.
//
// verb is validated by the spec's enum before this is reached.
func (s *server) confirmPage(nonce, tok, verb string) page {
	// Peek without deciding, so an expired or unknown link says so here
	// rather than after a pointless tap.
	st, err := s.store.peek(nonce, tok)
	if err != nil {
		return errorPage(err)
	}
	if st.decided != decisionNone {
		return page{http.StatusOK, "Already decided",
			fmt.Sprintf("This request was already %sd.", st.decided), ""}
	}
	return s.autoSubmitPage(nonce, tok, verb)
}

// decide records the decision and publishes it, returning the page the
// person sees.
func (s *server) decide(nonce, tok, verb string) page {
	d := decisionApprove
	if verb == "deny" {
		d = decisionDeny
	}

	res, err := s.store.decide(nonce, tok, d)
	if err != nil {
		return errorPage(err)
	}

	if !res.publish {
		return page{http.StatusOK, "Already decided",
			fmt.Sprintf("This request was already %sd. Nothing further was sent.", res.decision), ""}
	}

	if err := s.publish(res.decision, nonce, s.store.detailFor(nonce)); err != nil {
		s.store.releasePublish(nonce)
		slog.Error("publish failed", "nonce", nonce, "err", err)
		return page{http.StatusBadGateway, "Could not notify",
			"Could not reach the notification service. Tap again to retry.", ""}
	}
	s.store.markPublished(nonce)
	slog.Info("decided", "nonce", nonce, "decision", res.decision)

	title := "Approved"
	if res.decision == decisionDeny {
		title = "Denied"
	}
	return page{http.StatusOK, title,
		fmt.Sprintf("Sent %q. You can close this page.", res.decision), ""}
}

// page is an HTML response: a status, what it says, and optionally a
// form to submit. Returned rather than written, so the decision logic
// does not need an http.ResponseWriter and can be tested without one.
type page struct {
	code  int
	title string
	msg   string
	// form, when set, is the HTML of a button that POSTs back here.
	form string
}

// errorPage renders a failure for a person. A *userError carries a
// status and wording chosen for the reader; anything else is not
// something they can act on, so it says as little as possible.
func errorPage(err error) page {
	var ue *userError
	if errors.As(err, &ue) {
		return page{ue.code, ue.title, ue.msg, ""}
	}
	slog.Error("request failed", "err", err)
	return page{http.StatusInternalServerError, "Error", "Something went wrong.", ""}
}

// publish writes the decision to the response topic, which this service
// holds the only credential for.
func (s *server) publish(d decision, nonce, detail string) error {
	// Deliberately NOT the request context: the decision is already
	// recorded, and the replay guard stops a retry from re-publishing. If
	// the phone closed the connection after tapping, cancelling here would
	// strand the store saying "approved" while the Mac never hears it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.ntfyURL+"/"+s.respTopic, strings.NewReader(decisionBody(d, nonce)))
	if err != nil {
		return fmt.Errorf("build publish request: %w", err)
	}
	if s.ntfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.ntfyToken)
	}
	req.Header.Set("Title", "approval "+string(d))
	if detail != "" {
		req.Header.Set("X-Detail", detail)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("publish to topic %q: %w", s.respTopic, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("publish to topic %q: ntfy returned %s", s.respTopic, resp.Status)
	}
	return nil
}

// decisionBody is the wire format the Mac parses. Defined once so the page
// and the published message cannot drift apart.
func decisionBody(d decision, nonce string) string {
	return string(d) + " " + nonce
}

// autoSubmitPage turns the browser's GET into the POST that actually
// decides, without asking the user anything - they already decided when
// they tapped the notification.
//
// The indirection exists only because a tap opens a URL as a GET, and a GET
// must not have side effects: a link preview, scanner, prefetch or
// automatic retry would otherwise approve a production credential request
// on the user's behalf. Those clients fetch HTML but do not run scripts, so
// submitting from JS is what separates a real tap from a machine touching
// the URL. The noscript fallback keeps it usable if scripting is off.
// autoSubmitPage is the page a tap lands on: it POSTs straight back so
// the decision is never made by a GET, which link previews and
// prefetchers issue freely.
func (s *server) autoSubmitPage(nonce, token, verb string) page {
	action := fmt.Sprintf("/d/%s/%s/%s",
		url.PathEscape(nonce), url.PathEscape(token), url.PathEscape(verb))
	label := "Approve"
	if verb == "deny" {
		label = "Deny"
	}
	return page{
		code:  http.StatusOK,
		title: label,
		msg:   "Sending\u2026",
		form: fmt.Sprintf(`<form id="f" method="POST" action="%s">
<noscript><button type="submit">%s</button></noscript></form>
<script>document.getElementById('f').submit()</script>`,
			html.EscapeString(action), html.EscapeString(label)),
	}
}

// render turns a page into the HTML bytes sent to the browser. One
// place builds the document, so every response - a decision, an
// expiry, an error - looks the same and escapes the same way.
func (p page) render() string {
	return fmt.Sprintf(`<!doctype html><meta name=viewport content="width=device-width,initial-scale=1">
<title>%s</title><style>body{font-family:-apple-system,system-ui,sans-serif;margin:0;
display:flex;min-height:100vh;align-items:center;justify-content:center;background:#111;color:#eee}
div{max-width:28rem;padding:2rem;text-align:center}p{margin:0;color:#aaa}
button{font-size:1.1rem;padding:.9rem 2.5rem;border:0;border-radius:.5rem;background:#2563eb;color:#fff}</style>
<div><h1>%s</h1><p>%s</p>%s</div>`,
		html.EscapeString(p.title), html.EscapeString(p.title),
		html.EscapeString(p.msg), p.form)
}

// routeLogger logs the matched route pattern rather than the raw URI, so
// the capability token in the path is never written anywhere.

// envOr returns the environment value for k, or def when unset.

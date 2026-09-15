// approvald issues one-tap approvals for commands that need a human in the
// loop.
//
// The Mac that asks for approval must not be able to grant it: if one
// credential could both ask and answer, anything running there could
// self-approve and the gate would be decorative. So the Mac's ntfy token is
// write-only on the request topic and read-only on the response topic, and
// only this service holds a credential that can publish a decision.
//
// A tap URL, not a login session, is the credential. It authorizes exactly
// one pending request, so a leaked one grants a single approval rather than
// all of them, and it expires in minutes rather than weeks.
//
// See README.md for the request flow and configuration.
package main

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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.registerAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	nonce := strings.TrimSpace(r.FormValue("nonce"))
	if nonce == "" || len(nonce) > maxNonceLen || !isSafeNonce(nonce) {
		http.Error(w, "invalid nonce", http.StatusBadRequest)
		return
	}
	tok, err := randHex(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	detail := r.FormValue("detail")
	if len(detail) > maxDetailLen {
		detail = detail[:maxDetailLen]
	}
	detail = strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, detail)

	if !s.store.register(&request{nonce: nonce, token: tok, createdAt: time.Now(), detail: detail}) {
		http.Error(w, "nonce already registered", http.StatusConflict)
		return
	}

	approve := fmt.Sprintf("%s/d/%s/%s/approve", s.baseURL, url.PathEscape(nonce), tok)
	deny := fmt.Sprintf("%s/d/%s/%s/deny", s.baseURL, url.PathEscape(nonce), tok)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"approve_url\":%q,\"deny_url\":%q,\"expires_in_seconds\":%d}\n",
		approve, deny, int(requestTTL.Seconds()))
}

func (s *server) registerAuthorized(r *http.Request) bool {
	if s.registerToken == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, p)), []byte(s.registerToken)) == 1
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
func (s *server) handleTapConfirm(w http.ResponseWriter, r *http.Request) {
	nonce := chi.URLParam(r, "nonce")
	tok := chi.URLParam(r, "token")
	verb := chi.URLParam(r, "verb")

	if verb != "approve" && verb != "deny" {
		s.page(w, http.StatusBadRequest, "Bad request", "Unknown action.")
		return
	}

	// Peek without deciding, so an expired or unknown link says so here
	// rather than after a pointless tap.
	if st, err := s.store.peek(nonce, tok); err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			s.page(w, ue.code, ue.title, ue.msg)
			return
		}
		s.page(w, http.StatusInternalServerError, "Error", "Something went wrong.")
		return
	} else if st.decided != decisionNone {
		s.page(w, http.StatusOK, "Already decided",
			fmt.Sprintf("This request was already %sd.", st.decided))
		return
	}

	s.autoSubmitPage(w, nonce, tok, verb)
}

func (s *server) handleTap(w http.ResponseWriter, r *http.Request) {
	nonce := chi.URLParam(r, "nonce")
	tok := chi.URLParam(r, "token")
	verb := chi.URLParam(r, "verb")

	var d decision
	switch verb {
	case "approve":
		d = decisionApprove
	case "deny":
		d = decisionDeny
	default:
		s.page(w, http.StatusBadRequest, "Bad request", "Unknown action.")
		return
	}

	res, err := s.store.decide(nonce, tok, d)
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			s.page(w, ue.code, ue.title, ue.msg)
			return
		}
		s.page(w, http.StatusInternalServerError, "Error", "Something went wrong.")
		return
	}

	if !res.publish {
		s.page(w, http.StatusOK, "Already decided",
			fmt.Sprintf("This request was already %sd. Nothing further was sent.", res.decision))
		return
	}

	if err := s.publish(res.decision, nonce, s.store.detailFor(nonce)); err != nil {
		s.store.releasePublish(nonce)
		slog.Error("publish failed", "nonce", nonce, "err", err)
		s.page(w, http.StatusBadGateway, "Could not notify",
			"Could not reach the notification service. Tap again to retry.")
		return
	}
	s.store.markPublished(nonce)
	slog.Info("decided", "nonce", nonce, "decision", res.decision)

	title := "Approved"
	if res.decision == decisionDeny {
		title = "Denied"
	}
	s.page(w, http.StatusOK, title, fmt.Sprintf("Sent %q. You can close this page.", res.decision))
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
func (s *server) autoSubmitPage(w http.ResponseWriter, nonce, token, verb string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	action := fmt.Sprintf("/d/%s/%s/%s", url.PathEscape(nonce), url.PathEscape(token), url.PathEscape(verb))
	label := "Approve"
	if verb == "deny" {
		label = "Deny"
	}
	fmt.Fprintf(w, `<!doctype html><meta name=viewport content="width=device-width,initial-scale=1">
<title>%s</title><style>body{font-family:-apple-system,system-ui,sans-serif;margin:0;
display:flex;min-height:100vh;align-items:center;justify-content:center;background:#111;color:#eee}
div{max-width:28rem;padding:2rem;text-align:center}p{margin:0;color:#aaa}
button{font-size:1.1rem;padding:.9rem 2.5rem;border:0;border-radius:.5rem;background:#2563eb;color:#fff}</style>
<div><p>Sending%s</p>
<form id="f" method="POST" action="%s">
<noscript><button type="submit">%s</button></noscript></form></div>
<script>document.getElementById('f').submit()</script>`,
		html.EscapeString(label), html.EscapeString("\u2026"),
		html.EscapeString(action), html.EscapeString(label))
}

func (s *server) page(w http.ResponseWriter, code int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A prefetching client must not be able to approve on the user's behalf.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta name=viewport content="width=device-width,initial-scale=1">
<title>%s</title><style>body{font-family:-apple-system,system-ui,sans-serif;margin:0;
display:flex;min-height:100vh;align-items:center;justify-content:center;background:#111;color:#eee}
div{max-width:28rem;padding:2rem;text-align:center}h1{font-size:1.5rem;margin:0 0 .75rem}
p{margin:0;color:#aaa;line-height:1.5}</style><div><h1>%s</h1><p>%s</p></div>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

// routeLogger logs the matched route pattern rather than the raw URI, so
// the capability token in the path is never written anywhere.
func routeLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("request",
			"method", r.Method,
			"route", chi.RouteContext(r.Context()).RoutePattern(),
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// envOr returns the environment value for k, or def when unset.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing required environment variable", "key", k)
		os.Exit(1)
	}
	return v
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	s := &server{
		store:         newStore(),
		baseURL:       strings.TrimSuffix(mustEnv("BASE_URL"), "/"),
		ntfyURL:       strings.TrimSuffix(mustEnv("NTFY_URL"), "/"),
		ntfyToken:     mustEnv("NTFY_TOKEN"),
		respTopic:     mustEnv("RESPONSE_TOPIC"),
		registerToken: mustEnv("REGISTER_TOKEN"),
		client:        &http.Client{Timeout: 10 * time.Second},
	}

	// Runs for the life of the process; the server never returns normally.
	go s.store.reapLoop(context.Background(), time.Minute)

	r := chi.NewRouter()
	// NOT middleware.Logger: it writes r.RequestURI verbatim, which would
	// put live capability tokens in the log on every tap, before the
	// handler even runs. Log the route pattern instead.
	r.Use(routeLogger)
	r.Use(middleware.Recoverer)
	r.Post("/register", s.handleRegister)
	r.Get("/d/{nonce}/{token}/{verb}", s.handleTapConfirm)
	r.Post("/d/{nonce}/{token}/{verb}", s.handleTap)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// The deployment annotates this pod for scraping; without a handler
	// those annotations point at a 404 and the target reads as down.
	r.Handle("/metrics", promhttp.Handler())

	addr := "0.0.0.0:" + envOr("PORT", "3000")
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	slog.Info("approvald started", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

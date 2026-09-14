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
)

const (
	// Bounds how long a leaked tap URL stays useful.
	requestTTL = 10 * time.Minute

	// Retained past the decision so replays are rejected, not re-published.
	retainAfterDecision = 30 * time.Minute

	maxNonceLen = 128
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
	detail    string
}

func (r *request) expired(now time.Time) bool {
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

func (s *store) put(r *request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[r.nonce] = r
}

func (s *store) get(nonce string) (*request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[nonce]
	return r, ok
}

// decide records a decision if none has been made yet, returning the
// decision now in force and whether this call set it. The server is
// authoritative: the Mac also treats the first reply as final, but the two
// halves agreeing should not depend on the client behaving.
func (s *store) decide(nonce string, d decision) (current decision, first bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[nonce]
	if !ok {
		return decisionNone, false, errUnknown
	}
	if r.expired(time.Now()) {
		return decisionNone, false, errExpired
	}
	if r.decided != decisionNone {
		return r.decided, false, nil
	}
	r.decided = d
	r.decidedAt = time.Now()
	return d, true, nil
}

func (s *store) reap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, r := range s.m {
		if r.expired(now) {
			delete(s.m, k)
		}
	}
}

var (
	errUnknown = errors.New("unknown nonce")
	errExpired = errors.New("request expired")
)

type server struct {
	store      *store
	baseURL    string
	ntfyURL    string
	ntfyToken  string
	respTopic  string
	registerPW string
	httpc      *http.Client
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
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	if !s.authedRegister(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	nonce := strings.TrimSpace(r.FormValue("nonce"))
	if nonce == "" || len(nonce) > maxNonceLen || !isSafeNonce(nonce) {
		http.Error(w, "invalid nonce", http.StatusBadRequest)
		return
	}
	if _, exists := s.store.get(nonce); exists {
		// Re-issuing would mint a fresh tap URL for someone else's pending
		// request.
		http.Error(w, "nonce already registered", http.StatusConflict)
		return
	}
	tok, err := randHex(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.store.put(&request{nonce: nonce, token: tok, createdAt: time.Now(), detail: r.FormValue("detail")})

	approve := fmt.Sprintf("%s/d/%s/%s/approve", s.baseURL, url.PathEscape(nonce), tok)
	deny := fmt.Sprintf("%s/d/%s/%s/deny", s.baseURL, url.PathEscape(nonce), tok)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"approve_url\":%q,\"deny_url\":%q,\"expires_in_seconds\":%d}\n",
		approve, deny, int(requestTTL.Seconds()))
}

func (s *server) authedRegister(r *http.Request) bool {
	if s.registerPW == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, p)), []byte(s.registerPW)) == 1
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
func (s *server) decide(w http.ResponseWriter, r *http.Request) {
	nonce := chi.URLParam(r, "nonce")
	tok := chi.URLParam(r, "token")
	verb := chi.URLParam(r, "verb")
	if nonce == "" || tok == "" || verb == "" {
		s.page(w, http.StatusBadRequest, "Bad request", "That link is malformed.")
		return
	}

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

	req, ok := s.store.get(nonce)
	if !ok {
		s.page(w, http.StatusNotFound, "Not found", "No pending request with that id. It may have expired.")
		return
	}
	// Constant-time compare so a wrong token can't be discovered by timing.
	if subtle.ConstantTimeCompare([]byte(req.token), []byte(tok)) != 1 {
		s.page(w, http.StatusForbidden, "Forbidden", "That link is not valid for this request.")
		return
	}
	if req.expired(time.Now()) {
		s.page(w, http.StatusGone, "Expired", "This request expired. Re-run the command to ask again.")
		return
	}

	current, first, err := s.store.decide(nonce, d)
	switch {
	case errors.Is(err, errUnknown):
		s.page(w, http.StatusNotFound, "Not found", "No pending request with that id.")
		return
	case errors.Is(err, errExpired):
		s.page(w, http.StatusGone, "Expired", "This request expired.")
		return
	case err != nil:
		s.page(w, http.StatusInternalServerError, "Error", "Something went wrong.")
		return
	}

	if !first {
		s.page(w, http.StatusOK, "Already decided",
			fmt.Sprintf("This request was already %sd. Nothing further was sent.", current))
		return
	}

	if err := s.publish(string(current), nonce, req.detail); err != nil {
		slog.Error("publish failed", "nonce", nonce, "err", err)
		s.page(w, http.StatusBadGateway, "Could not notify",
			"The decision was recorded but publishing it failed. The command will time out and deny.")
		return
	}
	slog.Info("decided", "nonce", nonce, "decision", current)
	title := "Approved"
	if current == decisionDeny {
		title = "Denied"
	}
	s.page(w, http.StatusOK, title, fmt.Sprintf("Sent %q. You can close this page.", string(current)+" "+nonce))
}

// publish writes the decision to the response topic, which this service
// holds the only credential for.
func (s *server) publish(dec, nonce, detail string) error {
	body := dec + " " + nonce
	req, err := http.NewRequest(http.MethodPost, s.ntfyURL+"/"+s.respTopic, strings.NewReader(body))
	if err != nil {
		return err
	}
	if s.ntfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.ntfyToken)
	}
	req.Header.Set("Title", "approval "+dec)
	if detail != "" {
		req.Header.Set("X-Detail", detail)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy returned %s", resp.Status)
	}
	return nil
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

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing required environment variable", "key", k)
		os.Exit(1)
	}
	return v
}

func main() {
	s := &server{
		store:      newStore(),
		baseURL:    strings.TrimSuffix(mustEnv("BASE_URL"), "/"),
		ntfyURL:    strings.TrimSuffix(mustEnv("NTFY_URL"), "/"),
		ntfyToken:  mustEnv("NTFY_TOKEN"),
		respTopic:  mustEnv("RESPONSE_TOPIC"),
		registerPW: mustEnv("REGISTER_TOKEN"),
		httpc:      &http.Client{Timeout: 10 * time.Second},
	}

	go func() {
		for range time.Tick(time.Minute) {
			s.store.reap()
		}
	}()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Post("/register", s.register)
	r.Get("/d/{nonce}/{token}/{verb}", s.decide)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              "0.0.0.0:3000",
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("approvald started", "addr", ":3000")
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

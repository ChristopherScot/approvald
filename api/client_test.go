package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The header a server uses to see which client versions still call it.
func TestClientSendsVersionHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(ClientVersionHeader)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := NewHTTPClient(HTTPOptions{}).Do(req); err != nil {
		t.Fatal(err)
	}
	if got != ClientVersion {
		t.Errorf("%s = %q, want %q", ClientVersionHeader, got, ClientVersion)
	}
}

func TestRetryIsBoundedAndBreakerOpens(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewHTTPClient(HTTPOptions{
		Policy:  NoRetry{},
		Breaker: &Breaker{Threshold: 3, Cooldown: time.Minute},
	})
	var opened int
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		if _, err := c.Do(req); err == ErrCircuitOpen {
			opened++
		}
	}
	if hits != 3 {
		t.Errorf("upstream saw %d requests, want 3 before the breaker opened", hits)
	}
	if opened != 2 {
		t.Errorf("%d calls rejected by an open circuit, want 2", opened)
	}
}

// A POST may already have applied, so repeating it can create a second
// thing.
func TestPostIsNeverRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	NewHTTPClient(HTTPOptions{}).Do(req)
	if hits != 1 {
		t.Errorf("POST made %d attempts, want 1", hits)
	}
}

func TestIdempotentRequestIsRetriedOnce(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	NewHTTPClient(HTTPOptions{}).Do(req)
	if hits != 2 {
		t.Errorf("GET made %d attempts, want 2 (initial + one retry)", hits)
	}
}

// The server knows when capacity returns; the client does not.
func TestRetryAfterBeatsThePolicyBackoff(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := NewHTTPClient(HTTPOptions{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the retry", resp.StatusCode)
	}
	// SingleRetry would have waited 1s; the server said 2.
	if elapsed < 2*time.Second {
		t.Errorf("waited %v, want >= 2s - the server's Retry-After was ignored", elapsed)
	}
}

// A server can say 3600; sleeping that long inside a request is
// indistinguishable from a hang.
func TestRetryAfterIsCapped(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"3600"}}}
	if _, ok := retryAfter(resp, 30*time.Second); ok {
		t.Error("accepted an hour-long Retry-After")
	}
}

func TestRetryAfterAcceptsAnHTTPDate(t *testing.T) {
	when := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	resp := &http.Response{Header: http.Header{"Retry-After": []string{when}}}
	d, ok := retryAfter(resp, 30*time.Second)
	if !ok {
		t.Fatal("rejected a valid HTTP-date")
	}
	if d < 1*time.Second || d > 4*time.Second {
		t.Errorf("parsed delay = %v, want about 3s", d)
	}
}

func TestUnparseableRetryAfterFallsBackToThePolicy(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"soon-ish"}}}
	if _, ok := retryAfter(resp, 30*time.Second); ok {
		t.Error("accepted an unparseable Retry-After")
	}
}

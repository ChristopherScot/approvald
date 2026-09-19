package api

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	ht "github.com/ogen-go/ogen/http"
)

// Consuming this service from Go needs no publishing step: api/ is
// committed, so another module imports it directly.
//
//	go get github.com/<owner>/approvald@<tag>
//
// A version tag on the service repo is a version of its client. That is
// why this file lives in api/ rather than beside main: a consumer that
// imported only the generated code would get the protocol client with no
// timeout, no retry and no breaker - the defaults are the point.
//
// A caller of this service gets the generated api.Client, which speaks
// the protocol and nothing else: ogen deliberately ships no retries, no
// timeout and no breaker. Its only extension point is WithClient, which
// takes anything with a Do method - so this supplies the parts that
// decide how the service behaves when a dependency is slow or failing.
//
//	c, err := api.NewClient(url, api.WithClient(api.NewHTTPClient(api.HTTPOptions{})))
//
// Defaults are chosen for what they do to the SYSTEM, not for what is
// permissive: one retry rather than five, because five can mean five
// times the traffic to something already struggling.

// ClientVersion is sent on every request as Client-Version, so a
// server can see which client versions are still calling it before
// changing something they depend on. It tracks the spec's info.version.
const ClientVersion = "0.1.0"

// ClientVersionHeader names the header carrying ClientVersion.
//
// No X- prefix: RFC 6648 deprecated it in 2012, because a header that
// becomes a standard cannot shed the prefix without breaking every
// client that sent it. Renamed while nothing read it - the header had
// been write-only since it was added, so this cost nothing, and it
// would have cost a coordinated rollout later.
const ClientVersionHeader = "Client-Version"

// ClientNameHeader carries HTTPOptions.Name: WHICH service is calling,
// where Client-Version says which version of the spec it was built
// against.
//
// Without it every request a server logs looks the same whoever sent
// it, so "something is polling a dead battle once a second" cannot be
// attributed without inference. Version alone does not separate two
// callers built from the same spec.
//
// A plain header rather than W3C Baggage: Baggage earns its complexity
// by surviving multi-hop propagation into spans, and these are
// single-hop calls into a stack that collects logs and metrics and no
// traces. Revisit if a tracing backend ever lands.
const ClientNameHeader = "Client-Name"

// RetryPolicy decides whether a failed request is worth repeating, and
// how long to wait. The length of Backoffs is the retry count.
type RetryPolicy interface {
	Backoffs() []time.Duration
	Retry(req *http.Request, resp *http.Response, err error) bool
}

// SingleRetry retries once, a second later. The default: it covers the
// transient failure - a pod rolling, a connection reset - without showing
// a struggling dependency several times its normal traffic.
type SingleRetry struct{}

func (SingleRetry) Backoffs() []time.Duration { return []time.Duration{time.Second} }
func (SingleRetry) Retry(req *http.Request, resp *http.Response, err error) bool {
	return retryable(req, resp, err)
}

// ExponentialRetry backs off with jitter, for a dependency that is slow
// rather than broken. Five attempts can still mean five times the load,
// so reach for it deliberately.
type ExponentialRetry struct{}

func (ExponentialRetry) Backoffs() []time.Duration {
	out := make([]time.Duration, 5)
	next := 100 * time.Millisecond
	for i := range out {
		// Jitter, so a fleet of callers does not retry in lockstep and
		// arrive at the recovering service as one synchronised wave.
		out[i] = next + time.Duration((rand.Float64()*2-1)*0.25*float64(next))
		next *= 2
	}
	return out
}
func (ExponentialRetry) Retry(req *http.Request, resp *http.Response, err error) bool {
	return retryable(req, resp, err)
}

// NoRetry is for a caller that would rather fail fast.
type NoRetry struct{}

func (NoRetry) Backoffs() []time.Duration                       { return nil }
func (NoRetry) Retry(*http.Request, *http.Response, error) bool { return false }

// retryable repeats only what is safe to repeat: an idempotent method,
// and a failure that says the request did not take effect. A POST may
// already have applied, so repeating it can create a second thing.
func retryable(req *http.Request, resp *http.Response, err error) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
	default:
		return false
	}
	if err != nil {
		return true
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

// ErrCircuitOpen is returned instead of calling a dependency that is
// failing, so a caller fails immediately rather than queueing behind a
// timeout it is going to hit anyway.
var ErrCircuitOpen = errors.New("circuit open")

// Breaker trips after Threshold consecutive failures and lets one request
// through every Cooldown to see whether the dependency has recovered.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration

	mu       sync.Mutex
	failures int
	openedAt time.Time
}

func (b *Breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < b.Threshold {
		return true
	}
	if time.Since(b.openedAt) > b.Cooldown {
		b.failures = 0 // half-open: one probe decides whether to close
		return true
	}
	return false
}

func (b *Breaker) record(failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !failed {
		b.failures = 0
		return
	}
	b.failures++
	if b.failures == b.Threshold {
		b.openedAt = time.Now()
	}
}

// HTTPOptions configure the client. The zero value is the default set.
type HTTPOptions struct {
	// Timeout bounds a single attempt. Zero means 5s; a retry gets its
	// own full timeout.
	Timeout time.Duration
	// Policy defaults to SingleRetry.
	Policy RetryPolicy
	// Breaker is optional; nil disables circuit breaking.
	Breaker *Breaker

	// MaxRetryAfter bounds how long a server's Retry-After can park this
	// client. Zero means 30s. A server may answer with an hour, and
	// sleeping that long inside a request looks exactly like a hang.
	MaxRetryAfter time.Duration

	// Name identifies the SERVICE making the call, sent as Client-Name.
	//
	// The caller's own name - "pokedex-web", not the service it is
	// calling. Empty sends nothing and the server records the caller as
	// unknown, which is honest; guessing from the user agent would put
	// a confident wrong answer in the logs.
	Name string
}

// HTTPClient satisfies ogen's ht.Client.
type HTTPClient struct {
	http          *http.Client
	policy        RetryPolicy
	breaker       *Breaker
	maxRetryAfter time.Duration
	name          string
}

var _ ht.Client = (*HTTPClient)(nil)

// NewHTTPClient builds a client with the defaults filled in.
func NewHTTPClient(o HTTPOptions) *HTTPClient {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	if o.Policy == nil {
		o.Policy = SingleRetry{}
	}
	if o.MaxRetryAfter == 0 {
		o.MaxRetryAfter = 30 * time.Second
	}
	return &HTTPClient{
		http:          &http.Client{Timeout: o.Timeout},
		policy:        o.Policy,
		breaker:       o.Breaker,
		maxRetryAfter: o.MaxRetryAfter,
		name:          o.Name,
	}
}

// retryAfter reads the server's own answer to "when should I come back".
//
// A client that retries a 429 on its own schedule is the reason rate
// limits have to be strict. RFC 9110 allows either a delay in seconds or
// an HTTP-date, and servers send both in the wild.
//
// The cap matters: a server can say 3600, and a client that sleeps for an
// hour inside a request is indistinguishable from one that hung.
func retryAfter(resp *http.Response, max time.Duration) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}

	var d time.Duration
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs < 0 {
			return 0, false
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
		if d < 0 {
			d = 0
		}
	} else {
		return 0, false // unparseable; fall back to the policy's backoff
	}

	if d > max {
		return 0, false // too long to wait inside a request
	}
	return d, true
}

func (c *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c.breaker != nil && !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}
	req.Header.Set(ClientVersionHeader, ClientVersion)
	if c.name != "" {
		req.Header.Set(ClientNameHeader, c.name)
	}

	backoffs := c.policy.Backoffs()
	var (
		resp *http.Response
		err  error
	)
	for attempt := 0; ; attempt++ {
		resp, err = c.http.Do(req)
		if attempt >= len(backoffs) || !c.policy.Retry(req, resp, err) {
			break
		}

		// The server's Retry-After wins over the policy's backoff: it
		// knows when capacity returns and the client does not. Falling
		// back to the policy when the header is absent, unparseable, or
		// further away than maxRetryAfter.
		wait := backoffs[attempt]
		if d, ok := retryAfter(resp, c.maxRetryAfter); ok {
			wait = d
		}

		// Not draining the body here leaks the connection back to the
		// pool unusable.
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(wait)
	}
	if c.breaker != nil {
		c.breaker.record(err != nil || (resp != nil && resp.StatusCode >= http.StatusInternalServerError))
	}
	return resp, err
}

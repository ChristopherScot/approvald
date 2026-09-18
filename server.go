package main

// Everything in this file knows the API exists. main.go does not: it
// calls handler() and starts whatever comes back.
//
// The service implements the generated api.Handler, so adding a path to
// openapi.yml and running `homelabctl regen` makes this fail to compile
// until a method exists for it. That is the point of generating from the
// spec: the contract is checked, not remembered.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ogen-go/ogen/middleware"
	"github.com/ogen-go/ogen/ogenerrors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ChristopherScot/approvald/api"
)

// Request metrics, labelled by the spec's operation ID.
//
// That is load-bearing here beyond the usual cardinality argument: the
// path of a tap contains a live capability token. Labelling by path
// would write every token anyone ever used into the monitoring stack,
// where it outlives the request it authorised. An operation ID comes
// from the spec, so a caller cannot put anything into a label.
var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Requests by operation, method and status.",
	}, []string{"operation", "method", "status"})

	latency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Request latency by operation and method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation", "method"})
)

// observe logs and measures every request that reaches a known
// operation. A request to an unrouted path is rejected by the generated
// router before this runs, so a URL - and the token in it - can never
// become a label or a log field.
//
// This is what replaced chi's middleware.Logger, which writes
// r.RequestURI verbatim and would have logged a live capability token on
// every tap, before the handler even ran.
func observe(req middleware.Request, next middleware.Next) (middleware.Response, error) {
	if req.OperationID == "getHealthz" {
		return next(req)
	}

	start := time.Now()
	resp, err := next(req)

	status := strconv.Itoa(http.StatusOK)
	if err != nil {
		status = strconv.Itoa(http.StatusInternalServerError)
	}
	elapsed := time.Since(start)

	requests.WithLabelValues(req.OperationID, req.Raw.Method, status).Inc()
	latency.WithLabelValues(req.OperationID, req.Raw.Method).Observe(elapsed.Seconds())

	slog.Info("request",
		"method", req.Raw.Method,
		"operation", req.OperationID,
		"status", status,
		"duration_ms", elapsed.Milliseconds())
	return resp, err
}

// service adapts this service's logic to the generated api.Handler.
// The logic itself lives in approval.go and knows nothing about ogen.
type service struct{ s *server }

func (svc service) GetHealthz(context.Context) (*api.Health, error) {
	return &api.Health{Status: api.HealthStatusOk}, nil
}

// Register mints the tap URLs for a nonce. The bearer token was already
// checked by the generated security handler, so by here the caller is
// authorised.
func (svc service) Register(_ context.Context, req *api.RegisterReq) (api.RegisterRes, error) {
	reg, err := svc.s.register(req.Nonce, req.Detail.Or(""))
	switch {
	case errors.Is(err, errBadNonce):
		return &api.RegisterBadRequest{Message: "invalid nonce"}, nil
	case errors.Is(err, errDuplicate):
		return &api.RegisterConflict{Message: "nonce already registered"}, nil
	case err != nil:
		return nil, err
	}
	return reg, nil
}

func (svc service) TapConfirm(_ context.Context, p api.TapConfirmParams) (api.TapConfirmRes, error) {
	pg := svc.s.confirmPage(p.Nonce, p.Token, string(p.Verb))
	switch pg.code {
	case http.StatusNotFound:
		return &api.TapConfirmNotFound{Data: strings.NewReader(pg.render())}, nil
	case http.StatusGone:
		return &api.TapConfirmGone{Data: strings.NewReader(pg.render())}, nil
	}
	return &api.TapConfirmOK{Data: strings.NewReader(pg.render())}, nil
}

func (svc service) Tap(_ context.Context, p api.TapParams) (api.TapRes, error) {
	pg := svc.s.decide(p.Nonce, p.Token, string(p.Verb))
	switch pg.code {
	case http.StatusNotFound:
		return &api.TapNotFound{Data: strings.NewReader(pg.render())}, nil
	case http.StatusGone:
		return &api.TapGone{Data: strings.NewReader(pg.render())}, nil
	case http.StatusBadGateway:
		return &api.TapBadGateway{Data: strings.NewReader(pg.render())}, nil
	}
	return &api.TapOK{Data: strings.NewReader(pg.render())}, nil
}

// NewError renders an error as the spec's Error schema, and logs it -
// ogen returns the error to the caller but does not log it, so without
// this a failure leaves nothing on the server saying what happened.
//
// The status comes from ogen where ogen knows it. This is not only the
// handler's errors: a rejected bearer token, an undecodable body and a
// bad path parameter all arrive here as typed errors carrying their own
// status. Answering 500 to all of them told an unauthorized caller that
// the service was broken, when it had correctly refused them.
//
// Anything without a status of its own is ours and unexpected, so it is
// a 500 - and the message is fixed, because an internal error string is
// not something a caller should be told.
func (svc service) NewError(_ context.Context, err error) *api.ErrorStatusCode {
	code := ogenerrors.ErrorCode(err)
	if code == 0 {
		code = http.StatusInternalServerError
	}

	msg := http.StatusText(code)
	if code == http.StatusInternalServerError {
		slog.Error("handler failed", "err", err)
		msg = "internal error"
	} else {
		slog.Info("request refused", "status", code, "err", err)
	}

	return &api.ErrorStatusCode{
		StatusCode: code,
		Response:   api.Error{Message: msg},
	}
}

// HandleRegisterToken checks the bearer token on POST /register.
//
// Generated from the spec's securitySchemes, so the check cannot be
// forgotten on an operation that declares it - which a hand-written
// `if !authorized` at the top of a handler can be.
func (svc service) HandleRegisterToken(ctx context.Context, _ api.OperationName, t api.RegisterToken) (context.Context, error) {
	if !svc.s.registerAuthorized(t.Token) {
		return ctx, errors.New("unauthorized")
	}
	return ctx, nil
}

func handler() (http.Handler, error) {
	s, err := newServer()
	if err != nil {
		return nil, err
	}
	svc := service{s: s}

	// Runs for the life of the process; expired requests are swept so a
	// nonce cannot be tapped long after the command that asked for it.
	go s.store.reapLoop(context.Background(), time.Minute)

	return handlerWith(svc)
}

// handlerWith builds the routes around an already-constructed service,
// so a test can supply one without the environment this reads from.
func handlerWith(svc service) (http.Handler, error) {
	srv, err := api.NewServer(svc, svc, api.WithMiddleware(observe))
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	// Not in the spec, because it is the platform's endpoint rather than
	// this service's API. The deployment annotates this pod to be scraped
	// here; without it the annotation points at a 404.
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", srv)
	return mux, nil
}

// newServer reads the configuration this service needs. Anything missing
// is fatal rather than defaulted: a blank REGISTER_TOKEN would leave the
// register endpoint open, and a blank NTFY_URL would make every decision
// silently un-notified.
func newServer() (*server, error) {
	var missing []string
	env := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	s := &server{
		store:         newStore(),
		baseURL:       env("BASE_URL"),
		ntfyURL:       env("NTFY_URL"),
		ntfyToken:     env("NTFY_TOKEN"),
		respTopic:     env("RESPONSE_TOPIC"),
		registerToken: env("REGISTER_TOKEN"),
		client:        &http.Client{Timeout: 10 * time.Second},
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}
	return s, nil
}

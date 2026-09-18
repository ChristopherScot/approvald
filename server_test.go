package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ChristopherScot/approvald/api"
)

func testService(t *testing.T) service {
	t.Helper()
	return service{s: &server{
		store:         newStore(),
		baseURL:       "https://approve.example.com",
		ntfyURL:       "http://ntfy.invalid",
		ntfyToken:     "n",
		respTopic:     "r",
		registerToken: "secret",
		client:        http.DefaultClient,
	}}
}

// The bearer check is generated from the spec's securityScheme, so it
// cannot be forgotten on an operation that declares it - unlike an
// `if !authorized` at the top of a handler.
func TestRegisterTokenIsChecked(t *testing.T) {
	svc := testService(t)
	if _, err := svc.HandleRegisterToken(context.Background(), "register",
		api.RegisterToken{Token: "wrong"}); err == nil {
		t.Error("a wrong bearer token was accepted")
	}
	if _, err := svc.HandleRegisterToken(context.Background(), "register",
		api.RegisterToken{Token: "secret"}); err != nil {
		t.Errorf("the correct token was rejected: %v", err)
	}
}

// An empty REGISTER_TOKEN must not mean "anything matches" - that would
// leave the endpoint open if the secret failed to sync.
func TestEmptyRegisterTokenRefusesEveryone(t *testing.T) {
	svc := service{s: &server{store: newStore()}}
	for _, presented := range []string{"", "anything"} {
		if _, err := svc.HandleRegisterToken(context.Background(), "register",
			api.RegisterToken{Token: presented}); err == nil {
			t.Errorf("token %q accepted while none is configured", presented)
		}
	}
}

func TestRegisterMintsBothURLs(t *testing.T) {
	svc := testService(t)
	res, err := svc.Register(context.Background(), &api.RegisterReq{Nonce: "abc123"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg, ok := res.(*api.Registration)
	if !ok {
		t.Fatalf("Register returned %T, want *api.Registration", res)
	}
	if !strings.HasSuffix(reg.ApproveURL, "/approve") || !strings.HasSuffix(reg.DenyURL, "/deny") {
		t.Errorf("URLs are not the two verbs: %+v", reg)
	}
	if reg.ExpiresInSeconds == 0 {
		t.Error("no expiry reported; a caller cannot know when to give up")
	}
}

// The spec documents 400 and 409, so they must come back as those types
// rather than as a generic error.
func TestRegisterRefusalsUseTheDocumentedStatuses(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	if res, _ := svc.Register(ctx, &api.RegisterReq{Nonce: "has spaces"}); func() bool {
		_, ok := res.(*api.RegisterBadRequest)
		return !ok
	}() {
		t.Errorf("an invalid nonce gave %T, want *api.RegisterBadRequest", res)
	}

	if _, err := svc.Register(ctx, &api.RegisterReq{Nonce: "dup"}); err != nil {
		t.Fatal(err)
	}
	if res, _ := svc.Register(ctx, &api.RegisterReq{Nonce: "dup"}); func() bool {
		_, ok := res.(*api.RegisterConflict)
		return !ok
	}() {
		t.Errorf("a duplicate nonce gave %T, want *api.RegisterConflict", res)
	}
}

// A GET must not decide anything: link previews and prefetchers issue
// GETs, and a decision made by one would approve something nobody read.
func TestConfirmPageDoesNotDecide(t *testing.T) {
	svc := testService(t)
	res, err := svc.Register(context.Background(), &api.RegisterReq{Nonce: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	reg := res.(*api.Registration)
	tok := tokenFromURL(t, reg.ApproveURL)

	got, err := svc.TapConfirm(context.Background(), api.TapConfirmParams{
		Nonce: "n1", Token: tok, Verb: api.VerbApprove})
	if err != nil {
		t.Fatalf("TapConfirm: %v", err)
	}
	ok, isOK := got.(*api.TapConfirmOK)
	if !isOK {
		t.Fatalf("TapConfirm gave %T, want *api.TapConfirmOK", got)
	}
	body, _ := io.ReadAll(ok.Data)
	if !strings.Contains(string(body), `method="POST"`) {
		t.Error("the confirm page does not POST, so a GET could decide")
	}
	if st, err := svc.s.store.peek("n1", tok); err != nil || st.decided != decisionNone {
		t.Error("the GET recorded a decision")
	}
}

// The whole point of the metrics labelling: a capability token must
// never reach a label, a log line, or a metric.
func TestTokenNeverReachesMetrics(t *testing.T) {
	h, err := handlerWith(testService(t))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/d/n/supersecrettoken/approve", nil))

	m := httptest.NewRecorder()
	h.ServeHTTP(m, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(m.Body.String(), "supersecrettoken") {
		t.Error("a capability token reached the metrics endpoint")
	}
}

func tokenFromURL(t *testing.T, u string) string {
	t.Helper()
	parts := strings.Split(u, "/")
	if len(parts) < 2 {
		t.Fatalf("cannot find a token in %q", u)
	}
	return parts[len(parts)-2]
}

// A refused request must answer with the status the spec documents, not
// 500. ogen delivers a rejected bearer token, an undecodable body and a
// bad path parameter to NewError as typed errors carrying their own
// status; returning 500 for all of them told an unauthorized caller the
// service was broken when it had correctly refused them.
func TestRefusalsUseTheirOwnStatus(t *testing.T) {
	h, err := handlerWith(testService(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		req  *http.Request
		want int
	}{
		{
			"no bearer token",
			httptest.NewRequest(http.MethodPost, "/register",
				strings.NewReader("nonce=x")),
			http.StatusUnauthorized,
		},
		{
			"wrong bearer token",
			withAuth(httptest.NewRequest(http.MethodPost, "/register",
				strings.NewReader("nonce=x")), "Bearer nope"),
			http.StatusUnauthorized,
		},
		{
			"verb outside the enum",
			httptest.NewRequest(http.MethodGet, "/d/n/t/maybe", nil),
			http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, tc.req)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d - body: %s", w.Code, tc.want, w.Body.String())
			}
			if w.Code == http.StatusInternalServerError {
				t.Error("a refused request reported the service as broken")
			}
		})
	}
}

func withAuth(r *http.Request, v string) *http.Request {
	r.Header.Set("Authorization", v)
	return r
}

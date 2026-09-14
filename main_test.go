package main

import (
	"sync"
	"testing"
	"time"
)

func newPending(t *testing.T, nonce, token string, age time.Duration) *store {
	t.Helper()
	st := newStore()
	if !st.register(&request{nonce: nonce, token: token, createdAt: time.Now().Add(-age)}) {
		t.Fatalf("register(%q) = false, want true", nonce)
	}
	return st
}

// The property the whole security model rests on: concurrent taps must
// agree, and exactly one may publish.
func TestDecideFirstWinsUnderConcurrency(t *testing.T) {
	const taps = 16
	st := newPending(t, "n1", "tok", 0)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		publishes int
		seen      = map[decision]int{}
	)
	for i := 0; i < taps; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := decisionApprove
			if i%2 == 1 {
				d = decisionDeny
			}
			res, err := st.decide("n1", "tok", d)
			if err != nil {
				t.Errorf("decide() error = %v, want nil", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if res.publish {
				publishes++
			}
			seen[res.decision]++
		}(i)
	}
	wg.Wait()

	if publishes != 1 {
		t.Errorf("publishes = %d, want 1", publishes)
	}
	if len(seen) != 1 {
		t.Errorf("decide() returned %d distinct decisions, want 1 (got %v)", len(seen), seen)
	}
}

func TestDecideRejectsWrongToken(t *testing.T) {
	st := newPending(t, "n1", "right", 0)

	if _, err := st.decide("n1", "wrong", decisionApprove); err != errBadToken {
		t.Errorf("decide(wrong token) error = %v, want errBadToken", err)
	}
	// A rejected tap must leave the request decidable by the real link.
	res, err := st.decide("n1", "right", decisionApprove)
	if err != nil || !res.publish || res.decision != decisionApprove {
		t.Errorf("decide(right token) = %+v, %v; want {approve true}, nil", res, err)
	}
}

func TestDecideUnknownNonce(t *testing.T) {
	st := newStore()
	if _, err := st.decide("nope", "tok", decisionApprove); err != errUnknown {
		t.Errorf("decide(unknown) error = %v, want errUnknown", err)
	}
}

func TestDecideExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		wantErr error
	}{
		{"just inside TTL", requestTTL - time.Second, nil},
		{"just outside TTL", requestTTL + time.Second, errExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newPending(t, "n1", "tok", tc.age)
			if _, err := st.decide("n1", "tok", decisionApprove); err != tc.wantErr {
				t.Errorf("decide() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// A decision whose publish failed must stay retriable, or the user taps
// again, is told "already decided", and the Mac never hears.
func TestUnpublishedDecisionStaysRetriable(t *testing.T) {
	st := newPending(t, "n1", "tok", 0)

	res, _ := st.decide("n1", "tok", decisionApprove)
	if !res.publish {
		t.Fatal("first tap: publish = false, want true")
	}
	// Simulate publish failure: the handler releases the claim on error.
	st.releasePublish("n1")
	res, _ = st.decide("n1", "tok", decisionDeny)
	if !res.publish {
		t.Error("retry after failed publish: publish = false, want true")
	}
	if res.decision != decisionApprove {
		t.Errorf("retry decision = %v, want approve (decision must not change)", res.decision)
	}

	st.markPublished("n1")
	res, _ = st.decide("n1", "tok", decisionApprove)
	if res.publish {
		t.Error("tap after successful publish: publish = true, want false")
	}
}

func TestRegisterRejectsDuplicateNonce(t *testing.T) {
	st := newPending(t, "n1", "tok", 0)
	if st.register(&request{nonce: "n1", token: "other", createdAt: time.Now()}) {
		t.Error("register(duplicate) = true, want false")
	}
	// The original token must still be the one that works.
	if _, err := st.decide("n1", "other", decisionApprove); err != errBadToken {
		t.Errorf("decide(second token) error = %v, want errBadToken", err)
	}
}

func TestReapCollectsExpiredOnly(t *testing.T) {
	st := newStore()
	st.register(&request{nonce: "fresh", token: "t", createdAt: time.Now()})
	st.register(&request{nonce: "stale", token: "t", createdAt: time.Now().Add(-requestTTL - time.Minute)})
	st.reap()

	if _, err := st.decide("fresh", "t", decisionApprove); err != nil {
		t.Errorf("fresh survived reap? error = %v, want nil", err)
	}
	if _, err := st.decide("stale", "t", decisionApprove); err != errUnknown {
		t.Errorf("stale after reap: error = %v, want errUnknown", err)
	}
}

func TestIsSafeNonce(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"abc123", true},
		{"a-b_c", true},
		{"../../etc/passwd", false},
		{"has space", false},
		{"semi;colon", false},
		{"", true}, // emptiness is rejected by the caller's length check
	} {
		if got := isSafeNonce(tc.in); got != tc.want {
			t.Errorf("isSafeNonce(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDecisionBody(t *testing.T) {
	if got, want := decisionBody(decisionApprove, "n1"), "approve n1"; got != want {
		t.Errorf("decisionBody() = %q, want %q", got, want)
	}
}

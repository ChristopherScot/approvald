package api

import (
	"context"
	"errors"
	"testing"
)

func pages(t *testing.T) (Fetch[string], *int) {
	t.Helper()
	data := map[string]Page[string]{
		"":   {Items: []string{"a", "b"}, Next: "p2"},
		"p2": {Items: []string{"c", "d"}, Next: "p3"},
		"p3": {Items: []string{"e"}},
	}
	calls := 0
	return func(_ context.Context, cursor string) (Page[string], error) {
		calls++
		return data[cursor], nil
	}, &calls
}

func TestPagedWalksEveryPage(t *testing.T) {
	fetch, calls := pages(t)
	var got []string
	for item, err := range Paged(context.Background(), fetch) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, item)
	}
	if want := "abcde"; string(join(got)) != want {
		t.Errorf("items = %q, want %q", join(got), want)
	}
	if *calls != 3 {
		t.Errorf("fetched %d pages, want 3", *calls)
	}
}

// Breaking out of the loop must stop fetching, or an early exit still
// pays for the whole result set.
func TestPagedStopsFetchingOnBreak(t *testing.T) {
	fetch, calls := pages(t)
	for item := range Paged(context.Background(), fetch) {
		if item == "b" {
			break
		}
	}
	if *calls != 1 {
		t.Errorf("fetched %d pages after an early break, want 1", *calls)
	}
}

func TestPagedReportsAnError(t *testing.T) {
	boom := errors.New("boom")
	fetch := func(context.Context, string) (Page[string], error) {
		return Page[string]{}, boom
	}
	var seen error
	var items int
	for item, err := range Paged(context.Background(), fetch) {
		_ = item
		if err != nil {
			seen = err
			continue
		}
		items++
	}
	if !errors.Is(seen, boom) {
		t.Errorf("error = %v, want %v", seen, boom)
	}
	if items != 0 {
		t.Errorf("yielded %d items alongside an error, want 0", items)
	}
}

// A server that repeats a cursor would otherwise spin forever against
// production.
func TestPagedStopsOnARepeatedCursor(t *testing.T) {
	calls := 0
	fetch := func(context.Context, string) (Page[string], error) {
		calls++
		return Page[string]{Items: []string{"x"}, Next: "same"}, nil
	}
	n := 0
	for range Paged(context.Background(), fetch) {
		if n++; n > 100 {
			t.Fatal("iterated past 100 items on a repeating cursor")
		}
	}
	if calls != 2 {
		t.Errorf("made %d calls on a repeating cursor, want 2", calls)
	}
}

func TestPagedStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fetch, calls := pages(t)
	var seen error
	for _, err := range Paged(ctx, fetch) {
		if err != nil {
			seen = err
		}
	}
	if !errors.Is(seen, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", seen)
	}
	if *calls != 0 {
		t.Errorf("fetched %d pages after cancellation, want 0", *calls)
	}
}

func join(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s
	}
	return out
}

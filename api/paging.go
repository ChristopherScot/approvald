package api

import (
	"context"
	"iter"
)

// Paging. A cursor loop written by hand is easy to get subtly wrong, and
// the failure modes are bad: a forgotten cursor update is an infinite
// loop against a real service, and a missed error check silently
// truncates a result set.
//
// wag generates a per-operation iterator from an `x-paging` extension.
// ogen has no such extension and is not ours to extend, so this is
// generic instead: the caller closes over the generated client method and
// this owns the loop. One implementation covers every paged operation,
// including ones added later.
//
//	for user, err := range Paged(ctx, func(ctx context.Context, cursor string) (Page[User], error) {
//	    res, err := c.ListUsers(ctx, ListUsersParams{After: cursor})
//	    if err != nil {
//	        return Page[User]{}, err
//	    }
//	    return Page[User]{Items: res.Items, Next: res.NextCursor}, nil
//	}) {
//	    if err != nil {
//	        return err
//	    }
//	    // use user
//	}

// Page is one response from a paged operation: the items it carried and
// the cursor for the next call. An empty Next ends the iteration.
type Page[T any] struct {
	Items []T
	Next  string
}

// Fetch requests one page. cursor is empty on the first call.
type Fetch[T any] func(ctx context.Context, cursor string) (Page[T], error)

// Paged iterates every item across pages, fetching as it goes.
//
// The sequence yields an error exactly once, as its final element, and
// stops - so a `range` loop that checks err and returns cannot silently
// process a truncated list. Breaking out of the loop stops fetching.
func Paged[T any](ctx context.Context, fetch Fetch[T]) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var cursor string
		seen := map[string]bool{}

		for {
			// Honour cancellation between pages: a long iteration should
			// stop when its caller has given up.
			if err := ctx.Err(); err != nil {
				var zero T
				yield(zero, err)
				return
			}

			page, err := fetch(ctx, cursor)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}

			for _, item := range page.Items {
				if !yield(item, nil) {
					return
				}
			}

			// A server that repeats a cursor would spin forever; stop
			// rather than hammer it.
			if page.Next == "" || seen[page.Next] {
				return
			}
			seen[page.Next] = true
			cursor = page.Next
		}
	}
}

package store

import "context"

// alertStreamPage is how many rows a stream fetches per round trip. It must
// not exceed what pageLimit accepts: anything larger is silently clamped back
// to 50, which would make the full-page check below misfire and re-query the
// same rows forever.
const alertStreamPage = 500

// streamAlerts walks a filtered alert set a page at a time, calling fn for
// each row in order.
//
// f.Limit is the total cap on rows delivered, not a page size — callers use
// it to bound an export. A zero or negative Limit streams every matching row.
// Paging keeps memory bounded by one page regardless of how large the result
// set is, and the keyset cursor keeps the walk correct when two alerts share
// a timestamp.
//
// The walk is not a snapshot: rows inserted while it runs may or may not be
// seen, depending on where the cursor has reached. Callers that need a
// consistent view have to take a transaction themselves.
func streamAlerts(
	ctx context.Context,
	f AlertFilter,
	list func(context.Context, AlertFilter) ([]Alert, error),
	fn func(Alert) error,
) error {
	remaining := f.Limit
	page := f
	for {
		// A cancelled export should stop querying rather than run to
		// completion writing into a closed connection.
		if err := ctx.Err(); err != nil {
			return err
		}
		page.Limit = alertStreamPage
		if remaining > 0 && remaining < page.Limit {
			page.Limit = remaining
		}
		batch, err := list(ctx, page)
		if err != nil {
			return err
		}
		for _, a := range batch {
			if err := fn(a); err != nil {
				return err
			}
		}
		if remaining > 0 {
			remaining -= len(batch)
			if remaining <= 0 {
				return nil
			}
		}
		// A short page means the filter is exhausted.
		if len(batch) < page.Limit {
			return nil
		}
		page.AfterID = batch[len(batch)-1].ID
	}
}

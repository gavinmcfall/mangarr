package model

import "time"

// ActivityFilter is the query parameter set for the Activity page's
// server-side filtering + pagination. The zero value selects everything
// (no filters, no paging) so callers set only what they need.
type ActivityFilter struct {
	Action     ActivityAction // "" = any action
	SeriesLike string         // "" = any series; case-insensitive substring on series_title
	Tag        string         // "" = any tag; matches series carrying this exact tag
	After      time.Time      // zero = no lower bound (inclusive)
	Limit      int            // 0 = no limit (caller sets a page size)
	Offset     int            // pagination offset
}

// ActivityPage is one page of filtered activity plus the total matching
// count (across all pages) so the UI can render navigation.
type ActivityPage struct {
	Items []ActivityEntry
	Total int
}

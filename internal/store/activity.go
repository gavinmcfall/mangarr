package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// ListActivityFiltered returns one page of activity matching the filter plus
// the total match count across all pages (for navigation). Newest first.
// A zero Limit means no limit.
func (s *Store) ListActivityFiltered(f model.ActivityFilter) (model.ActivityPage, error) {
	var where []string
	var args []any

	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, string(f.Action))
	}
	if f.SeriesLike != "" {
		where = append(where, "series_title LIKE ? COLLATE NOCASE")
		args = append(args, "%"+f.SeriesLike+"%")
	}
	if f.Tag != "" {
		where = append(where, `series_title IN (
			SELECT s.title FROM series s
			JOIN series_tags t ON t.series_id = s.id
			WHERE t.tag = ?)`)
		args = append(args, f.Tag)
	}
	if !f.After.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.After.UTC().Format("2006-01-02 15:04:05"))
	}

	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM activity `+clause, args...).Scan(&total); err != nil {
		return model.ActivityPage{}, fmt.Errorf("ListActivityFiltered count: %w", err)
	}

	q := `SELECT id, ts, series_title, action, detail, via FROM activity ` + clause + ` ORDER BY id DESC`
	pageArgs := append([]any{}, args...)
	if f.Limit > 0 {
		q += " LIMIT ?"
		pageArgs = append(pageArgs, f.Limit)
		if f.Offset > 0 {
			q += " OFFSET ?"
			pageArgs = append(pageArgs, f.Offset)
		}
	}
	rows, err := s.db.Query(q, pageArgs...)
	if err != nil {
		return model.ActivityPage{}, fmt.Errorf("ListActivityFiltered query: %w", err)
	}
	defer rows.Close()
	var out []model.ActivityEntry
	for rows.Next() {
		var e model.ActivityEntry
		var action string
		// Scan ts directly into e.Time, exactly like ListActivity does.
		if err := rows.Scan(&e.ID, &e.Time, &e.SeriesTitle, &action, &e.Detail, &e.Via); err != nil {
			return model.ActivityPage{}, err
		}
		e.Action = model.ActivityAction(action)
		out = append(out, e)
	}
	return model.ActivityPage{Items: out, Total: total}, rows.Err()
}

// DeleteActivityOlderThan removes activity rows with ts strictly before the
// cutoff. Returns the number of rows deleted. The activity-gc task calls
// this with now-minus-retention.
func (s *Store) DeleteActivityOlderThan(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM activity WHERE ts < ?`,
		cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return 0, fmt.Errorf("DeleteActivityOlderThan: %w", err)
	}
	return res.RowsAffected()
}

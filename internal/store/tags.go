package store

import (
	"fmt"
	"sort"
	"strings"
)

func (s *Store) SetSeriesTags(seriesID int64, tags []string) error {
	seen := map[string]struct{}{}
	clean := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		clean = append(clean, t)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("SetSeriesTags begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM series_tags WHERE series_id=?`, seriesID); err != nil {
		return fmt.Errorf("SetSeriesTags delete: %w", err)
	}
	for _, t := range clean {
		if _, err := tx.Exec(`INSERT INTO series_tags (series_id, tag) VALUES (?,?)`, seriesID, t); err != nil {
			return fmt.Errorf("SetSeriesTags insert %q: %w", t, err)
		}
	}
	return tx.Commit()
}

func (s *Store) tagsForSeries(seriesID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT tag FROM series_tags WHERE series_id=? ORDER BY tag`, seriesID)
	if err != nil {
		return nil, fmt.Errorf("tagsForSeries: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ListAllTags() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT tag FROM series_tags ORDER BY tag`)
	if err != nil {
		return nil, fmt.Errorf("ListAllTags: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Strings(out)
	return out, rows.Err()
}

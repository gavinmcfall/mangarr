package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
	_ "modernc.org/sqlite"
)

// sqliteDupColumnMarker is the substring modernc.org/sqlite returns when
// ALTER TABLE ... ADD COLUMN runs against a column that already exists.
// Promoted to a documented constant so a future driver-wording shift is
// a one-line fix rather than a quiet swallow of a legitimate error.
//
// Upstream SQLite source:
//
//	https://github.com/sqlite/sqlite — alter.c, sqlite3AlterFinishAddColumn
//
// NOTE: this is the last single-column inline migration. The next time
// we need to add a column or table, introduce a proper migrations table
// (e.g. a `schema_migrations(version INTEGER PRIMARY KEY)` row + a
// switch over version) instead of stacking more ALTER TABLE swallows.
const sqliteDupColumnMarker = "duplicate column"

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB handle. Used by the backup scheduler.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS series (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL,
  source_path TEXT NOT NULL UNIQUE,
  source TEXT,
  type TEXT,
  status TEXT NOT NULL,
  chapter_count INTEGER DEFAULT 0,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS activity (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts DATETIME DEFAULT CURRENT_TIMESTAMP,
  series_title TEXT,
  action TEXT,
  detail TEXT
);
CREATE TABLE IF NOT EXISTS settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS classification_cache (
  title_norm TEXT PRIMARY KEY,
  type TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	// Additive migrations — idempotent. Each ADD COLUMN runs once per
	// fresh DB; on subsequent boots SQLite reports the dup-column
	// marker and we ignore it. This avoids a separate migrations table
	// for what is currently a one-off Plan B addition; see the const
	// doc comment for the "next time this happens, build the table"
	// rule. Logging on the swallow path makes a future driver-wording
	// change surface fast in operator logs instead of silently breaking
	// the idempotency contract.
	if _, err := s.db.Exec(`ALTER TABLE activity ADD COLUMN via TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), sqliteDupColumnMarker) {
			return fmt.Errorf("store: add activity.via column: %w", err)
		}
		log.Printf("store: activity.via column already present (sqlite ADD COLUMN returned: %v)", err)
	}
	return nil
}

func (s *Store) UpsertSeries(in model.Series) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO series (title, source_path, source, type, status, chapter_count, updated_at)
VALUES (?,?,?,?,?,?,CURRENT_TIMESTAMP)
ON CONFLICT(source_path) DO UPDATE SET
  title=excluded.title, source=excluded.source, type=excluded.type,
  status=excluded.status, chapter_count=excluded.chapter_count, updated_at=CURRENT_TIMESTAMP`,
		in.Title, in.SourcePath, in.Source, string(in.Type), string(in.Status), in.ChapterCount)
	if err != nil {
		return 0, err
	}
	if id, err := res.LastInsertId(); err == nil && id != 0 {
		return id, nil
	}
	var id int64
	err = s.db.QueryRow(`SELECT id FROM series WHERE source_path=?`, in.SourcePath).Scan(&id)
	return id, err
}

func (s *Store) GetSeriesByPath(path string) (model.Series, error) {
	var m model.Series
	var typ, status string
	err := s.db.QueryRow(`SELECT id,title,source,type,status,chapter_count FROM series WHERE source_path=?`, path).
		Scan(&m.ID, &m.Title, &m.Source, &typ, &status, &m.ChapterCount)
	m.SourcePath, m.Type, m.Status = path, model.ContentType(typ), model.Status(status)
	return m, err
}

// GetSeriesByID returns the series with the given primary key. Returns
// sql.ErrNoRows (wrapped) if no such series exists.
func (s *Store) GetSeriesByID(id int64) (model.Series, error) {
	var m model.Series
	var typ, status string
	err := s.db.QueryRow(`SELECT id,title,source_path,source,type,status,chapter_count FROM series WHERE id=?`, id).
		Scan(&m.ID, &m.Title, &m.SourcePath, &m.Source, &typ, &status, &m.ChapterCount)
	m.Type, m.Status = model.ContentType(typ), model.Status(status)
	return m, err
}

func (s *Store) ListSeries() ([]model.Series, error) {
	rows, err := s.db.Query(`SELECT id,title,source_path,source,type,status,chapter_count FROM series ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		var m model.Series
		var typ, status string
		if err := rows.Scan(&m.ID, &m.Title, &m.SourcePath, &m.Source, &typ, &status, &m.ChapterCount); err != nil {
			return nil, err
		}
		m.Type, m.Status = model.ContentType(typ), model.Status(status)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) AddActivity(e model.ActivityEntry) error {
	_, err := s.db.Exec(`INSERT INTO activity (series_title, action, detail, via) VALUES (?,?,?,?)`,
		e.SeriesTitle, string(e.Action), e.Detail, e.Via)
	return err
}

func (s *Store) SaveSettings(set model.Settings) error {
	b, err := json.Marshal(set)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings (id,json) VALUES (1,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json`, string(b))
	return err
}

func (s *Store) GetSettings() (model.Settings, error) {
	var raw string
	err := s.db.QueryRow(`SELECT json FROM settings WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return defaultSettings(), nil
	}
	if err != nil {
		return model.Settings{}, err
	}
	var set model.Settings
	return set, json.Unmarshal([]byte(raw), &set)
}

func defaultSettings() model.Settings {
	return model.Settings{
		FileMode:     model.ModeHardlink,
		RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:  15,
		LibraryRoots: map[model.ContentType]string{},
	}
}

// ListUnmatched returns all series with StatusUnmatched.
func (s *Store) ListUnmatched() ([]model.Series, error) {
	rows, err := s.db.Query(`SELECT id,title,source_path,source,type,status,chapter_count FROM series WHERE status=? ORDER BY title`, string(model.StatusUnmatched))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		var m model.Series
		var typ, status string
		if err := rows.Scan(&m.ID, &m.Title, &m.SourcePath, &m.Source, &typ, &status, &m.ChapterCount); err != nil {
			return nil, err
		}
		m.Type, m.Status = model.ContentType(typ), model.Status(status)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListActivity returns the most recent `limit` activity entries, newest first.
func (s *Store) ListActivity(limit int) ([]model.ActivityEntry, error) {
	rows, err := s.db.Query(`SELECT id,ts,series_title,action,detail,via FROM activity ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ActivityEntry
	for rows.Next() {
		var e model.ActivityEntry
		var action string
		if err := rows.Scan(&e.ID, &e.Time, &e.SeriesTitle, &action, &e.Detail, &e.Via); err != nil {
			return nil, err
		}
		e.Action = model.ActivityAction(action)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetSeriesType updates the type and status for a series, used when a user
// manually assigns a type via the web UI. Status is set to StatusPending so
// the next RunOnce will attempt to file it.
func (s *Store) SetSeriesType(id int64, ct model.ContentType) error {
	_, err := s.db.Exec(`UPDATE series SET type=?, status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		string(ct), string(model.StatusPending), id)
	return err
}

// MarkUnmatched upserts a series with StatusUnmatched. Called by the poller.
func (s *Store) MarkUnmatched(series model.Series) error {
	series.Status = model.StatusUnmatched
	_, err := s.UpsertSeries(series)
	return err
}

// CacheClassification / GetCachedClassification back the AniList cache + remembered manual choices.
func (s *Store) CacheClassification(titleNorm string, t model.ContentType) error {
	_, err := s.db.Exec(`INSERT INTO classification_cache (title_norm,type) VALUES (?,?) ON CONFLICT(title_norm) DO UPDATE SET type=excluded.type`, titleNorm, string(t))
	return err
}

func (s *Store) GetCachedClassification(titleNorm string) (model.ContentType, bool, error) {
	var t string
	err := s.db.QueryRow(`SELECT type FROM classification_cache WHERE title_norm=?`, titleNorm).Scan(&t)
	if err == sql.ErrNoRows {
		return model.TypeUnknown, false, nil
	}
	return model.ContentType(t), err == nil, err
}

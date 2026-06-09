package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

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
	// `?_pragma=foreign_keys(1)` is the modernc.org/sqlite DSN form for
	// enabling SQLite's foreign-key enforcement at connection time. Without
	// it, `REFERENCES … ON DELETE CASCADE` clauses (e.g. on
	// bulk_job_chapters.job_id → bulk_jobs.id) are documentary-only and the
	// cascade never fires. Once-per-process; benefits every table with FKs.
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	// Run the numbered migrations framework alongside the legacy inline
	// migrate() so the Library Bindings v2 tables (bindings,
	// classification_rules) and Migration 2's v1 → v2 settings
	// translation are applied on every boot. Idempotent —
	// schema_versions tracks applied migrations so this is a no-op on
	// subsequent boots.
	if err := runMigrations(s.db); err != nil {
		return nil, err
	}
	// Boot recovery (spec section "Orchestrator state machine"): any
	// bulk_job_chapters rows left in state='fed' from a previous mangarr
	// process that died mid-tick (OOM, SIGKILL, k8s eviction) get demoted
	// to 'pending' so the orchestrator re-feeds them. Suwayomi's enqueue
	// is idempotent, so re-feeding an already-queued chapter is a no-op.
	if _, err := s.db.Exec(`UPDATE bulk_job_chapters SET state='pending' WHERE state='fed'`); err != nil {
		return nil, fmt.Errorf("boot recovery: demote fed→pending: %w", err)
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

// UpsertSeries inserts a row keyed by source_path, or updates an existing
// row's mutable view-model columns (title, source, status, chapter_count,
// updated_at). It deliberately does NOT touch:
//
//   - type — Scanner.ScanAll passes empty Type; the FileOne manual-classify
//     path writes type via SetSeriesType, and we must not silently erase
//     that on the next poll tick. Insert still seeds type=in.Type on fresh
//     rows so a caller can populate it on first observation.
//   - manual_binding_id — same rationale, written by SetSeriesManualBinding
//     and read by the classifier at step 0. The ON CONFLICT clause leaves
//     it untouched so a persisted override survives every poll.
func (s *Store) UpsertSeries(in model.Series) (int64, error) {
	_, err := s.db.Exec(`
INSERT INTO series (title, source_path, source, type, status, chapter_count, updated_at)
VALUES (?,?,?,?,?,?,CURRENT_TIMESTAMP)
ON CONFLICT(source_path) DO UPDATE SET
  title=excluded.title, source=excluded.source,
  status=excluded.status, chapter_count=excluded.chapter_count, updated_at=CURRENT_TIMESTAMP`,
		in.Title, in.SourcePath, in.Source, string(in.Type), string(in.Status), in.ChapterCount)
	if err != nil {
		return 0, err
	}
	// Always resolve the id by the UNIQUE source_path rather than trusting
	// res.LastInsertId(): on the ON CONFLICT DO UPDATE path SQLite does NOT
	// update last_insert_rowid, so it returns a stale rowid from a prior
	// INSERT on the same pooled connection — non-zero and WRONG. The bug was
	// latent until a caller started using the returned id (series.current_
	// binding_id write), at which point every updated row got the wrong id
	// or none. source_path is UNIQUE so this SELECT is exact.
	var id int64
	err = s.db.QueryRow(`SELECT id FROM series WHERE source_path=?`, in.SourcePath).Scan(&id)
	return id, err
}

func (s *Store) GetSeriesByPath(path string) (model.Series, error) {
	var m model.Series
	var typ, status string
	var manual, current sql.NullInt64
	err := s.db.QueryRow(`SELECT id,title,source,type,status,chapter_count,manual_binding_id,current_binding_id FROM series WHERE source_path=?`, path).
		Scan(&m.ID, &m.Title, &m.Source, &typ, &status, &m.ChapterCount, &manual, &current)
	m.SourcePath, m.Type, m.Status = path, model.ContentType(typ), model.Status(status)
	if manual.Valid {
		v := manual.Int64
		m.ManualBindingID = &v
	}
	if current.Valid {
		v := current.Int64
		m.CurrentBindingID = &v
	}
	return m, err
}

// GetSeriesByID returns the series with the given primary key. Returns
// sql.ErrNoRows (wrapped) if no such series exists.
func (s *Store) GetSeriesByID(id int64) (model.Series, error) {
	var m model.Series
	var typ, status string
	var manual, current sql.NullInt64
	err := s.db.QueryRow(`SELECT id,title,source_path,source,type,status,chapter_count,manual_binding_id,current_binding_id FROM series WHERE id=?`, id).
		Scan(&m.ID, &m.Title, &m.SourcePath, &m.Source, &typ, &status, &m.ChapterCount, &manual, &current)
	m.Type, m.Status = model.ContentType(typ), model.Status(status)
	if manual.Valid {
		v := manual.Int64
		m.ManualBindingID = &v
	}
	if current.Valid {
		v := current.Int64
		m.CurrentBindingID = &v
	}
	return m, err
}

func (s *Store) ListSeries() ([]model.Series, error) {
	rows, err := s.db.Query(`SELECT id,title,source_path,source,type,status,chapter_count,manual_binding_id,current_binding_id FROM series ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		var m model.Series
		var typ, status string
		var manual, current sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Title, &m.SourcePath, &m.Source, &typ, &status, &m.ChapterCount, &manual, &current); err != nil {
			return nil, err
		}
		m.Type, m.Status = model.ContentType(typ), model.Status(status)
		if manual.Valid {
			v := manual.Int64
			m.ManualBindingID = &v
		}
		if current.Valid {
			v := current.Int64
			m.CurrentBindingID = &v
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		tags, err := s.tagsForSeries(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Tags = tags
	}
	return out, nil
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
	if err := json.Unmarshal([]byte(raw), &set); err != nil {
		return model.Settings{}, err
	}
	applySettingsDefaults(&set)
	return set, nil
}

func defaultSettings() model.Settings {
	s := model.Settings{
		FileMode:     model.ModeHardlink,
		RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:  15,
		LibraryRoots: map[model.ContentType]string{},
	}
	applySettingsDefaults(&s)
	return s
}

// applySettingsDefaults fills in zero-value fields with their spec'd defaults.
// Called both by defaultSettings (fresh store) and GetSettings (post-unmarshal)
// so that rows stored before a new field was added get the correct default on
// every read without a schema migration.
func applySettingsDefaults(s *model.Settings) {
	if s.BulkMaxInFlight == 0 {
		s.BulkMaxInFlight = 5
	}
	if s.BulkRefillThreshold == 0 {
		s.BulkRefillThreshold = 2
	}
	if s.BulkInterBatchDelaySec == 0 {
		s.BulkInterBatchDelaySec = 1
	}
	if s.BulkStallTimeoutMinutes == 0 {
		s.BulkStallTimeoutMinutes = 30
	}
	if s.BulkChapterMaxRetries == 0 {
		s.BulkChapterMaxRetries = 3
	}
	// BulkAutoErrorEmptyChaptersDisabled: zero value (false) IS the
	// correct default (auto-error enabled), so no defaulting needed here.
	if s.ActivityRetentionDays == 0 {
		s.ActivityRetentionDays = 90
	}
	if s.ReconcileGraceMinutes == 0 {
		s.ReconcileGraceMinutes = 10
	}
	if s.ReconcileMassVanishPercent == 0 {
		s.ReconcileMassVanishPercent = 25
	}
	if s.ReconcileMassVanishMinCount == 0 {
		s.ReconcileMassVanishMinCount = 5
	}
}

// ListUnmatched returns all series with StatusUnmatched.
func (s *Store) ListUnmatched() ([]model.Series, error) {
	rows, err := s.db.Query(`SELECT id,title,source_path,source,type,status,chapter_count,manual_binding_id,current_binding_id FROM series WHERE status=? ORDER BY title`, string(model.StatusUnmatched))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		var m model.Series
		var typ, status string
		var manual, current sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Title, &m.SourcePath, &m.Source, &typ, &status, &m.ChapterCount, &manual, &current); err != nil {
			return nil, err
		}
		m.Type, m.Status = model.ContentType(typ), model.Status(status)
		if manual.Valid {
			v := manual.Int64
			m.ManualBindingID = &v
		}
		if current.Valid {
			v := current.Int64
			m.CurrentBindingID = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetSeriesCurrentBinding records the binding the auto-classifier most
// recently resolved a series to. Pass nil to clear (e.g. on Unmatched).
// The same value goes into series.current_binding_id; the /series page
// reads this to render the auto-classified pill when no manual override
// is set.
func (s *Store) SetSeriesCurrentBinding(id int64, bindingID *int64) error {
	var arg interface{}
	if bindingID == nil {
		arg = nil
	} else {
		if *bindingID == 0 {
			return fmt.Errorf("SetSeriesCurrentBinding: bindingID 0 is not valid; pass nil to clear")
		}
		arg = *bindingID
	}
	_, err := s.db.Exec(`UPDATE series SET current_binding_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		arg, id)
	return err
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
//
// Deprecated under v2 Library Bindings — the classifier no longer reads
// series.type. Kept only for the poller's FileOne path which still
// writes a ContentType on successful manual file. Web reclassify now
// goes through SetSeriesManualBinding.
func (s *Store) SetSeriesType(id int64, ct model.ContentType) error {
	_, err := s.db.Exec(`UPDATE series SET type=?, status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		string(ct), string(model.StatusPending), id)
	return err
}

// SetSeriesManualBinding writes (or clears, when bindingID is nil) the
// user-set override the v2 classifier reads at step 0 of its six-step
// flow. Status is reset to StatusPending so the next RunOnce picks the
// series up and routes via the manual binding.
//
// A non-nil bindingID = 0 is rejected — the "no override" sentinel is
// nil, not zero, so the wire is unambiguous. (The web handler maps the
// "— clear override —" option to nil before calling.)
func (s *Store) SetSeriesManualBinding(id int64, bindingID *int64) error {
	var arg interface{}
	if bindingID == nil {
		arg = nil
	} else {
		if *bindingID == 0 {
			return fmt.Errorf("SetSeriesManualBinding: bindingID 0 is not a valid override; pass nil to clear")
		}
		arg = *bindingID
	}
	_, err := s.db.Exec(`UPDATE series SET manual_binding_id=?, status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		arg, string(model.StatusPending), id)
	return err
}

// MarkUnmatched flips the existing series row to StatusUnmatched. Called
// by the poller after UpsertSeries (which lands the row at the top of
// the RunOnce iteration), so the row is guaranteed to exist.
//
// Status-only UPDATE rather than full UPSERT avoids the double-write
// per unmatched series per tick that the upfront UpsertSeries + a
// MarkUnmatched-as-upsert would otherwise produce. If the row doesn't
// exist (shouldn't happen in normal flow), the UPDATE silently no-ops.
func (s *Store) MarkUnmatched(series model.Series) error {
	_, err := s.db.Exec(
		`UPDATE series SET status=?, updated_at=CURRENT_TIMESTAMP WHERE source_path=?`,
		string(model.StatusUnmatched), series.SourcePath,
	)
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

// ListBindings returns all bindings ordered by name. Stable order matters
// for UI rendering; ID would also be stable but Name reads better in lists.
func (s *Store) ListBindings() ([]model.Binding, error) {
	rows, err := s.db.Query(`SELECT id, name, library_root, kavita_lib_id, default_is_adult FROM bindings ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	defer rows.Close()

	var out []model.Binding
	for rows.Next() {
		var b model.Binding
		var isAdult int64
		if err := rows.Scan(&b.ID, &b.Name, &b.LibraryRoot, &b.KavitaLibID, &isAdult); err != nil {
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		b.DefaultIsAdult = isAdult != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveBindings atomically replaces the entire bindings table contents.
// Existing rows that aren't in the input are deleted. Any input row with
// ID == 0 is treated as new and assigned an autoincremented ID; rows with
// ID > 0 are upserted by ID. Single transaction so partial failures
// don't leave the bindings table in a torn state.
func (s *Store) SaveBindings(in []model.Binding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin save bindings: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("store: rollback save bindings: %v", rbErr)
			}
		}
	}()

	// Collect input IDs (non-zero) to know which existing rows to keep.
	keep := make(map[int64]bool)
	for _, b := range in {
		if b.ID > 0 {
			keep[b.ID] = true
		}
	}

	// Delete existing rows whose IDs are not in the input. Use a NOT IN
	// query if there's anything to keep, or wipe the table if not.
	if len(keep) == 0 {
		if _, err := tx.Exec(`DELETE FROM bindings`); err != nil {
			return fmt.Errorf("delete all bindings: %w", err)
		}
	} else {
		ids := make([]any, 0, len(keep))
		placeholders := make([]string, 0, len(keep))
		for id := range keep {
			ids = append(ids, id)
			placeholders = append(placeholders, "?")
		}
		q := fmt.Sprintf(`DELETE FROM bindings WHERE id NOT IN (%s)`, strings.Join(placeholders, ","))
		if _, err := tx.Exec(q, ids...); err != nil {
			return fmt.Errorf("prune bindings: %w", err)
		}
	}

	// Upsert each input row. SQLite supports ON CONFLICT(id) DO UPDATE.
	for i := range in {
		b := &in[i]
		isAdult := int64(0)
		if b.DefaultIsAdult {
			isAdult = 1
		}
		if b.ID == 0 {
			res, err := tx.Exec(
				`INSERT INTO bindings (name, library_root, kavita_lib_id, default_is_adult) VALUES (?, ?, ?, ?)`,
				b.Name, b.LibraryRoot, b.KavitaLibID, isAdult,
			)
			if err != nil {
				return fmt.Errorf("insert binding %q: %w", b.Name, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("get insert id for binding %q: %w", b.Name, err)
			}
			b.ID = id
		} else {
			_, err := tx.Exec(
				`INSERT INTO bindings (id, name, library_root, kavita_lib_id, default_is_adult)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO UPDATE SET name=excluded.name, library_root=excluded.library_root,
				   kavita_lib_id=excluded.kavita_lib_id, default_is_adult=excluded.default_is_adult`,
				b.ID, b.Name, b.LibraryRoot, b.KavitaLibID, isAdult,
			)
			if err != nil {
				return fmt.Errorf("upsert binding id=%d: %w", b.ID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save bindings: %w", err)
	}
	committed = true
	return nil
}

// ListRules returns all classification rules sorted ascending by Priority
// so the classifier can walk them first-match-wins.
func (s *Store) ListRules() ([]model.ClassificationRule, error) {
	rows, err := s.db.Query(`SELECT id, priority, name, condition_json, binding_id FROM classification_rules ORDER BY priority`)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	var out []model.ClassificationRule
	for rows.Next() {
		var r model.ClassificationRule
		var condJSON string
		if err := rows.Scan(&r.ID, &r.Priority, &r.Name, &condJSON, &r.BindingID); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		if err := json.Unmarshal([]byte(condJSON), &r.Condition); err != nil {
			return nil, fmt.Errorf("unmarshal rule %d condition: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveRules atomically replaces the classification_rules table contents.
// Same shape as SaveBindings: ID==0 rows are inserted (and the input slice
// gets its IDs populated in place), ID>0 rows are upserted by ID, existing
// rows not in the input are deleted. Single transaction so partial
// failures don't leave the table in a torn state.
func (s *Store) SaveRules(in []model.ClassificationRule) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin save rules: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("store: rollback save rules: %v", rbErr)
			}
		}
	}()

	keep := make(map[int64]bool)
	for _, r := range in {
		if r.ID > 0 {
			keep[r.ID] = true
		}
	}

	if len(keep) == 0 {
		if _, err := tx.Exec(`DELETE FROM classification_rules`); err != nil {
			return fmt.Errorf("delete all rules: %w", err)
		}
	} else {
		ids := make([]any, 0, len(keep))
		placeholders := make([]string, 0, len(keep))
		for id := range keep {
			ids = append(ids, id)
			placeholders = append(placeholders, "?")
		}
		q := fmt.Sprintf(`DELETE FROM classification_rules WHERE id NOT IN (%s)`, strings.Join(placeholders, ","))
		if _, err := tx.Exec(q, ids...); err != nil {
			return fmt.Errorf("prune rules: %w", err)
		}
	}

	for i := range in {
		r := &in[i]
		condJSON, err := json.Marshal(r.Condition)
		if err != nil {
			return fmt.Errorf("marshal rule %q condition: %w", r.Name, err)
		}
		if r.ID == 0 {
			res, err := tx.Exec(
				`INSERT INTO classification_rules (priority, name, condition_json, binding_id) VALUES (?, ?, ?, ?)`,
				r.Priority, r.Name, string(condJSON), r.BindingID,
			)
			if err != nil {
				return fmt.Errorf("insert rule %q: %w", r.Name, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("get insert id for rule %q: %w", r.Name, err)
			}
			r.ID = id
		} else {
			_, err := tx.Exec(
				`INSERT INTO classification_rules (id, priority, name, condition_json, binding_id)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO UPDATE SET priority=excluded.priority, name=excluded.name,
				   condition_json=excluded.condition_json, binding_id=excluded.binding_id`,
				r.ID, r.Priority, r.Name, string(condJSON), r.BindingID,
			)
			if err != nil {
				return fmt.Errorf("upsert rule id=%d: %w", r.ID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save rules: %w", err)
	}
	committed = true
	return nil
}

// SeriesLite is the minimal projection the reconcile pass needs.
type SeriesLite struct {
	ID           int64
	SourcePath   string
	Status       model.Status
	MissingSince *time.Time
}

// ListSeriesLite returns id, source_path, status, missing_since for every
// series. Cheap projection used by the reconcile pass.
func (s *Store) ListSeriesLite() ([]SeriesLite, error) {
	rows, err := s.db.Query(`SELECT id,source_path,status,missing_since FROM series`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesLite
	for rows.Next() {
		var l SeriesLite
		var status string
		var ms sql.NullInt64
		if err := rows.Scan(&l.ID, &l.SourcePath, &status, &ms); err != nil {
			return nil, err
		}
		l.Status = model.Status(status)
		if ms.Valid {
			t := time.Unix(ms.Int64, 0).UTC()
			l.MissingSince = &t
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetSeriesMissingSince sets (or clears, when t is nil) series.missing_since.
//
// Deliberately does not touch updated_at: missing_since is an internal
// reconcile grace-timer, not a content change. The user-visible orphan flip
// (SetSeriesStatus) is what bumps updated_at.
func (s *Store) SetSeriesMissingSince(id int64, t *time.Time) error {
	if t == nil {
		_, err := s.db.Exec(`UPDATE series SET missing_since=NULL WHERE id=?`, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE series SET missing_since=? WHERE id=?`, t.Unix(), id)
	return err
}

// SetSeriesStatus updates series.status.
func (s *Store) SetSeriesStatus(id int64, st model.Status) error {
	_, err := s.db.Exec(`UPDATE series SET status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, string(st), id)
	return err
}

// DeleteSeries removes the series row and its tags. Idempotent: deleting a
// non-existent id is a no-op, not an error. Does NOT touch files on disk;
// file removal is the caller's concern (web binSeriesFiles).
func (s *Store) DeleteSeries(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM series_tags WHERE series_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM series WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

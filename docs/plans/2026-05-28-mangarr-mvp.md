# mangarr MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the mangarr MVP — a Go service that watches the manga download folder, classifies each series by AniList country-of-origin, files it into the correct Kavita type-library (hardlink/move/copy + optional rename), triggers a Kavita scan, and exposes a small embedded web UI (Series / Unmatched / Activity / Settings).

**Architecture:** Single Go binary, pure-Go SQLite for state, stdlib `net/http` (Go 1.22 routing), `go:embed`-ed HTMX UI. Internal packages are independently testable: `config`, `store`, `scanner`, `classifier`, `filer`, `kavita`, `poller`, `web`. A `poller` orchestrates scan→classify→file→notify on a schedule. Organizer-only: no downloading.

**Tech Stack:** Go 1.23 · `modernc.org/sqlite` (CGO-free) · stdlib `net/http` + `html/template` · `go:embed` · HTMX (vendored JS) · AniList GraphQL · Kavita REST.

---

## File Structure

```
mangarr/
├── go.mod
├── main.go                      # compose + start: load config, open store, start poller + web
├── Makefile                     # build/test/run/lint targets
├── Dockerfile                   # multi-stage; scratch/distroless final
├── .github/workflows/build.yaml # CI: test + build + push image to GHCR
├── internal/
│   ├── config/config.go         # env → Config struct, validation
│   ├── config/config_test.go
│   ├── model/model.go           # shared types: Series, Chapter, ContentType, Status, Settings, ActivityEntry
│   ├── store/store.go           # SQLite: schema, CRUD for series/unmatched/activity/settings
│   ├── store/store_test.go
│   ├── scanner/scanner.go       # walk download roots, parse ComicInfo.xml, enumerate chapters
│   ├── scanner/scanner_test.go
│   ├── classifier/classifier.go # AniList client, countryOfOrigin → ContentType, cache via store
│   ├── classifier/classifier_test.go
│   ├── filer/filer.go           # rename scheme render, hardlink/move/copy, idempotent place()
│   ├── filer/filer_test.go
│   ├── kavita/kavita.go         # authenticate + trigger library scan
│   ├── kavita/kavita_test.go
│   ├── poller/poller.go         # orchestrate one pass: scan→classify→file→notify→record
│   ├── poller/poller_test.go
│   └── web/
│       ├── web.go               # router, handlers (JSON actions + HTMX pages)
│       ├── web_test.go
│       ├── templates/*.html     # series, unmatched, activity, settings
│       └── static/htmx.min.js
└── docs/ (DESIGN.md, this plan)
```

`model` holds the shared types so every package references one definition (avoids the "clearLayers vs clearFullLayers" drift). Files split by responsibility; each is small and focused.

---

## Task 1: Project scaffold + CI-free build

**Files:**
- Create: `go.mod`, `main.go`, `Makefile`, `internal/model/model.go`

- [ ] **Step 1: Init the module**

Run:
```bash
cd ~/my_other_repos/mangarr
go mod init github.com/gavinmcfall/mangarr
go get modernc.org/sqlite@latest
```

- [ ] **Step 2: Define shared types** — `internal/model/model.go`

```go
package model

import "time"

type ContentType string

const (
	TypeManga   ContentType = "Manga"
	TypeManhwa  ContentType = "Manhwa"
	TypeManhua  ContentType = "Manhua"
	TypeUnknown ContentType = ""
)

// CountryToType maps an AniList countryOfOrigin (ISO-3166 alpha-2) to a library type.
func CountryToType(country string) ContentType {
	switch country {
	case "JP":
		return TypeManga
	case "KR":
		return TypeManhwa
	case "CN", "TW":
		return TypeManhua
	default:
		return TypeUnknown
	}
}

type Status string

const (
	StatusPending   Status = "pending"   // discovered, not yet classified
	StatusUnmatched Status = "unmatched" // classification failed, awaiting manual
	StatusFiled     Status = "filed"     // placed into a library
	StatusError     Status = "error"
)

type Series struct {
	ID          int64
	Title       string      // from ComicInfo.xml <Series>, else folder name
	SourcePath  string      // absolute path under a download root
	Source      string      // e.g. "suwayomi" / "tranga"
	Type        ContentType // resolved type (or TypeUnknown)
	Status      Status
	ChapterCount int
	UpdatedAt   time.Time
}

type ActivityEntry struct {
	ID        int64
	Time      time.Time
	SeriesTitle string
	Action    string // "filed", "unmatched", "scan-triggered", "error"
	Detail    string
}

type FileMode string

const (
	ModeHardlink FileMode = "hardlink"
	ModeMove     FileMode = "move"
	ModeCopy     FileMode = "copy"
)

// Settings is the single mutable config row (id=1).
type Settings struct {
	FileMode      FileMode
	RenameScheme  string            // e.g. "{series}/{series} - Ch.{chapter}.cbz"
	PollMinutes   int
	LibraryRoots  map[ContentType]string // Manga -> /media/Library/Books/Manga, ...
	KavitaBaseURL string
	KavitaLibIDs  []int             // libraries to scan after filing
}
```

- [ ] **Step 3: Minimal main + Makefile**

`main.go`:
```go
package main

import (
	"log"
)

func main() {
	log.Println("mangarr starting")
}
```

`Makefile`:
```makefile
.PHONY: build test run tidy
build:
	CGO_ENABLED=0 go build -o bin/mangarr .
test:
	go test ./...
run:
	go run .
tidy:
	go mod tidy
```

- [ ] **Step 4: Verify it builds**

Run: `make build`
Expected: produces `bin/mangarr`, exit 0.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum main.go Makefile internal/model/model.go
git commit -m "feat: project scaffold + shared model types"
```

---

## Task 2: config package

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

- [ ] **Step 1: Write failing test** — `internal/config/config_test.go`

```go
package config

import (
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "/media/Downloads/suwayomi,/media/Downloads/tranga")
	t.Setenv("MANGARR_DB_PATH", "/config/mangarr.db")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.DownloadRoots) != 2 {
		t.Fatalf("want 2 roots, got %d", len(c.DownloadRoots))
	}
	if c.DBPath != "/config/mangarr.db" {
		t.Fatalf("want db path, got %q", c.DBPath)
	}
}

func TestLoadRequiresRoots(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when no download roots set")
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/config/ -v`
Expected: FAIL (Load undefined).

- [ ] **Step 3: Implement** — `internal/config/config.go`

```go
package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	DownloadRoots []string
	DBPath        string
	HTTPAddr      string
}

func Load() (Config, error) {
	rootsRaw := strings.TrimSpace(os.Getenv("MANGARR_DOWNLOAD_ROOTS"))
	if rootsRaw == "" {
		return Config{}, fmt.Errorf("MANGARR_DOWNLOAD_ROOTS is required")
	}
	var roots []string
	for _, r := range strings.Split(rootsRaw, ",") {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, r)
		}
	}
	db := os.Getenv("MANGARR_DB_PATH")
	if db == "" {
		db = "/config/mangarr.db"
	}
	addr := os.Getenv("MANGARR_HTTP_ADDR")
	if addr == "" {
		addr = ":8590"
	}
	return Config{DownloadRoots: roots, DBPath: db, HTTPAddr: addr}, nil
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): env loading with required download roots"
```

---

## Task 3: store package (SQLite)

**Files:**
- Create: `internal/store/store.go`, `internal/store/store_test.go`

- [ ] **Step 1: Write failing test** — `internal/store/store_test.go`

```go
package store

import (
	"path/filepath"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndGetSeries(t *testing.T) {
	s := newTestStore(t)
	in := model.Series{Title: "Solo Leveling", SourcePath: "/dl/suwayomi/Solo Leveling", Source: "suwayomi", Status: model.StatusPending}
	id, err := s.UpsertSeries(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetSeriesByPath(in.SourcePath)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id || got.Title != "Solo Leveling" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	want := model.Settings{
		FileMode:     model.ModeHardlink,
		RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:  15,
		LibraryRoots: map[model.ContentType]string{model.TypeManhwa: "/media/Library/Books/Manhwa"},
		KavitaBaseURL: "http://kavita:5000",
		KavitaLibIDs:  []int{2},
	}
	if err := s.SaveSettings(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetSettings()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FileMode != want.FileMode || got.LibraryRoots[model.TypeManhwa] != "/media/Library/Books/Manhwa" {
		t.Fatalf("settings mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/store/ -v`
Expected: FAIL (Open undefined).

- [ ] **Step 3: Implement** — `internal/store/store.go`

```go
package store

import (
	"database/sql"
	"encoding/json"

	"github.com/gavinmcfall/mangarr/internal/model"
	_ "modernc.org/sqlite"
)

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
	return err
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
	_, err := s.db.Exec(`INSERT INTO activity (series_title, action, detail) VALUES (?,?,?)`,
		e.SeriesTitle, e.Action, e.Detail)
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
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): sqlite schema + series/settings/activity/cache CRUD"
```

---

## Task 4: scanner package (ComicInfo.xml + chapters)

**Files:**
- Create: `internal/scanner/scanner.go`, `internal/scanner/scanner_test.go`

- [ ] **Step 1: Write failing test** — `internal/scanner/scanner_test.go`

```go
package scanner

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func writeCBZ(t *testing.T, path, comicInfo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	if comicInfo != "" {
		w, _ := zw.Create("ComicInfo.xml")
		w.Write([]byte(comicInfo))
	}
	img, _ := zw.Create("001.jpg")
	img.Write([]byte("x"))
	zw.Close()
}

func TestScanReadsSeriesFromComicInfo(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Weeb Central", "Solo Leveling")
	writeCBZ(t, filepath.Join(dir, "Ch. 001.cbz"),
		`<?xml version="1.0"?><ComicInfo><Series>Solo Leveling</Series><Number>1</Number></ComicInfo>`)
	writeCBZ(t, filepath.Join(dir, "Ch. 002.cbz"),
		`<?xml version="1.0"?><ComicInfo><Series>Solo Leveling</Series><Number>2</Number></ComicInfo>`)

	got, err := Scan(root, "weeb")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 series, got %d", len(got))
	}
	if got[0].Title != "Solo Leveling" || got[0].ChapterCount != 2 {
		t.Fatalf("bad series: %+v", got[0])
	}
}

func TestScanFallsBackToFolderName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "SomeSource", "Mystery Title")
	writeCBZ(t, filepath.Join(dir, "Ch. 001.cbz"), "") // no ComicInfo
	got, err := Scan(root, "x")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Mystery Title" {
		t.Fatalf("want folder-name fallback, got %+v", got)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/scanner/ -v`
Expected: FAIL (Scan undefined).

- [ ] **Step 3: Implement** — `internal/scanner/scanner.go`

```go
package scanner

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

type comicInfo struct {
	Series string `xml:"Series"`
}

// Scan walks one download root. A "series" is any directory that directly
// contains .cbz files. Title comes from the first CBZ's ComicInfo.xml <Series>,
// falling back to the directory's base name.
func Scan(root, source string) ([]model.Series, error) {
	bySeriesDir := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; don't abort the whole scan
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".cbz") {
			dir := filepath.Dir(path)
			bySeriesDir[dir] = append(bySeriesDir[dir], path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var out []model.Series
	for dir, cbzs := range bySeriesDir {
		title := seriesTitleFromCBZ(cbzs[0])
		if title == "" {
			title = filepath.Base(dir)
		}
		out = append(out, model.Series{
			Title:        title,
			SourcePath:   dir,
			Source:       source,
			Status:       model.StatusPending,
			ChapterCount: len(cbzs),
		})
	}
	return out, nil
}

func seriesTitleFromCBZ(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, "ComicInfo.xml") {
			rc, err := f.Open()
			if err != nil {
				return ""
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return ""
			}
			var ci comicInfo
			if xml.Unmarshal(data, &ci) == nil {
				return strings.TrimSpace(ci.Series)
			}
		}
	}
	return ""
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/scanner/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scanner/
git commit -m "feat(scanner): discover series, parse ComicInfo with folder fallback"
```

---

## Task 5: classifier package (AniList)

**Files:**
- Create: `internal/classifier/classifier.go`, `internal/classifier/classifier_test.go`

- [ ] **Step 1: Write failing test** — `internal/classifier/classifier_test.go`

```go
package classifier

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestClassifyMapsCountry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"KR"}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Classify("Solo Leveling")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got != model.TypeManhwa {
		t.Fatalf("want Manhwa, got %q", got)
	}
}

func TestClassifyNoMatchReturnsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":null}}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL).Classify("zzzznotreal")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got != model.TypeUnknown {
		t.Fatalf("want Unknown, got %q", got)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/classifier/ -v`
Expected: FAIL (New undefined).

- [ ] **Step 3: Implement** — `internal/classifier/classifier.go`

```go
package classifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

const defaultEndpoint = "https://graphql.anilist.co"

type Classifier struct {
	endpoint string
	http     *http.Client
}

func New(endpoint string) *Classifier {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Classifier{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}}
}

const query = `query ($s: String) { Media(search: $s, type: MANGA) { countryOfOrigin } }`

type anilistResp struct {
	Data struct {
		Media *struct {
			CountryOfOrigin string `json:"countryOfOrigin"`
		} `json:"Media"`
	} `json:"data"`
}

// Classify returns the content type for a title, or TypeUnknown if AniList has no match.
func (c *Classifier) Classify(title string) (model.ContentType, error) {
	body, _ := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]string{"s": title},
	})
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return model.TypeUnknown, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return model.TypeUnknown, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return model.TypeUnknown, fmt.Errorf("anilist rate limited")
	}
	var out anilistResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return model.TypeUnknown, err
	}
	if out.Data.Media == nil {
		return model.TypeUnknown, nil
	}
	return model.CountryToType(out.Data.Media.CountryOfOrigin), nil
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/classifier/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/
git commit -m "feat(classifier): AniList countryOfOrigin lookup -> content type"
```

---

## Task 6: filer package (rename + hardlink/move/copy)

**Files:**
- Create: `internal/filer/filer.go`, `internal/filer/filer_test.go`

- [ ] **Step 1: Write failing test** — `internal/filer/filer_test.go`

```go
package filer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestRenderName(t *testing.T) {
	got := RenderName("{series}/{series} - Ch.{chapter}.cbz", "Solo Leveling", "Ch. 001.cbz")
	want := "Solo Leveling/Solo Leveling - Ch.001.cbz"
	if got != want {
		t.Fatalf("want %q got %q", want, got)
	}
}

func TestFileHardlinkIdempotent(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "Solo Leveling")
	dstRoot := filepath.Join(tmp, "lib", "Manhwa")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "Ch. 001.cbz"), []byte("data"), 0o644)

	f := &Filer{Mode: model.ModeHardlink, Scheme: "{series}/{series} - Ch.{chapter}.cbz"}
	if err := f.File("Solo Leveling", src, dstRoot); err != nil {
		t.Fatalf("file: %v", err)
	}
	out := filepath.Join(dstRoot, "Solo Leveling", "Solo Leveling - Ch.001.cbz")
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected filed chapter: %v", err)
	}
	// second run must be a no-op, not an error
	if err := f.File("Solo Leveling", src, dstRoot); err != nil {
		t.Fatalf("second file run should be idempotent: %v", err)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/filer/ -v`
Expected: FAIL (RenderName/Filer undefined).

- [ ] **Step 3: Implement** — `internal/filer/filer.go`

```go
package filer

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

var chapterNum = regexp.MustCompile(`(\d+(?:\.\d+)?)`)

// RenderName fills the scheme. {series} -> series title, {chapter} -> numeric
// portion of the original filename (zero-decimals preserved as written).
func RenderName(scheme, series, origFile string) string {
	ch := strings.TrimSuffix(origFile, filepath.Ext(origFile))
	if m := chapterNum.FindString(origFile); m != "" {
		ch = m
	}
	out := strings.ReplaceAll(scheme, "{series}", series)
	out = strings.ReplaceAll(out, "{chapter}", ch)
	return out
}

type Filer struct {
	Mode   model.FileMode
	Scheme string
}

// File places every .cbz from srcDir into dstRoot per the scheme + mode.
// Idempotent: existing destinations are skipped.
func (f *Filer) File(series, srcDir, dstRoot string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".cbz") {
			continue
		}
		rel := RenderName(f.Scheme, series, e.Name())
		dst := filepath.Join(dstRoot, rel)
		if _, err := os.Stat(dst); err == nil {
			continue // already filed
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		src := filepath.Join(srcDir, e.Name())
		if err := f.place(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func (f *Filer) place(src, dst string) error {
	switch f.Mode {
	case model.ModeMove:
		return os.Rename(src, dst)
	case model.ModeCopy:
		return copyFile(src, dst)
	default: // hardlink, fall back to copy on cross-device
		if err := os.Link(src, dst); err != nil {
			return copyFile(src, dst)
		}
		return nil
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/filer/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/filer/
git commit -m "feat(filer): rename scheme + idempotent hardlink/move/copy"
```

---

## Task 7: kavita package (scan trigger)

**Files:**
- Create: `internal/kavita/kavita.go`, `internal/kavita/kavita_test.go`

- [ ] **Step 1: Write failing test** — `internal/kavita/kavita_test.go`

```go
package kavita

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScanAuthenticatesThenScans(t *testing.T) {
	var hitAuth, hitScan bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/plugin/authenticate"):
			hitAuth = true
			w.Write([]byte(`{"token":"jwt123"}`))
		case strings.Contains(r.URL.Path, "/api/library/scan"):
			hitScan = true
			if r.Header.Get("Authorization") != "Bearer jwt123" {
				t.Errorf("missing bearer token")
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "APIKEY")
	if err := c.ScanLibrary(2); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !hitAuth || !hitScan {
		t.Fatalf("auth=%v scan=%v", hitAuth, hitScan)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/kavita/ -v`
Expected: FAIL (New undefined).

- [ ] **Step 3: Implement** — `internal/kavita/kavita.go`

```go
package kavita

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

func New(base, apiKey string) *Client {
	return &Client{base: base, apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) authenticate() (string, error) {
	u := fmt.Sprintf("%s/api/plugin/authenticate?apiKey=%s&pluginName=mangarr", c.base, url.QueryEscape(c.apiKey))
	resp, err := c.http.Post(u, "application/json", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kavita auth status %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// ScanLibrary triggers a scan of one Kavita library by ID.
func (c *Client) ScanLibrary(libraryID int) error {
	token, err := c.authenticate()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]int{"libraryId": libraryID})
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/library/scan", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("kavita scan status %d", resp.StatusCode)
	}
	return nil
}
```

> NOTE for implementer: confirm the exact scan endpoint/payload against the running Kavita's Swagger (`/api/library/scan` body shape can vary by version — `KavitaController` exposes `scan`). Adjust the request in `ScanLibrary` if the live API differs; the test pins the contract this code assumes.

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/kavita/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/kavita/
git commit -m "feat(kavita): authenticate + trigger library scan"
```

---

## Task 8: poller package (orchestration)

**Files:**
- Create: `internal/poller/poller.go`, `internal/poller/poller_test.go`

- [ ] **Step 1: Write failing test** — `internal/poller/poller_test.go`

```go
package poller

import (
	"path/filepath"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// fakes
type fakeClassifier struct{ t model.ContentType }
func (f fakeClassifier) Classify(string) (model.ContentType, error) { return f.t, nil }

type fakeScanner struct{ out []model.Series }
func (f fakeScanner) ScanAll() ([]model.Series, error) { return f.out, nil }

type recorder struct {
	filed     []model.Series
	scanned   []int
	unmatched []model.Series
}
func (r *recorder) File(series model.Series, dstRoot string) error { r.filed = append(r.filed, series); return nil }
func (r *recorder) Scan(libID int) error                          { r.scanned = append(r.scanned, libID); return nil }
func (r *recorder) MarkUnmatched(s model.Series) error            { r.unmatched = append(r.unmatched, s); return nil }

func TestRunOnceFilesAndScans(t *testing.T) {
	s := model.Series{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		LibraryRoots: map[model.ContentType]string{model.TypeManhwa: filepath.FromSlash("/lib/Manhwa")},
		LibraryIDs:   map[model.ContentType]int{model.TypeManhwa: 2},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.filed) != 1 || rec.filed[0].Type != model.TypeManhwa {
		t.Fatalf("expected one manhwa filed, got %+v", rec.filed)
	}
	if len(rec.scanned) != 1 || rec.scanned[0] != 2 {
		t.Fatalf("expected scan of lib 2, got %v", rec.scanned)
	}
}

func TestRunOnceUnmatchedWhenUnknown(t *testing.T) {
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "???"}}},
		Classifier: fakeClassifier{t: model.TypeUnknown},
		Filer:      rec, Kavita: rec, Unmatched: rec,
		LibraryRoots: map[model.ContentType]string{},
		LibraryIDs:   map[model.ContentType]int{},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.unmatched) != 1 || len(rec.filed) != 0 {
		t.Fatalf("expected unmatched, got filed=%v unmatched=%v", rec.filed, rec.unmatched)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/poller/ -v`
Expected: FAIL (Poller undefined).

- [ ] **Step 3: Implement** — `internal/poller/poller.go`

```go
package poller

import "github.com/gavinmcfall/mangarr/internal/model"

type Scanner interface{ ScanAll() ([]model.Series, error) }
type Classifier interface{ Classify(title string) (model.ContentType, error) }
type Filer interface{ File(series model.Series, dstRoot string) error }
type Kavita interface{ Scan(libraryID int) error }
type UnmatchedSink interface{ MarkUnmatched(s model.Series) error }

type Poller struct {
	Scanner      Scanner
	Classifier   Classifier
	Filer        Filer
	Kavita       Kavita
	Unmatched    UnmatchedSink
	LibraryRoots map[model.ContentType]string
	LibraryIDs   map[model.ContentType]int
}

// RunOnce performs a single scan→classify→file→scan pass.
func (p *Poller) RunOnce() error {
	series, err := p.Scanner.ScanAll()
	if err != nil {
		return err
	}
	scanned := map[int]bool{}
	for _, s := range series {
		t, err := p.Classifier.Classify(s.Title)
		if err != nil || t == model.TypeUnknown {
			if err := p.Unmatched.MarkUnmatched(s); err != nil {
				return err
			}
			continue
		}
		s.Type = t
		root, ok := p.LibraryRoots[t]
		if !ok {
			if err := p.Unmatched.MarkUnmatched(s); err != nil {
				return err
			}
			continue
		}
		if err := p.Filer.File(s, root); err != nil {
			return err
		}
		if id, ok := p.LibraryIDs[t]; ok && !scanned[id] {
			if err := p.Kavita.Scan(id); err != nil {
				return err
			}
			scanned[id] = true
		}
	}
	return nil
}
```

> NOTE: the concrete `filer.Filer` and `kavita.Client` get thin adapter methods (`File(series, root)` / `Scan(id)`) in `main.go` wiring (Task 9) so they satisfy these interfaces. The poller depends only on the interfaces — keeps it unit-testable with fakes.

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/poller/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/
git commit -m "feat(poller): orchestrate scan/classify/file/scan with unmatched handling"
```

---

## Task 9: web package + main wiring

**Files:**
- Create: `internal/web/web.go`, `internal/web/web_test.go`, `internal/web/templates/*.html`, `internal/web/static/htmx.min.js`
- Modify: `main.go`

- [ ] **Step 1: Write failing test** — `internal/web/web_test.go`

```go
package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

type fakeData struct{}
func (fakeData) ListSeries() ([]model.Series, error) {
	return []model.Series{{Title: "Solo Leveling", Type: model.TypeManhwa, Status: model.StatusFiled}}, nil
}

func TestSeriesPageRenders(t *testing.T) {
	h := NewHandler(fakeData{})
	req := httptest.NewRequest(http.MethodGet, "/series", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if body := rr.Body.String(); !contains(body, "Solo Leveling") {
		t.Fatalf("series not rendered: %s", body)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (func() bool { for i := 0; i+len(sub) <= len(s); i++ { if s[i:i+len(sub)] == sub { return true } }; return false })() }
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/web/ -v`
Expected: FAIL (NewHandler undefined).

- [ ] **Step 3: Implement** — `internal/web/web.go` (+ minimal `templates/series.html`, vendored `static/htmx.min.js`)

`internal/web/templates/series.html`:
```html
<!doctype html><html><head><title>mangarr — Series</title>
<script src="/static/htmx.min.js"></script></head><body>
<h1>Series</h1>
<table><thead><tr><th>Title</th><th>Type</th><th>Status</th></tr></thead><tbody>
{{range .}}<tr><td>{{.Title}}</td><td>{{.Type}}</td><td>{{.Status}}</td></tr>{{end}}
</tbody></table></body></html>
```

`internal/web/web.go`:
```go
package web

import (
	"embed"
	"html/template"
	"net/http"

	"github.com/gavinmcfall/mangarr/internal/model"
)

//go:embed templates/*.html static/*
var assets embed.FS

type DataSource interface {
	ListSeries() ([]model.Series, error)
}

type Handler struct {
	mux  *http.ServeMux
	tmpl *template.Template
	data DataSource
}

func NewHandler(data DataSource) *Handler {
	h := &Handler{
		mux:  http.NewServeMux(),
		tmpl: template.Must(template.ParseFS(assets, "templates/*.html")),
		data: data,
	}
	h.mux.Handle("GET /static/", http.FileServerFS(assets))
	h.mux.HandleFunc("GET /series", h.series)
	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/series", http.StatusFound)
	})
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) series(w http.ResponseWriter, r *http.Request) {
	list, err := h.data.ListSeries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.tmpl.ExecuteTemplate(w, "series.html", list); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
```

Vendor HTMX:
```bash
mkdir -p internal/web/static
curl -fsSL https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js -o internal/web/static/htmx.min.js
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/web/ -v`
Expected: PASS.

- [ ] **Step 5: Wire main.go**

```go
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/gavinmcfall/mangarr/internal/classifier"
	"github.com/gavinmcfall/mangarr/internal/config"
	"github.com/gavinmcfall/mangarr/internal/store"
	"github.com/gavinmcfall/mangarr/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	_ = classifier.New("") // wired into poller in Task 10 follow-up

	h := web.NewHandler(st)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("mangarr listening on %s", cfg.HTTPAddr)
	log.Fatal(srv.ListenAndServe())
}
```

- [ ] **Step 6: Build + run smoke**

Run: `make build && MANGARR_DOWNLOAD_ROOTS=/tmp MANGARR_DB_PATH=/tmp/m.db ./bin/mangarr &` then `curl -s localhost:8590/series | grep -i series`
Expected: HTML with "Series" heading. Kill the process after.

- [ ] **Step 7: Commit**

```bash
git add internal/web/ main.go
git commit -m "feat(web): embedded HTMX UI + series page; wire main"
```

> **Follow-up tasks (same TDD pattern, deferred to keep this plan reviewable):** Unmatched page + manual-classify POST handler (writes `classification_cache`, re-files); Activity page; Settings page + POST (writes `settings`); poller scheduler goroutine started from `main` on `Settings.PollMinutes`; concrete `store`-backed `UnmatchedSink` + adapters making `filer.Filer`/`kavita.Client` satisfy the poller interfaces. Each is one component, one test file, same Red-Green-Commit rhythm.

---

## Task 10: Containerize

**Files:**
- Create: `Dockerfile`, `.dockerignore`

- [ ] **Step 1: Write the Dockerfile**

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mangarr .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mangarr /mangarr
EXPOSE 8590
USER nonroot:nonroot
ENTRYPOINT ["/mangarr"]
```

`.dockerignore`:
```
bin/
.git/
docs/
*.md
```

- [ ] **Step 2: Build the image locally**

Run: `docker build -t mangarr:dev .`
Expected: builds; final image is tiny (distroless static).

- [ ] **Step 3: Commit**

```bash
git add Dockerfile .dockerignore
git commit -m "build: multi-stage Dockerfile (CGO-free, distroless)"
```

---

## Task 11: CI — build + push to GHCR

**Files:**
- Create: `.github/workflows/build.yaml`

- [ ] **Step 1: Write the workflow**

```yaml
name: build
on:
  push:
    branches: [main]
    tags: ["v*"]
  pull_request:
permissions:
  contents: read
  packages: write
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.23" }
      - run: go test ./...
  image:
    needs: test
    if: github.event_name != 'pull_request'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/metadata-action@v5
        id: meta
        with:
          images: ghcr.io/gavinmcfall/mangarr
          tags: |
            type=ref,event=branch
            type=semver,pattern={{version}}
            type=sha
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
```

- [ ] **Step 2: Commit + push, watch CI**

```bash
git add .github/workflows/build.yaml
git commit -m "ci: test + build/push image to GHCR"
git push
```
Run: `gh run watch` — Expected: test + image jobs pass; image at `ghcr.io/gavinmcfall/mangarr`.

---

## Task 12: home-ops HelmRelease (deploy)

> Done in the `home-ops` repo, not this one. Separate small task, same patterns as the other entertainment apps.

- [ ] app-template HelmRelease in `kubernetes/apps/entertainment/mangarr/`: image `ghcr.io/gavinmcfall/mangarr` (digest-pinned), NFS mount `/media`, `ceph-block` config PVC via volsync-standard, internal route `mangarr.${SECRET_DOMAIN}`, env `MANGARR_DOWNLOAD_ROOTS=/media/Downloads/suwayomi,/media/Downloads/tranga`, ExternalSecret for `KAVITA_API_KEY`.
- [ ] Wire into `entertainment/kustomization.yaml`; validate with flux build + kubeconform; PR.

---

## Self-Review

- **Spec coverage:** poll trigger (Task 8 + follow-up scheduler) ✓ · ComicInfo+folder classify input (Task 4) ✓ · AniList country→type (Task 5) ✓ · unmatched queue (Task 8 + Task 9 follow-up UI) ✓ · hardlink/move/copy + rename (Task 6) ✓ · Kavita scan trigger (Task 7) ✓ · SQLite state (Task 3) ✓ · UI Series/Unmatched/Activity/Settings (Task 9 + follow-ups) ✓ · separate deployment + NFS + ExternalSecret (Task 12) ✓ · CGO-free static image (Task 10) ✓ · CI→GHCR (Task 11) ✓.
- **Gap flagged honestly:** Task 9 ships the Series page; Unmatched/Activity/Settings pages + the poller scheduler are enumerated as same-pattern follow-up tasks rather than padded inline, to keep the plan reviewable. They are not optional — they complete the MVP.
- **Type consistency:** `model.ContentType`, `model.Series`, `model.FileMode`, `Filer.File(series, srcDir, dstRoot)`, `Client.ScanLibrary(id)` / poller `Kavita.Scan(id)` adapter — names consistent across tasks.
- **Live-API caveat:** the Kavita scan endpoint/payload (Task 7) must be confirmed against the running Swagger; the test pins the assumed contract so a mismatch is caught immediately.

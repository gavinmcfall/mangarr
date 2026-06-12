package model

import "time"

// BulkJobStatus is the lifecycle state of one bulk-download job. The
// string values are persisted in SQLite and returned by the JSON API,
// so changing them is a schema change.
type BulkJobStatus string

const (
	BulkJobPending   BulkJobStatus = "pending"   // created, awaiting first orchestrator tick
	BulkJobRunning   BulkJobStatus = "running"   // orchestrator actively feeding chapters
	BulkJobPaused    BulkJobStatus = "paused"    // operator clicked pause; in-flight chapters complete naturally
	BulkJobCompleted BulkJobStatus = "completed" // all chapters reached state='done'
	BulkJobErrored   BulkJobStatus = "errored"   // consecutive_failures reached 5, or all chapters errored
)

// IsTerminal returns true for statuses the orchestrator no longer picks
// up. Pending/Running/Paused are active; Completed/Errored are terminal.
func (s BulkJobStatus) IsTerminal() bool {
	return s == BulkJobCompleted || s == BulkJobErrored
}

// BulkChapterState is the per-chapter lifecycle within a bulk job.
type BulkChapterState string

const (
	BulkChapterPending BulkChapterState = "pending" // not yet fed to Suwayomi
	BulkChapterFed     BulkChapterState = "fed"     // EnqueueChapterDownloads call made
	BulkChapterDone    BulkChapterState = "done"    // confirmed isDownloaded=true
	BulkChapterErrored BulkChapterState = "errored" // orchestrator gave up: max retries, empty chapter, or stall timeout
)

// BulkJob is one row in the bulk_jobs table.
type BulkJob struct {
	ID                  int64
	MangaID             int64  // Suwayomi numeric manga ID
	SourceID            string // Suwayomi numeric source ID (per-provider lock key)
	Title               string // snapshot at creation, for display
	SourceName          string // snapshot at creation, for display
	Status              BulkJobStatus
	TotalChapters       int
	CompletedChapters   int
	ErroredChapters     int
	LastError           string     // truncated 429/network/auth message, empty when no error
	BackoffUntil        *time.Time // nil means "no backoff active"; orchestrator skips if non-nil and in future
	ConsecutiveFailures int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// BulkJobChapter is one row in the bulk_job_chapters table.
type BulkJobChapter struct {
	JobID         int64
	ChapterID     int64 // Suwayomi numeric chapter ID
	State         BulkChapterState
	ErroredReason string // non-empty when State == BulkChapterErrored; persisted in errored_reason column
	Tries         int    // number of times mangarr has fed this chapter to Suwayomi; independent of Suwayomi's own tries counter
	UpdatedAt     time.Time
}

// LibraryCacheEntry is one row in the library_cache table — the per-manga
// chapter-count cache the Library page reads to render its "Missing"
// badges without a per-row Suwayomi roundtrip on every page load.
type LibraryCacheEntry struct {
	MangaID       int64
	Title         string
	SourceID      string
	SourceName    string
	TotalChapters int
	Downloaded    int
	// DudCount is the number of not-downloaded chapters Suwayomi reports with
	// pageCount==0 (permanent source duds). FiledCount is the number of .cbz
	// files present in the series' Kavita library dir. Both written at Sync.
	DudCount   int
	FiledCount int
	RefreshedAt time.Time
}

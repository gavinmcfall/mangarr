package model

import "time"

// ContentType is the pre-Library-Bindings-v2 type tag for a series.
//
// Vestigial after Plan A: the classifier no longer emits ContentType
// (it emits Decision{BindingID, Via}) and the poller routes by
// Binding, not by ContentType. ContentType is retained because:
//
//  1. Migration 2 (internal/store/migrations_v1.go) seeds v2 bindings
//     from the v1 LibraryRoots/KavitaLibIDsByType maps which are keyed
//     by ContentType.
//  2. Settings.LibraryRoots / Settings.KavitaLibIDsByType / Series.Type
//     are still on the wire so a rollback to the v1 classifier finds
//     its data.
//  3. The poller's FileOne (manual classify-from-Unmatched) still takes
//     a ContentType from the UI.
//
// Plan C is expected to drop the deprecated v1 Settings fields one
// release after Plan A lands, at which point ContentType can be removed
// entirely.
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
	ID           int64
	Title        string      // from ComicInfo.xml <Series>, else folder name
	SourcePath   string      // absolute path under a download root
	Source       string      // e.g. "suwayomi" / "tranga"
	Type         ContentType // resolved type (or TypeUnknown)
	Status       Status
	ChapterCount int
	UpdatedAt    time.Time
	// ManualBindingID is a user-set override added by the v2 Series-page
	// reclassify control. When non-nil, the classifier short-circuits its
	// six-step flow at step 0 and routes straight to this binding with
	// Via = "manual". nil means "no override; classify normally". Cleared
	// by sending a nil pointer through SetSeriesManualBinding.
	ManualBindingID *int64
	// CurrentBindingID is the binding the classifier most recently routed
	// this series to (the result of step 4/5 in the six-step flow, written
	// at filer-success time). Distinct from ManualBindingID: this is the
	// auto-classifier's verdict, recorded so /series can render the visible
	// pill without bouncing to the Activity log. nil when no successful
	// classification has happened yet (series is fresh, or only ever hit
	// Unmatched). Cleared automatically on next successful classification.
	CurrentBindingID *int64
}

type ActivityAction string

const (
	ActionFiled              ActivityAction = "filed"
	ActionUnmatched          ActivityAction = "unmatched"
	ActionScanTriggered      ActivityAction = "scan-triggered"
	ActionError              ActivityAction = "error"
	ActionBulkQueued         ActivityAction = "bulk-queued"
	ActionBulkDone           ActivityAction = "bulk-done"
	ActionBulkChapterErrored ActivityAction = "bulk-chapter-errored"
)

type ActivityEntry struct {
	ID          int64
	Time        time.Time
	SeriesTitle string
	Action      ActivityAction
	Detail      string
	// Via records which classification path produced this entry, e.g.
	// "suwayomi-override:category=42", "anilist:KR", "anilist:JP", or
	// "unmatched". Empty for entries written before the Library Map
	// feature shipped (Plan B); the activity log renderer treats empty
	// as "unknown / legacy".
	Via string
}

type FileMode string

const (
	ModeHardlink FileMode = "hardlink"
	ModeMove     FileMode = "move"
	ModeCopy     FileMode = "copy"
)

// SuwayomiAuthType selects which upstream auth flow the suwayomi.Client
// uses. Values map 1:1 onto Suwayomi's `server.authMode` setting.
type SuwayomiAuthType string

const (
	SuwayomiAuthNone   SuwayomiAuthType = "none"
	SuwayomiAuthBasic  SuwayomiAuthType = "basic"
	SuwayomiAuthSimple SuwayomiAuthType = "simple" // simple_login
	SuwayomiAuthUI     SuwayomiAuthType = "ui"     // ui_login
)

// Settings is the single mutable config row (id=1).
type Settings struct {
	DownloadRoots      []string              `json:"download_roots"` // managed via UI; env seeds on first boot
	FileMode           FileMode
	RenameScheme       string // e.g. "{series}/{series} - Ch.{chapter}.cbz"
	PollMinutes        int
	LibraryRoots       map[ContentType]string // Manga -> /media/Library/Books/Manga, ...
	KavitaBaseURL      string
	KavitaAPIKey       string                // API key for Kavita plugin authentication
	KavitaLibIDs       []int64               // legacy: flat list of library IDs (kept for compatibility)
	KavitaLibIDsByType map[ContentType]int64 // per-type library IDs (used by poller)

	// --- Suwayomi (Library Map) ---
	// User-editable, fresh-per-call. Empty BaseURL = feature disabled,
	// classifier short-circuits to the AniList path unchanged.
	SuwayomiBaseURL  string           `json:"suwayomi_base_url,omitempty"`
	SuwayomiAuthType SuwayomiAuthType `json:"suwayomi_auth_type,omitempty"`
	SuwayomiUsername string           `json:"suwayomi_username,omitempty"`
	SuwayomiPassword string           `json:"suwayomi_password,omitempty"`
	// SuwayomiCategoryOverrides maps a Suwayomi category ID to a Kavita
	// library ID. First-match-wins by Suwayomi `category.order` (the
	// PathCache returns CategoryIDs in that order). Empty/nil map =
	// feature disabled = pure AniList classification.
	SuwayomiCategoryOverrides map[int64]int64 `json:"suwayomi_category_overrides,omitempty"`

	// DefaultBindingID is the optional catch-all routing target when no
	// classification rule matches and no Suwayomi override applies. nil
	// means "send unmatched series to the Unmatched queue" (the safe
	// default and the pre-v2 behaviour). Set to a Binding.ID to auto-
	// route everything else.
	DefaultBindingID *int64 `json:"default_binding_id,omitempty"`

	// SuwayomiCategoryBindings is the v2 routing map: Suwayomi category ID
	// → Binding.ID. Populated by Migration 2 from the v1-era
	// SuwayomiCategoryOverrides (which held Kavita library IDs) via
	// reverse-lookup against the user's bindings. v1 SuwayomiCategoryOverrides
	// is left untouched on the settings row so a pre-v2 rollback can still
	// read it.
	SuwayomiCategoryBindings map[int64]int64 `json:"suwayomi_category_bindings,omitempty"`

	// BulkMaxInFlight is the per-provider cap on chapters concurrently in
	// flight via Suwayomi's queue. The bulk-downloader orchestrator never
	// feeds new chapters when the in-flight count exceeds this. Default 5.
	BulkMaxInFlight int `json:"bulk_max_in_flight"`
	// BulkRefillThreshold is the in-flight count at or below which the
	// orchestrator feeds the next batch. Default 2.
	BulkRefillThreshold int `json:"bulk_refill_threshold"`
	// BulkInterBatchDelaySec is a courtesy sleep (in seconds) the
	// orchestrator inserts between feeding batches, on top of Suwayomi's
	// own per-chapter delay. Default 1.
	BulkInterBatchDelaySec int `json:"bulk_inter_batch_delay_sec"`

	// BulkStallTimeoutMinutes is the wall-clock age (in minutes) after which
	// a chapter stuck in state 'fed' is considered stalled and re-queued by
	// the stalled-job detector. Default 30.
	BulkStallTimeoutMinutes int `json:"bulk_stall_timeout_minutes,omitempty"`
	// BulkChapterMaxRetries is the maximum number of times the orchestrator
	// will re-queue a stalled chapter before marking it errored. Default 3.
	BulkChapterMaxRetries int `json:"bulk_chapter_max_retries,omitempty"`
	// BulkAutoErrorEmptyChaptersDisabled inverts the default-enabled
	// "auto-error chapters with zero pages" behaviour. The zero value (false)
	// means auto-error IS enabled; set to true to disable it.
	// Read sites should derive the effective flag as:
	//   autoError := !set.BulkAutoErrorEmptyChaptersDisabled
	BulkAutoErrorEmptyChaptersDisabled bool `json:"bulk_auto_error_empty_chapters_disabled,omitempty"`
}

// Binding is one library destination the user has defined. Replaces the
// closed-enum routing of v1 (Library Map). Each binding owns a filesystem
// root and a Kavita library ID for the scan trigger.
type Binding struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	LibraryRoot    string `json:"library_root"`
	KavitaLibID    int64  `json:"kavita_lib_id"`
	DefaultIsAdult bool   `json:"default_is_adult"`
}

// ClassificationRule maps a metadata condition to a binding. Rules are
// stored as an ordered list; the classifier walks them ascending by
// Priority and routes the series to the first matching rule's binding.
type ClassificationRule struct {
	ID        int64         `json:"id"`
	Priority  int           `json:"priority"`
	Name      string        `json:"name"`
	Condition RuleCondition `json:"condition"`
	BindingID int64         `json:"binding_id"`
}

// RuleCondition is AND-semantics across set fields. Pointer types let nil
// mean "wildcard" (don't constrain) while explicit zero values (e.g. an
// explicit IsAdult=false) constrain to that value.
type RuleCondition struct {
	CountryOfOrigin  *string `json:"country_of_origin,omitempty"`
	IsAdult          *bool   `json:"is_adult,omitempty"`
	Format           *string `json:"format,omitempty"`
	SourcePathPrefix *string `json:"source_path_prefix,omitempty"`
}

// IsPathOnly reports whether this condition only constrains the source
// path. Path-only rules are evaluated in the classifier's step 1 short-
// circuit, before any AniList call.
func (c RuleCondition) IsPathOnly() bool {
	return c.SourcePathPrefix != nil && c.CountryOfOrigin == nil && c.IsAdult == nil && c.Format == nil
}

// Decision is the classifier's output: which binding to route to, plus
// the Via tag that gets recorded on the activity log entry so users can
// audit how each series was classified.
type Decision struct {
	BindingID int64
	Via       string
}

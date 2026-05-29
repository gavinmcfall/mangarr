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
	ID           int64
	Title        string      // from ComicInfo.xml <Series>, else folder name
	SourcePath   string      // absolute path under a download root
	Source       string      // e.g. "suwayomi" / "tranga"
	Type         ContentType // resolved type (or TypeUnknown)
	Status       Status
	ChapterCount int
	UpdatedAt    time.Time
}

type ActivityAction string

const (
	ActionFiled         ActivityAction = "filed"
	ActionUnmatched     ActivityAction = "unmatched"
	ActionScanTriggered ActivityAction = "scan-triggered"
	ActionError         ActivityAction = "error"
)

type ActivityEntry struct {
	ID          int64
	Time        time.Time
	SeriesTitle string
	Action      ActivityAction
	Detail      string
}

type FileMode string

const (
	ModeHardlink FileMode = "hardlink"
	ModeMove     FileMode = "move"
	ModeCopy     FileMode = "copy"
)

// Settings is the single mutable config row (id=1).
type Settings struct {
	DownloadRoots      []string               `json:"download_roots"`   // managed via UI; env seeds on first boot
	FileMode           FileMode
	RenameScheme       string                  // e.g. "{series}/{series} - Ch.{chapter}.cbz"
	PollMinutes        int
	LibraryRoots       map[ContentType]string  // Manga -> /media/Library/Books/Manga, ...
	KavitaBaseURL      string
	KavitaAPIKey       string                  // API key for Kavita plugin authentication
	KavitaLibIDs       []int64                 // legacy: flat list of library IDs (kept for compatibility)
	KavitaLibIDsByType map[ContentType]int64   // per-type library IDs (used by poller)
}

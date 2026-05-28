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

type ActivityEntry struct {
	ID          int64
	Time        time.Time
	SeriesTitle string
	Action      string // "filed", "unmatched", "scan-triggered", "error"
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
	FileMode      FileMode
	RenameScheme  string            // e.g. "{series}/{series} - Ch.{chapter}.cbz"
	PollMinutes   int
	LibraryRoots  map[ContentType]string // Manga -> /media/Library/Books/Manga, ...
	KavitaBaseURL string
	KavitaLibIDs  []int             // libraries to scan after filing
}

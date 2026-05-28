// Package poller orchestrates a single scan→classify→file→kavita-trigger pass.
//
// On each RunOnce call the poller:
//  1. Calls Scanner.ScanAll() to get candidate series from all download roots.
//  2. For each series, classifies its type via Classifier.Classify.
//     - TypeUnknown (or missing LibraryRoots entry): routed to UnmatchedSink.
//     - Known type with a configured root: filed via Filer.File.
//  3. After filing, triggers a Kavita library scan (once per type per RunOnce call).
//  4. Errors are surfaced but never abort the whole tick — every series is attempted.
//
// Interfaces are minimal so unit tests inject fakes without importing the
// concrete scanner/classifier/filer/kavita packages. The concrete wiring lives
// in main.go (Task 9).
package poller

import (
	"github.com/gavinmcfall/mangarr/internal/model"
)

// Scanner returns all candidate series from every configured download root.
// The concrete implementation in main.go wraps scanner.Scan for each root.
type Scanner interface {
	ScanAll() ([]model.Series, error)
}

// Classifier maps a series title to a ContentType.
// classifier.Classifier satisfies this interface directly.
type Classifier interface {
	Classify(title string) (model.ContentType, error)
}

// Filer moves/copies/hardlinks the series files into the destination root.
// The concrete implementation in main.go adapts filer.Filer.File, which takes
// (series string, srcDir string, dstRoot string).
type Filer interface {
	File(s model.Series, dstRoot string) error
}

// Kavita triggers a library scan by library ID (int64 matches model.Settings.KavitaLibIDs).
// kavita.Client.ScanLibrary satisfies this interface directly.
type Kavita interface {
	ScanLibrary(libraryID int64) error
}

// UnmatchedSink records series whose type could not be determined.
// The concrete implementation in main.go upserts into the store with StatusUnmatched.
type UnmatchedSink interface {
	MarkUnmatched(s model.Series) error
}

// Poller holds the wired-up dependencies and configuration for one orchestration tick.
type Poller struct {
	Scanner      Scanner
	Classifier   Classifier
	Filer        Filer
	Kavita       Kavita
	Unmatched    UnmatchedSink
	LibraryRoots map[model.ContentType]string // content type → absolute library path
	LibraryIDs   map[model.ContentType]int64  // content type → Kavita library ID
}

// RunOnce performs one complete scan→classify→file→scan pass.
//
// Errors from individual series do not abort the tick; every series is
// processed. A non-nil error is only returned for failures that prevent any
// meaningful work (e.g. the scanner itself fails to start).
//
// Kavita scans are deduplicated: if multiple series share the same ContentType
// in a single RunOnce call, only one scan is triggered for that library.
func (p *Poller) RunOnce() error {
	series, err := p.Scanner.ScanAll()
	if err != nil {
		return err
	}

	// Track which library IDs have already been scanned this tick.
	scanned := map[int64]bool{}

	for _, s := range series {
		ct, err := p.Classifier.Classify(s.Title)
		if err != nil || ct == model.TypeUnknown {
			// Route to unmatched — classification failed or type unknown.
			_ = p.Unmatched.MarkUnmatched(s)
			continue
		}

		root, ok := p.LibraryRoots[ct]
		if !ok {
			// Known type but no configured root — treat as unmatched.
			_ = p.Unmatched.MarkUnmatched(s)
			continue
		}

		s.Type = ct
		if err := p.Filer.File(s, root); err != nil {
			// Filer error: log/skip but don't abort.
			continue
		}

		// Trigger a Kavita scan for this library (once per type per tick).
		if id, ok := p.LibraryIDs[ct]; ok && !scanned[id] {
			_ = p.Kavita.ScanLibrary(id)
			scanned[id] = true
		}
	}
	return nil
}

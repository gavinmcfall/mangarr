package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// bulkPreview is the per-manga preview row returned by apiBulkCreate
// when confirm=0. The JSON shape is preserved from Plan A T13 (scripted
// callers without HX-Request still receive it verbatim); promoting it
// from an inline struct to file scope lets renderBulkConfirmModal
// aggregate it for the HTMX confirmation modal (Plan B T3).
type bulkPreview struct {
	MangaID    int64  `json:"manga_id"`
	Title      string `json:"title"`
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
	Missing    int    `json:"missing"`
}

// apiBulkJobs handles GET /api/bulk/jobs. Returns a JSON array of all
// bulk jobs, optionally filtered by ?status=<running|paused|...>.
//
// Empty/missing status returns every row (across all states); an unknown
// status string falls through to the store, which returns an empty list
// — matching the contract that "no jobs match" is a 200 with `[]`, not
// a 4xx. Errors from the store surface as 500.
func (h *Handler) apiBulkJobs(w http.ResponseWriter, r *http.Request) {
	status := model.BulkJobStatus(r.URL.Query().Get("status"))
	jobs, err := h.store.ListBulkJobs(status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jobs = []model.BulkJob{} // marshal as `[]`, not `null`
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(jobs)
}

// apiLibrarySync handles POST /api/library/sync. Fetches every manga the
// operator has in Suwayomi's library via the existing
// ListLibraryWithCategories GraphQL query and upserts one row per manga
// into library_cache.
//
// Chapter counts (total / downloaded) are left at 0 here — the Library
// page's per-row HTMX fragment endpoint (Plan B T2) fills them in lazily
// so the sync call returns quickly even for libraries of 100+ series.
//
// Closes the Plan A→B gap flagged in the design review: library_cache
// existed in the schema but was never written, so POST /api/bulk 400'd on
// every request with "manga_id not in library cache".
//
// Returns:
//   - 503 when Suwayomi isn't configured
//   - 502 on a Suwayomi error
//   - 500 on a store write error
//   - 200 with {"synced":N} JSON on success
func (h *Handler) apiLibrarySync(w http.ResponseWriter, r *http.Request) {
	if h.suwayomi == nil {
		http.Error(w, "suwayomi client not configured", http.StatusServiceUnavailable)
		return
	}
	entries, err := h.suwayomi.ListLibraryWithCategories(r.Context())
	if err != nil {
		http.Error(w, "suwayomi library fetch: "+err.Error(), http.StatusBadGateway)
		return
	}
	for _, e := range entries {
		if err := h.store.SaveLibraryCacheEntry(model.LibraryCacheEntry{
			MangaID:    e.ID,
			Title:      e.Title,
			SourceID:   e.SourceID,
			SourceName: e.Source,
			// TotalChapters and Downloaded stay 0; T2's fragment endpoint
			// fills them in on first Library page render.
		}); err != nil {
			http.Error(w, "library_cache write: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = fmt.Fprintf(w, `{"synced":%d}`, len(entries))
}

// apiLibraryRowMissing handles GET /api/library/{mangaId}/missing.
// Returns an HTMX-swappable 3-cell <td> fragment with Total / Downloaded /
// Missing counts. Triggered lazily per-row from the Library page so the
// page paints fast and the per-manga Suwayomi roundtrip is amortised
// across N parallel HTMX requests.
//
// On a successful render the cache is best-effort refreshed with the
// new counts so subsequent reads are warm. A cache-write error is
// swallowed: the fragment still rendered correctly, and the next
// request will simply re-roundtrip.
//
// Returns:
//   - 400 on an unparseable mangaId path segment
//   - 404 when the manga isn't in library_cache (HTMX swaps a visible
//     "not in cache" indicator rather than blank cells)
//   - 503 when Suwayomi isn't configured
//   - 502 on a Suwayomi error
//   - 200 with the rendered fragment on success
func (h *Handler) apiLibraryRowMissing(w http.ResponseWriter, r *http.Request) {
	mIDStr := r.PathValue("mangaId")
	mID, err := strconv.ParseInt(mIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid mangaId", http.StatusBadRequest)
		return
	}
	entry, err := h.store.GetLibraryCacheEntry(mID)
	if err != nil {
		http.Error(w, "not in library cache", http.StatusNotFound)
		return
	}
	if h.suwayomi == nil {
		http.Error(w, "suwayomi client not configured", http.StatusServiceUnavailable)
		return
	}
	chapters, err := h.suwayomi.ListChapters(r.Context(), mID)
	if err != nil {
		http.Error(w, "suwayomi list chapters: "+err.Error(), http.StatusBadGateway)
		return
	}
	total := len(chapters)
	downloaded := 0
	for _, c := range chapters {
		if c.IsDownloaded {
			downloaded++
		}
	}
	missing := total - downloaded

	// Best-effort cache refresh — preserve Title/SourceID/SourceName from
	// the existing entry, overwrite only the count fields. A write error
	// is swallowed: the fragment rendered fine, the next request will
	// simply re-roundtrip and try again.
	entry.TotalChapters = total
	entry.Downloaded = downloaded
	_ = h.store.SaveLibraryCacheEntry(entry)

	data := struct {
		Total      int
		Downloaded int
		Missing    int
	}{total, downloaded, missing}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderTemplate(w, "library-row-count", "library-row-count", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// apiBulkCreate handles POST /api/bulk. Creates one BulkJob per manga_id
// from the multi-select form, populating chapter rows from Suwayomi's
// ListChapters filtered to isDownloaded=false. Series with zero missing
// chapters are silently skipped (no empty job created). On confirm=1 it
// redirects to /downloads; on confirm=0 it returns a JSON preview the
// caller can render in a confirmation modal (the modal HTML lands in
// Plan B).
//
// Returns:
//   - 503 if no SuwayomiClient is wired (test setups that don't need bulk routes)
//   - 400 on bad form, missing manga_id, unparseable manga_id, or unknown manga_id (no library cache entry)
//   - 500 on Suwayomi or store errors
func (h *Handler) apiBulkCreate(w http.ResponseWriter, r *http.Request) {
	if h.suwayomi == nil {
		http.Error(w, "suwayomi client not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	mangaIDStrs := r.Form["manga_id"]
	if len(mangaIDStrs) == 0 {
		http.Error(w, "no manga_id provided", http.StatusBadRequest)
		return
	}
	confirm := r.FormValue("confirm") == "1"

	var previews []bulkPreview

	for _, mIDStr := range mangaIDStrs {
		mID, err := strconv.ParseInt(mIDStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid manga_id: "+mIDStr, http.StatusBadRequest)
			return
		}
		entry, err := h.store.GetLibraryCacheEntry(mID)
		if err != nil {
			http.Error(w, "manga_id not in library cache: "+mIDStr, http.StatusBadRequest)
			return
		}
		chapters, err := h.suwayomi.ListChapters(r.Context(), mID)
		if err != nil {
			http.Error(w, "list chapters: "+err.Error(), http.StatusInternalServerError)
			return
		}
		var missingIDs []int64
		for _, c := range chapters {
			if !c.IsDownloaded {
				missingIDs = append(missingIDs, c.ID)
			}
		}
		previews = append(previews, bulkPreview{
			MangaID:    mID,
			Title:      entry.Title,
			SourceID:   entry.SourceID,
			SourceName: entry.SourceName,
			Missing:    len(missingIDs),
		})
		// Skip series with zero missing chapters: creating an empty job
		// would immediately satisfy the "all chapters done" terminal
		// condition and leave the operator wondering why a "Completed"
		// row appeared with TotalChapters=0.
		if confirm && len(missingIDs) > 0 {
			jobID, err := h.store.SaveBulkJob(model.BulkJob{
				MangaID:       mID,
				SourceID:      entry.SourceID,
				Title:         entry.Title,
				SourceName:    entry.SourceName,
				Status:        model.BulkJobRunning,
				TotalChapters: len(missingIDs),
			})
			if err != nil {
				http.Error(w, "save bulk job: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if err := h.store.BatchInsertBulkJobChapters(jobID, missingIDs); err != nil {
				http.Error(w, "insert chapters: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	if confirm {
		http.Redirect(w, r, "/downloads", http.StatusSeeOther)
		return
	}
	// Plan B T3: HTMX-driven submits from /library's form get the
	// confirmation modal as HTML. Scripted (non-HTMX) callers still get
	// the JSON preview from Plan A T13 — the HX-Request header is the
	// branch point and is added automatically by HTMX.
	if r.Header.Get("HX-Request") == "true" {
		h.renderBulkConfirmModal(w, previews)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(previews)
}

// renderBulkConfirmModal collapses the preview slice into the shape
// expected by bulk-confirm.html: per-provider aggregation (series +
// chapter counts grouped by SourceName, falling back to SourceID when
// the displayName is empty), running totals, and an .Empty flag so the
// template can branch to empty-state copy when every selected series
// is already fully downloaded.
//
// Providers are sorted by Name for stable rendering — pin-style tests
// rely on deterministic order, and the operator-facing list reads
// better alphabetised than in arbitrary map-iteration order.
func (h *Handler) renderBulkConfirmModal(w http.ResponseWriter, previews []bulkPreview) {
	type providerRow struct {
		Name         string
		SeriesCount  int
		ChapterCount int
	}
	byProvider := map[string]*providerRow{}
	totalChapters := 0
	seriesCount := 0
	mangaIDs := make([]int64, 0, len(previews))
	for _, p := range previews {
		mangaIDs = append(mangaIDs, p.MangaID)
		if p.Missing == 0 {
			// Don't include zero-missing series in the per-provider
			// breakdown; if every selection is zero-missing the .Empty
			// branch below renders the "fully downloaded" copy.
			continue
		}
		seriesCount++
		totalChapters += p.Missing
		key := p.SourceName
		if key == "" {
			key = p.SourceID
		}
		if pr, ok := byProvider[key]; ok {
			pr.SeriesCount++
			pr.ChapterCount += p.Missing
		} else {
			byProvider[key] = &providerRow{Name: key, SeriesCount: 1, ChapterCount: p.Missing}
		}
	}
	providers := make([]providerRow, 0, len(byProvider))
	for _, pr := range byProvider {
		providers = append(providers, *pr)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })

	data := struct {
		Empty         bool
		TotalChapters int
		SeriesCount   int
		ProviderCount int
		Providers     []providerRow
		MangaIDs      []int64
	}{
		Empty:         seriesCount == 0,
		TotalChapters: totalChapters,
		SeriesCount:   seriesCount,
		ProviderCount: len(providers),
		Providers:     providers,
		MangaIDs:      mangaIDs,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderTemplate(w, "bulk-confirm", "bulk-confirm", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// apiDownloadsAction handles POST /api/downloads/{id}/{action} where
// action is one of pause, resume, delete.
//
// Resume on an errored job ALSO clears backoff state (consecutive_failures
// + backoff_until) before flipping status back to running, so the next
// orchestrator tick is unencumbered by the prior failure ladder.
//
// Delete cascades to bulk_job_chapters via the FK ON DELETE CASCADE clause
// from Migration 4 (store.Open enables PRAGMA foreign_keys=ON so it fires).
//
// Returns:
//   - 200 on success (no body — clients reload the list via GET /api/bulk/jobs)
//   - 400 on unparseable {id} or unknown {action}
//   - 500 on store errors
func (h *Handler) apiDownloadsAction(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	action := r.PathValue("action")
	switch action {
	case "pause":
		if err := h.store.UpdateBulkJobStatus(id, model.BulkJobPaused); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "resume":
		// On resume from errored, also clear backoff state so the next
		// tick is unencumbered.
		if err := h.store.ClearBulkJobBackoff(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := h.store.UpdateBulkJobStatus(id, model.BulkJobRunning); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "delete":
		if err := h.store.DeleteBulkJob(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "unknown action: "+action, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

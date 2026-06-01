package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gavinmcfall/mangarr/internal/model"
)

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

	type preview struct {
		MangaID    int64  `json:"manga_id"`
		Title      string `json:"title"`
		SourceID   string `json:"source_id"`
		SourceName string `json:"source_name"`
		Missing    int    `json:"missing"`
	}
	var previews []preview

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
		previews = append(previews, preview{
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(previews)
}

package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

// binSeriesFiles sends every regular file under each dir to the recycle bin,
// then removes the now-empty dirs. recyclebin.Send rejects directories, so we
// recurse and send files individually. A dir that does not exist is a no-op
// (common: the source was already deleted upstream).
func binSeriesFiles(bin *recyclebin.Bin, dirs []string, now time.Time) error {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			_, sendErr := bin.Send(path, now)
			return sendErr
		})
		if walkErr != nil {
			return walkErr
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// apiSeriesDelete handles POST /api/series/{id}/delete.
// Form: delete_files=true|false. With delete_files=true, the series' source
// dir (and Kavita dest dir when resolvable) are sent to the recycle bin before
// the row is removed. Always durable: the DB row is deleted so the next scan
// cannot resurrect it.
func (h *Handler) apiSeriesDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	series, err := h.store.GetSeriesByID(id)
	if err != nil {
		http.Redirect(w, r, "/series", http.StatusSeeOther) // already gone
		return
	}
	if r.FormValue("delete_files") == "true" {
		if h.recycleBin == nil {
			http.Error(w, "recycle bin not configured", http.StatusServiceUnavailable)
			return
		}
		dirs := []string{series.SourcePath}
		if dst := h.resolveSeriesDestDir(r.Context(), id); dst != "" {
			dirs = append(dirs, dst)
		}
		if err := binSeriesFiles(h.recycleBin, dirs, time.Now()); err != nil {
			http.Error(w, "bin files: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := h.store.DeleteSeries(id); err != nil {
		http.Error(w, "delete series: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

// apiSeriesRestore handles POST /api/series/{id}/restore — clears the orphaned
// flag (back to pending) and the missing_since timer.
func (h *Handler) apiSeriesRestore(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	_ = h.store.SetSeriesMissingSince(id, nil)
	_ = h.store.SetSeriesStatus(id, model.StatusPending)
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

// resolveSeriesDestDir returns the Kavita library directory for a series by
// taking the parent dir of the first chapter plan from the previewer, or ""
// if the series cannot be planned (unmatched/misconfigured/no previewer).
func (h *Handler) resolveSeriesDestDir(ctx context.Context, id int64) string {
	if h.previewer == nil {
		return ""
	}
	pe, err := h.previewer.PreviewOne(ctx, id)
	if err != nil || len(pe.ChapterPlans) == 0 {
		return ""
	}
	return filepath.Dir(pe.ChapterPlans[0].DstPath)
}

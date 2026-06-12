package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

// binSeriesFiles sends every regular file under each dir to the recycle bin,
// then removes the now-empty dirs. recyclebin.Send rejects directories, so we
// recurse and send files individually. A dir that does not exist is a no-op
// (common: the source was already deleted upstream).
//
// Partial-failure contract: if a bin.Send fails mid-walk, the files already
// binned stay binned and the dir is NOT removed — the conservative outcome,
// since the operator can recover a half-binned dir but not a wrongly-removed
// one. Symlinked subdirs are visited as non-dir entries, so Send errors out on
// them rather than following the link — nothing outside the tree is moved.
func binSeriesFiles(bin *recyclebin.Bin, dirs []string, now time.Time) error {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		// Catastrophe backstop: refuse to act on a shallow path so a corrupt DB
		// row or a scanner bug that sets SourcePath to a root can't nuke a huge
		// subtree. Defense-in-depth only — both dirs are trustworthy by
		// construction (scanner-derived source under configured roots;
		// plan-derived dest) — so a simple segment-count check suffices.
		clean := filepath.Clean(dir)
		if clean == "/" || clean == "." || len(strings.Split(strings.Trim(clean, string(filepath.Separator)), string(filepath.Separator))) < 3 {
			return fmt.Errorf("binSeriesFiles: refusing to act on shallow path %q", dir)
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
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	series, err := h.store.GetSeriesByID(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Redirect(w, r, "/series", http.StatusSeeOther) // already gone
		return
	}
	if err != nil {
		http.Error(w, "get series: "+err.Error(), http.StatusInternalServerError)
		return
	}
	deletedFiles := r.FormValue("delete_files") == "true"
	if deletedFiles {
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
	if deletedFiles {
		setFlash(w, "success", "Removed series + files")
	} else {
		setFlash(w, "success", "Removed series")
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

// apiSeriesRestore handles POST /api/series/{id}/restore — clears the orphaned
// flag (back to pending) and the missing_since timer.
func (h *Handler) apiSeriesRestore(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.store.SetSeriesMissingSince(id, nil); err != nil {
		http.Error(w, "restore (clear missing_since): "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.store.SetSeriesStatus(id, model.StatusPending); err != nil {
		http.Error(w, "restore (set status): "+err.Error(), http.StatusInternalServerError)
		return
	}
	setFlash(w, "success", "Restored series")
	http.Redirect(w, r, "/series/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// resolveSeriesDestDir returns the Kavita library directory for a series by
// asking the previewer to resolve it from the persisted binding — no source
// folder read, so it works for orphaned series whose source has vanished.
// Returns "" when the previewer is not wired or the series has no binding.
func (h *Handler) resolveSeriesDestDir(ctx context.Context, id int64) string {
	if h.previewer == nil {
		return ""
	}
	dir, err := h.previewer.ResolveLibraryDir(ctx, id)
	if err != nil {
		return ""
	}
	return dir
}

package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/model"
)

// chapterFileView is one row on the series detail page.
type chapterFileView struct {
	SrcPath   string
	DstPath   string
	Mode      model.FileMode
	Status    string // "filed" (dst exists), "missing" (planned, not on disk), "error"
	Reason    string // populated when the plan entry was a PlanError
	SizeBytes int64
	ModTime   string // formatted; "" when the file doesn't exist
	FileName  string // base name of DstPath — used as the remove-to-bin handle
}

// chapterFiles enriches filer plan entries with on-disk size/mtime + a status
// the detail page renders. A dst that exists = "filed"; a plannable dst that
// isn't on disk = "missing"; a PlanError = "error".
func chapterFiles(plans []filer.PlanEntry) []chapterFileView {
	out := make([]chapterFileView, 0, len(plans))
	for _, p := range plans {
		v := chapterFileView{SrcPath: p.SrcPath, DstPath: p.DstPath, Mode: p.Mode, FileName: baseName(p.DstPath)}
		switch p.Action {
		case filer.PlanError:
			v.Status = "error"
			v.Reason = p.Error
		default:
			if fi, err := os.Stat(p.DstPath); err == nil {
				v.Status = "filed"
				v.SizeBytes = fi.Size()
				v.ModTime = fi.ModTime().Format("2006-01-02 15:04")
			} else {
				v.Status = "missing"
			}
		}
		out = append(out, v)
	}
	return out
}

// baseName returns the final path element of p (the file name), using the
// forward-slash separator the filer always emits in DstPath.
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// humanBytes renders a byte count as a short human-readable size string.
// 0 bytes renders as "—" so the detail table reads cleanly for not-yet-filed
// rows (which carry SizeBytes == 0).
func humanBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + []string{"KB", "MB", "GB", "TB", "PB"}[exp]
}

// seriesDetailData is the view-model for the per-series detail page.
type seriesDetailData struct {
	Page        string
	SeriesID    int64
	Title       string
	BindingName string
	DstRoot     string
	Status      string
	Reason      string
	Note        string
	Files       []chapterFileView
	Bindings    []model.Binding
}

// pageSeriesDetail renders GET /series/{id} — the per-chapter file list and
// per-series actions for one series, built off the poller's single-series
// preview pipeline.
func (h *Handler) pageSeriesDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if h.previewer == nil {
		http.Error(w, "preview pipeline not configured", http.StatusServiceUnavailable)
		return
	}
	entry, err := h.previewer.PreviewOne(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bindings, _ := h.store.ListBindings()

	h.render(w, "series-detail.html", seriesDetailData{
		Page:        "series",
		SeriesID:    id,
		Title:       entry.Title,
		BindingName: entry.BindingName,
		DstRoot:     entry.DstRoot,
		Status:      entry.Status,
		Reason:      entry.Reason,
		Note:        entry.Note,
		Files:       chapterFiles(entry.ChapterPlans),
		Bindings:    bindings,
	})
}

// apiSeriesRefile handles POST /api/series/{id}/refile — re-runs the filer for
// one series via its current classification, then redirects back to the detail
// page.
func (h *Handler) apiSeriesRefile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if h.refiler == nil {
		http.Error(w, "refiler not configured", http.StatusServiceUnavailable)
		return
	}
	if err := h.refiler.RefileOne(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/series/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// apiSeriesChapterRemove handles POST /api/series/{id}/chapter/remove — moves
// one already-filed chapter file into the recycle bin.
//
// The chapter is addressed by FILENAME (form value "name"), never by a
// caller-supplied path: the handler resolves the filename against the series'
// own preview plans and acts on the plan's exact, filer-computed DstPath. A
// name that matches no plan is rejected (400), and a defence-in-depth check
// confirms the resolved path is still under the series' destination root before
// anything is moved.
func (h *Handler) apiSeriesChapterRemove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if h.previewer == nil {
		http.Error(w, "preview pipeline not configured", http.StatusServiceUnavailable)
		return
	}
	if h.recycleBin == nil {
		http.Error(w, "recycle bin not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "missing chapter name", http.StatusBadRequest)
		return
	}

	entry, err := h.previewer.PreviewOne(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Resolve the filename against the series' own plans — use the plan's
	// exact DstPath rather than re-deriving a path from user input.
	var target string
	for _, p := range entry.ChapterPlans {
		if baseName(p.DstPath) == name {
			target = p.DstPath
			break
		}
	}
	if target == "" {
		http.Error(w, "chapter not found for this series", http.StatusBadRequest)
		return
	}

	// Defence in depth: the resolved path must sit under the destination root.
	if !pathUnder(entry.DstRoot, target) {
		http.Error(w, "resolved path escapes destination root", http.StatusBadRequest)
		return
	}

	if _, err := h.recycleBin.Send(target, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/series/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// pathUnder reports whether target is the root itself or lies within it, after
// cleaning both paths. An empty root fails closed (returns false).
func pathUnder(root, target string) bool {
	if root == "" {
		return false
	}
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	if cleanTarget == cleanRoot {
		return true
	}
	return strings.HasPrefix(cleanTarget, cleanRoot+string(filepath.Separator))
}

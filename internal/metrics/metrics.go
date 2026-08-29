// Package metrics provides a namespaced Prometheus registry for mangarr.
//
// All metric names use the "mangarr_" prefix.
// The Registry is safe for concurrent use.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the namespaced Prometheus registry for mangarr.
// All metrics created here use prefix "mangarr_".
type Registry struct {
	reg *prometheus.Registry

	// Counters
	filesFiled         *prometheus.CounterVec
	kavitaScans        *prometheus.CounterVec
	anilistLookups     *prometheus.CounterVec
	unmatchedTotal     prometheus.Counter
	fileErrorsTotal    prometheus.Counter
	fileConflictsTotal prometheus.Counter

	// Gauges
	pollerLastRun        prometheus.Gauge
	seriesCount          *prometheus.GaugeVec
	unmatchedSeries      prometheus.Gauge
	diskFreeBytes        *prometheus.GaugeVec
	diskTotalBytes       *prometheus.GaugeVec
	healthStatus         *prometheus.GaugeVec
	backupsCount         prometheus.Gauge
	backupLastModSeconds prometheus.Gauge
}

// NewRegistry creates and returns a new Registry with all metrics registered.
// It includes the standard Go runtime and process collectors.
func NewRegistry() *Registry {
	reg := prometheus.NewRegistry()

	// Standard collectors.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	r := &Registry{reg: reg}

	// ---- counters ----

	r.filesFiled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mangarr_files_filed_total",
		Help: "Total number of files filed by content category.",
	}, []string{"category"})
	reg.MustRegister(r.filesFiled)

	r.kavitaScans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mangarr_kavita_scan_total",
		Help: "Total Kavita library scan attempts by result.",
	}, []string{"result"})
	reg.MustRegister(r.kavitaScans)

	r.anilistLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mangarr_anilist_lookups_total",
		Help: "Total AniList lookup attempts by result.",
	}, []string{"result"})
	reg.MustRegister(r.anilistLookups)

	r.unmatchedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mangarr_unmatched_total",
		Help: "Total number of series routed to unmatched (events, not current count).",
	})
	reg.MustRegister(r.unmatchedTotal)

	r.fileErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mangarr_file_errors_total",
		Help: "Total number of file errors encountered during filing.",
	})
	reg.MustRegister(r.fileErrorsTotal)

	r.fileConflictsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mangarr_file_conflicts_total",
		Help: "Total source files skipped because their destination belongs to a different file (see filer.ConflictError).",
	})
	reg.MustRegister(r.fileConflictsTotal)

	// ---- gauges ----

	r.pollerLastRun = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mangarr_poller_last_run_timestamp_seconds",
		Help: "Unix timestamp (seconds) of the last completed poller RunOnce.",
	})
	reg.MustRegister(r.pollerLastRun)

	r.seriesCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mangarr_series_count",
		Help: "Current number of series in the store by content category.",
	}, []string{"category"})
	reg.MustRegister(r.seriesCount)

	r.unmatchedSeries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mangarr_unmatched_series",
		Help: "Current number of unmatched series in the store.",
	})
	reg.MustRegister(r.unmatchedSeries)

	r.diskFreeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mangarr_disk_free_bytes",
		Help: "Free disk bytes for each configured root path.",
	}, []string{"root"})
	reg.MustRegister(r.diskFreeBytes)

	r.diskTotalBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mangarr_disk_total_bytes",
		Help: "Total disk bytes for each configured root path.",
	}, []string{"root"})
	reg.MustRegister(r.diskTotalBytes)

	r.healthStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mangarr_health_status",
		Help: "Health check status by ID: 0=ok, 1=warn, 2=error.",
	}, []string{"id"})
	reg.MustRegister(r.healthStatus)

	r.backupsCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mangarr_backups_count",
		Help: "Current number of database backup files.",
	})
	reg.MustRegister(r.backupsCount)

	r.backupLastModSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mangarr_backup_last_modtime_seconds",
		Help: "Unix timestamp (seconds) of the most recent backup file.",
	})
	reg.MustRegister(r.backupLastModSeconds)

	return r
}

// Handler returns an http.Handler that serves the Prometheus metrics page.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// ---- counter methods ----

// IncFilesFiled increments the files-filed counter for the given category.
// category should be one of "manga", "manhwa", "manhua", or "unknown".
func (r *Registry) IncFilesFiled(category string) {
	r.filesFiled.WithLabelValues(category).Inc()
}

// IncKavitaScan increments the Kavita scan counter.
// result should be "success" or "error".
func (r *Registry) IncKavitaScan(result string) {
	r.kavitaScans.WithLabelValues(result).Inc()
}

// IncAniListLookup increments the AniList lookup counter.
// result should be one of "success", "miss", "error", or "cached".
func (r *Registry) IncAniListLookup(result string) {
	r.anilistLookups.WithLabelValues(result).Inc()
}

// IncUnmatched increments the unmatched-events counter.
func (r *Registry) IncUnmatched() {
	r.unmatchedTotal.Inc()
}

// IncFileError increments the file-errors counter.
func (r *Registry) IncFileError() {
	r.fileErrorsTotal.Inc()
}

// IncFileConflict increments the file-conflict counter by one.
func (r *Registry) IncFileConflict() {
	r.fileConflictsTotal.Inc()
}

// ---- gauge methods ----

// SetPollerLastRun sets the poller-last-run gauge to the given time.
func (r *Registry) SetPollerLastRun(t time.Time) {
	r.pollerLastRun.Set(float64(t.Unix()))
}

// SetUnmatchedCount sets the current unmatched-series gauge.
func (r *Registry) SetUnmatchedCount(n int) {
	r.unmatchedSeries.Set(float64(n))
}

// SetSeriesCount sets the series-count gauge for the given category.
func (r *Registry) SetSeriesCount(category string, n int) {
	r.seriesCount.WithLabelValues(category).Set(float64(n))
}

// SetDiskFreeBytes sets the disk-free-bytes gauge for the given root path.
func (r *Registry) SetDiskFreeBytes(root string, bytes uint64) {
	r.diskFreeBytes.WithLabelValues(root).Set(float64(bytes))
}

// SetDiskTotalBytes sets the disk-total-bytes gauge for the given root path.
func (r *Registry) SetDiskTotalBytes(root string, bytes uint64) {
	r.diskTotalBytes.WithLabelValues(root).Set(float64(bytes))
}

// SetHealthStatus sets the health-status gauge for a check ID.
// status: 0=ok, 1=warn, 2=error.
func (r *Registry) SetHealthStatus(id string, status int) {
	r.healthStatus.WithLabelValues(id).Set(float64(status))
}

// SetBackupCount sets the current backup-count gauge.
func (r *Registry) SetBackupCount(n int) {
	r.backupsCount.Set(float64(n))
}

// SetBackupLastModTime sets the backup-last-modtime gauge to the given time.
func (r *Registry) SetBackupLastModTime(t time.Time) {
	r.backupLastModSeconds.Set(float64(t.Unix()))
}

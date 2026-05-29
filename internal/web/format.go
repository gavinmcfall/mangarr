package web

import (
	"fmt"
	"time"
)

// formatBytes converts a byte count to a human-readable string.
// e.g. 1536 → "1.5 KB", 2097152 → "2.0 MB".
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// formatAge returns a short human-readable age string like "just now",
// "5m ago", "3h ago", or "7d ago". Returns "never" when mtime is zero.
func formatAge(now, mtime time.Time) string {
	if mtime.IsZero() {
		return "never"
	}
	d := now.Sub(mtime)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// formatInterval renders a task interval as a short human-readable string.
// Returns "On demand" for 0, otherwise e.g. "15m", "1h", "24h".
func formatInterval(ms int64) string {
	if ms <= 0 {
		return "On demand"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

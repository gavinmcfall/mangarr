package web

import (
	"net/http"
	"net/url"
)

// flashCookie is the short-lived cookie name carrying a one-shot toast message
// from a mutating handler to the next page load. base.html reads it on
// DOMContentLoaded, shows the toast, and clears it. A cookie (rather than an
// HX-Toast response header) is used because most actions end in a full-page
// reload or a 303 redirect — including the native-form series delete — and a
// cookie survives all of those uniformly, whereas a response header is hidden
// once the browser's XHR transparently follows a redirect.
const flashCookie = "mangarr_flash"

// setFlash queues a one-shot toast for the next page load. kind is "success"
// or "error"; msg is the human message. The value is "kind|msg", encoded with
// url.PathEscape so spaces become %20 — NOT url.QueryEscape, whose "+"-for-space
// encoding the browser's decodeURIComponent does not reverse (it would render
// "Synced+3+series"). MaxAge is short so a stale flash never re-appears on an
// unrelated later load. Must be called before the response status/body is written.
func setFlash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    url.PathEscape(kind + "|" + msg),
		Path:     "/",
		MaxAge:   10,
		SameSite: http.SameSiteLaxMode,
	})
}

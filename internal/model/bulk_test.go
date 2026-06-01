package model

import "testing"

func TestBulkJobStatusEnumValues(t *testing.T) {
	// Pin the wire strings — these appear in the SQLite schema's CHECK
	// constraints later and in the JSON API output.
	cases := map[BulkJobStatus]string{
		BulkJobPending:   "pending",
		BulkJobRunning:   "running",
		BulkJobPaused:    "paused",
		BulkJobCompleted: "completed",
		BulkJobErrored:   "errored",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("status: want %q, got %q", want, string(got))
		}
	}
}

func TestBulkChapterStateEnumValues(t *testing.T) {
	cases := map[BulkChapterState]string{
		BulkChapterPending: "pending",
		BulkChapterFed:     "fed",
		BulkChapterDone:    "done",
		BulkChapterErrored: "errored",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("state: want %q, got %q", want, string(got))
		}
	}
}

func TestBulkJobIsTerminal(t *testing.T) {
	terminals := []BulkJobStatus{BulkJobCompleted, BulkJobErrored}
	active := []BulkJobStatus{BulkJobPending, BulkJobRunning, BulkJobPaused}
	for _, s := range terminals {
		if !s.IsTerminal() {
			t.Errorf("status %q should be terminal", s)
		}
	}
	for _, s := range active {
		if s.IsTerminal() {
			t.Errorf("status %q should NOT be terminal", s)
		}
	}
}

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

func TestChapterStateErroredConstAndErroredReasonField(t *testing.T) {
	// Pin the wire string for the stalled-job detector.
	if string(ChapterStateErrored) != "errored" {
		t.Fatalf("ChapterStateErrored: want %q, got %q", "errored", string(ChapterStateErrored))
	}
	// Compile-check that BulkJobChapter has the ErroredReason field.
	bjc := BulkJobChapter{ErroredReason: "empty chapter (source returned 0 pages)"}
	if bjc.ErroredReason != "empty chapter (source returned 0 pages)" {
		t.Fatalf("ErroredReason field: want %q, got %q", "empty chapter (source returned 0 pages)", bjc.ErroredReason)
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

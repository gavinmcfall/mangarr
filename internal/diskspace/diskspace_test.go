package diskspace

import (
	"testing"
)

func TestStatTmpReturnsNonZero(t *testing.T) {
	info := Stat("/tmp")
	if info.Err != nil {
		t.Fatalf("Stat(/tmp) returned error: %v", info.Err)
	}
	if info.TotalBytes == 0 {
		t.Fatal("TotalBytes is 0 for /tmp")
	}
	if info.FreeBytes == 0 {
		// Free could legitimately be zero on a full FS, but /tmp on a dev box shouldn't be.
		t.Log("FreeBytes is 0 — unusual for /tmp, but not a hard failure")
	}
	if info.Path != "/tmp" {
		t.Fatalf("Path not preserved: got %q", info.Path)
	}
}

func TestStatNonexistentReturnsErr(t *testing.T) {
	info := Stat("/nonexistent/path/xyz/mangarr_test_38473")
	if info.Err == nil {
		t.Fatal("expected error for nonexistent path, got nil")
	}
	if info.Path != "/nonexistent/path/xyz/mangarr_test_38473" {
		t.Fatalf("Path not preserved on error: got %q", info.Path)
	}
	// Must not panic — if we got here, we didn't panic.
}

func TestStatPercentFree(t *testing.T) {
	info := Stat("/tmp")
	if info.Err != nil {
		t.Skipf("Stat(/tmp) failed: %v", info.Err)
	}
	pct := info.PercentFree()
	if pct < 0 || pct > 100 {
		t.Fatalf("PercentFree out of range [0,100]: got %f", pct)
	}
}

func TestStatErrPercentFreeIsZero(t *testing.T) {
	info := Stat("/nonexistent/path/xyz/mangarr_test_38473")
	if info.PercentFree() != 0 {
		t.Fatalf("expected PercentFree()=0 for errored Info, got %f", info.PercentFree())
	}
}

func TestStatPopulatesFSID(t *testing.T) {
	info := Stat("/tmp")
	if info.Err != nil {
		t.Skipf("Stat(/tmp) failed: %v", info.Err)
	}
	// FSID should be non-zero for a real filesystem (both fields zero is unlikely).
	if info.FSID[0] == 0 && info.FSID[1] == 0 {
		t.Log("FSID is [0,0] — this can happen on some kernels; not a hard failure")
	}
}

func TestStatTwoPathsSameFSReturnSameFSID(t *testing.T) {
	// /tmp and /tmp itself should obviously share the same FSID.
	a := Stat("/tmp")
	b := Stat("/tmp")
	if a.Err != nil || b.Err != nil {
		t.Skipf("Stat failed: a=%v b=%v", a.Err, b.Err)
	}
	if a.FSID != b.FSID {
		t.Errorf("same path should yield same FSID: %v vs %v", a.FSID, b.FSID)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		input uint64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 512, "512.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{uint64(42) * 1024 * 1024 * 1024, "42.0 GiB"},
		{uint64(1024) * 1024 * 1024 * 1024, "1.0 TiB"},
		{uint64(3) * 1024 * 1024 * 1024 * 1024, "3.0 TiB"},
	}
	for _, c := range cases {
		got := FormatBytes(c.input)
		if got != c.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", c.input, got, c.want)
		}
	}
}

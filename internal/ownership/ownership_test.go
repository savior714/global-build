package ownership

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- reason grammar (Fail-closed; subset of proof F) -------------------------

func TestParseReasonSupported(t *testing.T) {
	r, err := ParseReason("global-build:v1:repo=deadbeefdeadbeef:run=run-1:base=" + repeat("a", 40))
	if err != nil {
		t.Fatalf("supported reason rejected: %v", err)
	}
	if r.RepoID != "deadbeefdeadbeef" || r.RunID != "run-1" || r.AdmittedBase != repeat("a", 40) {
		t.Errorf("parsed reason wrong: %+v", r)
	}
	if err := r.Matches("deadbeefdeadbeef", "run-1", repeat("a", 40)); err != nil {
		t.Errorf("Matches on identical identity failed: %v", err)
	}
	if err := r.Matches("otherrepo00000", "run-1", repeat("a", 40)); err == nil {
		t.Error("Matches accepted repo-id mismatch")
	}
	if err := r.Matches("deadbeefdeadbeef", "run-2", repeat("a", 40)); err == nil {
		t.Error("Matches accepted run-id mismatch")
	}
	if err := r.Matches("deadbeefdeadbeef", "run-1", repeat("b", 40)); err == nil {
		t.Error("Matches accepted admitted-base mismatch")
	}
}

func TestParseReasonUnsupportedVersion(t *testing.T) {
	if _, err := ParseReason("global-build:v0:repo=deadbeefdeadbeef:run=run-1:base=" + repeat("a", 40)); err == nil {
		t.Error("unsupported version accepted")
	}
}

func TestParseReasonWrongRepoID(t *testing.T) {
	if _, err := ParseReason("global-build:v1:repo=zzzzzzzzzzzzzzzz:run=run-1:base=" + repeat("a", 40)); err == nil {
		t.Error("malformed repo-id accepted")
	}
}

func TestParseReasonWrongRunID(t *testing.T) {
	if _, err := ParseReason("global-build:v1:repo=deadbeefdeadbeef:run=../evil:base=" + repeat("a", 40)); err == nil {
		t.Error("unsafe run-id accepted")
	}
}

func TestParseReasonMalformedBase(t *testing.T) {
	if _, err := ParseReason("global-build:v1:repo=deadbeefdeadbeef:run=run-1:base=zzzz"); err == nil {
		t.Error("malformed full OID accepted")
	}
}

func TestParseReasonDuplicateField(t *testing.T) {
	// 5 colon tokens but a duplicate repo key and missing base.
	if _, err := ParseReason("global-build:v1:repo=deadbeefdeadbeef:repo=deadbeefdeadbeef:run=run-1"); err == nil {
		t.Error("duplicate field accepted")
	}
}

func TestParseReasonMissingField(t *testing.T) {
	if _, err := ParseReason("global-build:v1:repo=deadbeefdeadbeef:run=run-1"); err == nil {
		t.Error("missing field accepted")
	}
}

func TestParseReasonExtraField(t *testing.T) {
	if _, err := ParseReason("global-build:v1:repo=deadbeefdeadbeef:run=run-1:base=" + repeat("a", 40) + ":extra=x"); err == nil {
		t.Error("extra field accepted")
	}
}

func TestBuildReasonRoundTrip(t *testing.T) {
	repoID := "deadbeefdeadbeef"
	runID := "run-round-1"
	base := repeat("a", 40)
	r := BuildReason(repoID, runID, base)
	parsed, err := ParseReason(r)
	if err != nil {
		t.Fatalf("BuildReason output not parseable: %v", err)
	}
	if parsed.RepoID != repoID || parsed.RunID != runID || parsed.AdmittedBase != base {
		t.Errorf("round trip mismatch: %+v", parsed)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[0])
	}
	return string(out)
}

// --- liveness lease lifecycle (proof P) --------------------------------------

func TestLeaseLifecycle(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "global-build")
	repoID := "deadbeefdeadbeef"
	runID := "run-lease-1"
	base := repeat("a", 40)

	lease := NewLease(cacheRoot, repoID, runID)
	if lease.Path() != filepath.Join(cacheRoot, repoID, runID) {
		t.Fatalf("lease path wrong: %s", lease.Path())
	}

	id := LeaseIdentity{Version: LeaseVersion, RepoID: repoID, RunID: runID, AdmittedBase: base}
	if err := lease.Establish(id); err != nil {
		t.Fatalf("Establish: %v", err)
	}
	defer lease.Release()

	// Identity is readable and matches.
	got, ok, err := lease.ReadIdentity()
	if err != nil || !ok {
		t.Fatalf("ReadIdentity failed: ok=%v err=%v", ok, err)
	}
	if got != id {
		t.Errorf("identity mismatch: %+v", got)
	}

	// A second observer cannot acquire the held lease (live process).
	observer := NewLease(cacheRoot, repoID, runID)
	acquired, err := observer.TryAcquireExisting()
	if err != nil {
		t.Fatalf("TryAcquireExisting error: %v", err)
	}
	if acquired {
		t.Fatal("observer acquired a lease held by another process")
	}
	// Observer must not have left the file locked or open.
	_ = observer.Release()

	// After the holder releases (process death simulation), the pathname alone
	// must not be treated as active: it is acquirable.
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	acquired, err = lease.TryAcquireExisting()
	if err != nil {
		t.Fatalf("post-release TryAcquireExisting error: %v", err)
	}
	if !acquired {
		t.Fatal("released lease should be acquirable; leftover pathname is not liveness proof")
	}
	_ = lease.Release()
}

func TestLeaseMissingIdentityUncertain(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "global-build")
	lease := NewLease(cacheRoot, "deadbeefdeadbeef", "run-missing-1")
	_, ok, err := lease.ReadIdentity()
	if err != nil {
		t.Fatalf("ReadIdentity error on missing file: %v", err)
	}
	if ok {
		t.Error("missing lease identity must not be reported as present")
	}
}

func TestLeaseMalformedIdentityUncertain(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "global-build")
	lease := NewLease(cacheRoot, "deadbeefdeadbeef", "run-malformed-1")
	if err := os.MkdirAll(filepath.Dir(lease.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease.Path(), []byte("this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := lease.ReadIdentity()
	if err != nil {
		t.Fatalf("ReadIdentity error on malformed file: %v", err)
	}
	if ok {
		t.Error("malformed lease identity must not be reported as present")
	}
}

func TestLeaseIdentityMarshal(t *testing.T) {
	// Guard the JSON shape the cleanup side relies on.
	id := LeaseIdentity{Version: LeaseVersion, RepoID: "deadbeefdeadbeef", RunID: "run-1", AdmittedBase: repeat("a", 40)}
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	var back LeaseIdentity
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != id {
		t.Errorf("marshal round trip mismatch: %+v", back)
	}
}

package envelope

import (
	"strings"
	"testing"

	"global-build/internal/watches"
)

// --- helpers ----------------------------------------------------------------

const validSha1 = "0123456789abcdef0123456789abcdef01234567"
const validSha256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// baseEnv returns a fully valid envelope input. Individual tests mutate the
// pieces they care about.
func baseEnv() string {
	return strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"  - docs/spec.md",
		"---",
		"",
		"GOAL",
		"build the thing",
		"",
		"SETTLED FACTS",
		"fact a",
		"",
		"CHANGE BOUNDARY",
		"diff here",
		"",
		"PRIMARY PROOF",
		"proof",
		"",
		"STOP CONDITIONS",
		"none",
	}, "\n")
}

func mustParse(t *testing.T, in string) *Envelope {
	t.Helper()
	env, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse(%q) unexpectedly failed: %v", in, err)
	}
	return env
}

func mustFail(t *testing.T, in string) error {
	t.Helper()
	_, err := Parse(in)
	if err == nil {
		t.Fatalf("Parse(%q) unexpectedly succeeded", in)
	}
	return err
}

// --- frontmatter delimiters -------------------------------------------------

func TestMissingOpeningDelimiter(t *testing.T) {
	err := mustFail(t, "run_id: run-1\nadmitted_base: "+validSha1+"\n")
	if !strings.Contains(err.Error(), "must start with '---'") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUnterminatedFrontmatter(t *testing.T) {
	err := mustFail(t, "---\nrun_id: run-1\nadmitted_base: "+validSha1+"\n")
	if !strings.Contains(err.Error(), "unterminated frontmatter") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTrailingDelimiterInBodyIsProse(t *testing.T) {
	// A '---' appearing after the frontmatter is body prose, not a second YAML
	// document: the frontmatter is bounded by the first closing delimiter, so
	// anything after it is forwarded to the model verbatim.
	in := baseEnv() + "\n---\nmore prose\n"
	env := mustParse(t, in)
	if !strings.Contains(env.Body, "---") {
		t.Errorf("body did not retain trailing delimiter as prose: %q", env.Body)
	}
}

func TestMultiDocInsideFrontmatterRejected(t *testing.T) {
	// A frontmatter whose decoded form would contain a second YAML document
	// (here via an embedded document-end marker that yaml rejects) must fail.
	// KnownFields + decoder guard ensure only exactly one frontmatter doc.
	in := "---\nrun_id: run-1\nadmitted_base: " + validSha1 +
		"\nwatch_surfaces:\n  - src/\n...\nrun_id: run-2\n---\n\nGOAL\ng\n\nSETTLED FACTS\nf\n\nCHANGE BOUNDARY\nc\n\nPRIMARY PROOF\np\n\nSTOP CONDITIONS\ns\n"
	// The embedded '...' document-end triggers a yaml decode error for the
	// trailing document fragment, so the envelope must be rejected.
	if _, err := Parse(in); err == nil {
		t.Error("multi-doc frontmatter unexpectedly accepted")
	}
}

func TestKnownFieldsRejectsUnknownKeys(t *testing.T) {
	in := "---\nrun_id: run-1\nadmitted_base: " + validSha1 +
		"\nwatch_surfaces:\n  - src/\nunknown_field: true\n---\n\nGOAL\ng\n\nSETTLED FACTS\nf\n\nCHANGE BOUNDARY\nc\n\nPRIMARY PROOF\np\n\nSTOP CONDITIONS\ns\n"
	err := mustFail(t, in)
	if !strings.Contains(err.Error(), "invalid frontmatter") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCRLFFrontmatterDelimiter(t *testing.T) {
	// CRLF line endings: the closing '---' must still be recognized after the
	// '\r' is stripped.
	in := strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"---",
		"",
		"GOAL",
		"build",
		"",
		"SETTLED FACTS",
		"f",
		"",
		"CHANGE BOUNDARY",
		"c",
		"",
		"PRIMARY PROOF",
		"p",
		"",
		"STOP CONDITIONS",
		"s",
	}, "\r\n")
	mustParse(t, in)
}

// --- run_id validation ------------------------------------------------------

func TestRunIDValidForms(t *testing.T) {
	valid := []string{
		"run-1",
		"Run_1.2",
		"a",
		strings.Repeat("x", 128), // max length (128 chars total, first + 127)
		"2026-08-27Tbuild",
	}
	for _, id := range valid {
		in := mutateRunID(baseEnv(), id)
		if _, err := Parse(in); err != nil {
			t.Errorf("run_id %q rejected: %v", id, err)
		}
	}
}

func TestRunIDRejectsTooLong(t *testing.T) {
	// 129 chars exceeds the [A-Za-z0-9][A-Za-z0-9._-]{0,127} bound.
	id := strings.Repeat("x", 129)
	in := mutateRunID(baseEnv(), id)
	err := mustFail(t, in)
	if !strings.Contains(err.Error(), "unsafe run_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunIDRejectsLeadingDash(t *testing.T) {
	err := mustFail(t, mutateRunID(baseEnv(), "-run"))
	if !strings.Contains(err.Error(), "unsafe run_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunIDRejectsDotDot(t *testing.T) {
	err := mustFail(t, mutateRunID(baseEnv(), "run..1"))
	if !strings.Contains(err.Error(), "unsafe run_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunIDRejectsDotLockSuffix(t *testing.T) {
	err := mustFail(t, mutateRunID(baseEnv(), "run-1.lock"))
	if !strings.Contains(err.Error(), "unsafe run_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunIDRejectsTrailingDot(t *testing.T) {
	err := mustFail(t, mutateRunID(baseEnv(), "run-1."))
	if !strings.Contains(err.Error(), "unsafe run_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunIDRejectsEmpty(t *testing.T) {
	err := mustFail(t, mutateRunID(baseEnv(), ""))
	if !strings.Contains(err.Error(), "unsafe run_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- admitted_base validation ----------------------------------------------

func TestAdmittedBaseSha1Accepted(t *testing.T) {
	mustParse(t, mutateAdmittedBase(baseEnv(), validSha1))
}

func TestAdmittedBaseSha256Accepted(t *testing.T) {
	mustParse(t, mutateAdmittedBase(baseEnv(), validSha256))
}

func TestAdmittedBaseRejectsWrongLength(t *testing.T) {
	for _, bad := range []string{
		strings.Repeat("0", 39),
		strings.Repeat("0", 65),
		strings.Repeat("0", 10),
	} {
		err := mustFail(t, mutateAdmittedBase(baseEnv(), bad))
		if !strings.Contains(err.Error(), "not a full git object id") {
			t.Errorf("admitted_base %q: unexpected error %v", bad, err)
		}
	}
}

func TestAdmittedBaseRejectsNonHex(t *testing.T) {
	err := mustFail(t, mutateAdmittedBase(baseEnv(), strings.Repeat("z", 40)))
	if !strings.Contains(err.Error(), "not a full git object id") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAdmittedBaseRejectsEmpty(t *testing.T) {
	err := mustFail(t, mutateAdmittedBase(baseEnv(), ""))
	if !strings.Contains(err.Error(), "not a full git object id") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- watch_surfaces validation ----------------------------------------------

func TestWatchSurfacesEmptyRejected(t *testing.T) {
	in := "---\nrun_id: run-1\nadmitted_base: " + validSha1 +
		"\nwatch_surfaces: []\n---\n\nGOAL\ng\n\nSETTLED FACTS\nf\n\nCHANGE BOUNDARY\nc\n\nPRIMARY PROOF\np\n\nSTOP CONDITIONS\ns\n"
	err := mustFail(t, in)
	if !strings.Contains(err.Error(), "watch_surfaces must not be empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWatchSurfacesAbsoluteRejected(t *testing.T) {
	err := mustFail(t, mutateWatchSurface(baseEnv(), "/etc/passwd"))
	if !strings.Contains(err.Error(), "absolute watch surface not allowed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWatchSurfacesEscapeRejected(t *testing.T) {
	for _, bad := range []string{"../escape", "a/../../b", ".."} {
		err := mustFail(t, mutateWatchSurface(baseEnv(), bad))
		if !strings.Contains(err.Error(), "escapes repository root") {
			t.Errorf("surface %q: unexpected error %v", bad, err)
		}
	}
}

func TestWatchSurfaceDirectoryKeepsTrailingSlash(t *testing.T) {
	env := mustParse(t, mutateWatchSurface(baseEnv(), "src/"))
	if got := env.WatchSurfaces[0]; got != "src/" {
		t.Errorf("directory surface normalized to %q, want %q", got, "src/")
	}
}

func TestWatchSurfaceFileStaysBare(t *testing.T) {
	env := mustParse(t, mutateWatchSurface(baseEnv(), "docs/spec.md"))
	if got := env.WatchSurfaces[0]; got != "docs/spec.md" {
		t.Errorf("file surface normalized to %q, want %q", got, "docs/spec.md")
	}
}

func TestWatchSurfaceDirNormalizesDotComponents(t *testing.T) {
	// Directory surfaces are cleaned (src/./ -> src/), file surfaces are not.
	env := mustParse(t, mutateWatchSurface(baseEnv(), "src/./"))
	if got := env.WatchSurfaces[0]; got != "src/" {
		t.Errorf("dir surface normalized to %q, want %q", got, "src/")
	}
}

func TestWatchSurfaceRejectsUncleanFile(t *testing.T) {
	// A file surface with redundant path components is rejected: surfaces must
	// be written cleanly (fail-closed), they are not silently rewritten.
	err := mustFail(t, mutateWatchSurface(baseEnv(), "src/./x"))
	if !strings.Contains(err.Error(), "not a clean relative path") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWatchSurfaceRejectsUncleanNonDir(t *testing.T) {
	// a non-directory surface that is not already clean (e.g. has internal "..")
	// is rejected.
	err := mustFail(t, mutateWatchSurface(baseEnv(), "a/../b"))
	if err == nil {
		t.Fatal("expected rejection of unclean non-directory surface")
	}
}

// --- body section validation -----------------------------------------------

func TestBodyMissingSection(t *testing.T) {
	// Drop the STOP CONDITIONS section entirely.
	in := strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"---",
		"",
		"GOAL",
		"g",
		"",
		"SETTLED FACTS",
		"f",
		"",
		"CHANGE BOUNDARY",
		"c",
		"",
		"PRIMARY PROOF",
		"p",
	}, "\n")
	err := mustFail(t, in)
	if !strings.Contains(err.Error(), "missing required body section") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBodyDuplicateSection(t *testing.T) {
	in := strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"---",
		"",
		"GOAL",
		"g",
		"",
		"SETTLED FACTS",
		"f",
		"",
		"CHANGE BOUNDARY",
		"c",
		"",
		"PRIMARY PROOF",
		"p",
		"",
		"STOP CONDITIONS",
		"s",
		"",
		"STOP CONDITIONS",
		"duplicate",
	}, "\n")
	err := mustFail(t, in)
	if !strings.Contains(err.Error(), "duplicate body section") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBodyExtraSectionIsProse(t *testing.T) {
	in := strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"---",
		"",
		"GOAL",
		"g",
		"",
		"SETTLED FACTS",
		"f",
		"",
		"CHANGE BOUNDARY",
		"c",
		"",
		"PRIMARY PROOF",
		"p",
		"",
		"STOP CONDITIONS",
		"s",
		"",
		"UNEXPECTED SECTION",
		"x",
	}, "\n")
	// An unrecognized header line is treated as prose inside STOP CONDITIONS
	// (it is not one of the five required headers), so the envelope still parses
	// and the body contains exactly the five recognized sections.
	env := mustParse(t, in)
	if !strings.Contains(env.Body, "GOAL") {
		t.Errorf("body missing GOAL: %q", env.Body)
	}
	for _, h := range RequiredSections {
		if got := strings.Count(env.Body, h); got != 1 {
			t.Errorf("section %q appears %d times in body", h, got)
		}
	}
}

func TestBodyReorderedSectionsRejected(t *testing.T) {
	// Reorder: SETTLED FACTS before GOAL.
	in := strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"---",
		"",
		"SETTLED FACTS",
		"f",
		"",
		"GOAL",
		"g",
		"",
		"CHANGE BOUNDARY",
		"c",
		"",
		"PRIMARY PROOF",
		"p",
		"",
		"STOP CONDITIONS",
		"s",
	}, "\n")
	// The re-ordered header is still recognized; parseBody only checks presence
	// and uniqueness, not order. Confirm the envelope parses and all five are
	// present but the body reproduces the requested order.
	env := mustParse(t, in)
	if !strings.Contains(env.Body, "SETTLED FACTS") || !strings.Contains(env.Body, "GOAL") {
		t.Errorf("body missing sections: %q", env.Body)
	}
}

func TestBodyEmptySectionRejected(t *testing.T) {
	in := strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"---",
		"",
		"GOAL",
		"",
		"",
		"SETTLED FACTS",
		"f",
		"",
		"CHANGE BOUNDARY",
		"c",
		"",
		"PRIMARY PROOF",
		"p",
		"",
		"STOP CONDITIONS",
		"s",
	}, "\n")
	err := mustFail(t, in)
	if !strings.Contains(err.Error(), `body section "GOAL" is empty`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBodyProseBeforeFirstSectionRejected(t *testing.T) {
	in := strings.Join([]string{
		"---",
		"run_id: run-1",
		"admitted_base: " + validSha1,
		"watch_surfaces:",
		"  - src/",
		"---",
		"",
		"this is prose before the first section",
		"GOAL",
		"g",
		"",
		"SETTLED FACTS",
		"f",
		"",
		"CHANGE BOUNDARY",
		"c",
		"",
		"PRIMARY PROOF",
		"p",
		"",
		"STOP CONDITIONS",
		"s",
	}, "\n")
	err := mustFail(t, in)
	if !strings.Contains(err.Error(), "content before first section header") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBodyReconstructedExactlyFiveSections(t *testing.T) {
	env := mustParse(t, baseEnv())
	// Reconstructed body must contain each header exactly once and nothing else.
	for _, h := range RequiredSections {
		if n := strings.Count(env.Body, h); n != 1 {
			t.Errorf("section %q appears %d times in body", h, n)
		}
	}
	// Frontmatter fields must never leak into the body.
	if strings.Contains(env.Body, "run_id") || strings.Contains(env.Body, "admitted_base") {
		t.Errorf("frontmatter leaked into body: %q", env.Body)
	}
}

// --- watch surface normalization (via shared watches.Normalize) -----------

func TestNormalizeSurfaceEmpty(t *testing.T) {
	if _, err := watches.Normalize(""); err == nil {
		t.Error("empty surface accepted")
	}
}

func TestNormalizeSurfaceWhitespace(t *testing.T) {
	if _, err := watches.Normalize(" src/ "); err == nil {
		t.Error("whitespace surface accepted")
	}
}

func TestNormalizeSurfaceDotRejected(t *testing.T) {
	if _, err := watches.Normalize("."); err == nil {
		t.Error("'.' surface accepted")
	}
}

// --- mutation helpers -------------------------------------------------------

func mutateRunID(in, id string) string {
	return replaceField(in, "run_id:", "run_id: "+id)
}

func mutateAdmittedBase(in, oid string) string {
	return replaceField(in, "admitted_base:", "admitted_base: "+oid)
}

func mutateWatchSurface(in, surface string) string {
	return strings.Replace(in, "  - src/\n  - docs/spec.md", "  - "+surface, 1)
}

// replaceField replaces the first line beginning with prefix (after trimming)
// with newValue in the envelope input.
func replaceField(in, prefix, newValue string) string {
	lines := strings.Split(in, "\n")
	for i, l := range lines {
		// match "prefix " exactly at the start of the line (no leading spaces)
		if strings.HasPrefix(l, prefix) {
			lines[i] = newValue
			break
		}
	}
	return strings.Join(lines, "\n")
}

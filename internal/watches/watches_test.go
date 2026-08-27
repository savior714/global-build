package watches

import "testing"

// --- Normalize --------------------------------------------------------------

func TestNormalizeEmptyRejected(t *testing.T) {
	if _, err := Normalize(""); err == nil {
		t.Error("empty surface accepted")
	}
}

func TestNormalizeAbsoluteRejected(t *testing.T) {
	for _, s := range []string{"/etc/passwd", "/repo/src"} {
		if _, err := Normalize(s); err == nil {
			t.Errorf("absolute surface %q accepted", s)
		}
	}
}

func TestNormalizeWhitespaceRejected(t *testing.T) {
	for _, s := range []string{" src/", "src/ ", " src/ "} {
		if _, err := Normalize(s); err == nil {
			t.Errorf("whitespace surface %q accepted", s)
		}
	}
}

func TestNormalizeDotRejected(t *testing.T) {
	for _, s := range []string{".", ".."} {
		if _, err := Normalize(s); err == nil {
			t.Errorf("dot surface %q accepted", s)
		}
	}
}

func TestNormalizeDotDotPrefixRejected(t *testing.T) {
	if _, err := Normalize("../escape"); err == nil {
		t.Error("dot-dot-prefix surface accepted")
	}
}

func TestNormalizeDirSurfaceKeepsTrailingSlash(t *testing.T) {
	got, err := Normalize("src/")
	if err != nil {
		t.Fatalf("Normalize(src/) = %v", err)
	}
	if got != "src/" {
		t.Errorf("Normalize(src/) = %q, want %q", got, "src/")
	}
}

func TestNormalizeDirSurfaceNormalizesComponents(t *testing.T) {
	got, err := Normalize("src/./")
	if err != nil {
		t.Fatalf("Normalize(src/./) = %v", err)
	}
	if got != "src/" {
		t.Errorf("Normalize(src/./) = %q, want %q", got, "src/")
	}
}

func TestNormalizeFileSurfaceStaysBare(t *testing.T) {
	got, err := Normalize("docs/spec.md")
	if err != nil {
		t.Fatalf("Normalize(docs/spec.md) = %v", err)
	}
	if got != "docs/spec.md" {
		t.Errorf("Normalize(docs/spec.md) = %q, want %q", got, "docs/spec.md")
	}
}

func TestNormalizeFileSurfaceRejectsUnclean(t *testing.T) {
	for _, s := range []string{"src/./x", "a/../b", "docs/../spec.md"} {
		if _, err := Normalize(s); err == nil {
			t.Errorf("unclean file surface %q accepted", s)
		}
	}
}

func TestNormalizeDeepPathAccepted(t *testing.T) {
	got, err := Normalize("a/b/c/d.md")
	if err != nil {
		t.Fatalf("Normalize(a/b/c/d.md) = %v", err)
	}
	if got != "a/b/c/d.md" {
		t.Errorf("Normalize(a/b/c/d.md) = %q, want %q", got, "a/b/c/d.md")
	}
}

// --- Set.Contains -----------------------------------------------------------

func TestSetContainsExactFile(t *testing.T) {
	s := New([]string{"docs/spec.md"})
	if !s.Contains("docs/spec.md") {
		t.Error("exact file not contained")
	}
	if s.Contains("docs/other.md") {
		t.Error("sibling file incorrectly contained")
	}
}

func TestSetContainsDirPrefix(t *testing.T) {
	s := New([]string{"src/"})
	if !s.Contains("src/main.go") {
		t.Error("file under dir prefix not contained")
	}
	if !s.Contains("src/sub/deep.go") {
		t.Error("deep file under dir prefix not contained")
	}
	if s.Contains("other/src/main.go") {
		t.Error("sibling dir file incorrectly contained")
	}
}

func TestSetContainsMixedSurfaces(t *testing.T) {
	s := New([]string{"src/", "docs/spec.md", "README.md"})
	if !s.Contains("src/main.go") {
		t.Error("dir prefix failed")
	}
	if !s.Contains("docs/spec.md") {
		t.Error("exact file failed")
	}
	if !s.Contains("README.md") {
		t.Error("root exact file failed")
	}
	if s.Contains("cmd/main.go") {
		t.Error("outside surface incorrectly contained")
	}
}

func TestSetContainsEmptySet(t *testing.T) {
	s := New([]string{})
	if s.Contains("anything") {
		t.Error("empty set contained something")
	}
}

func TestSetContainsTrailingSlashDirVsFile(t *testing.T) {
	// "src" (no slash) is an exact file; "src/" is a directory prefix.
	s := New([]string{"src"})
	if !s.Contains("src") {
		t.Error("exact file 'src' not contained")
	}
	if s.Contains("src/main.go") {
		t.Error("file under 'src' directory incorrectly contained (surface is exact file)")
	}

	s2 := New([]string{"src/"})
	if s2.Contains("src") {
		t.Error("'src' file incorrectly contained by dir prefix 'src/'")
	}
	if !s2.Contains("src/main.go") {
		t.Error("file under 'src/' dir prefix not contained")
	}
}

// --- Set.CoversAll ----------------------------------------------------------

func TestCoversAllClean(t *testing.T) {
	s := New([]string{"src/", "docs/spec.md"})
	ok, outside := s.CoversAll([]string{"src/main.go", "docs/spec.md"})
	if !ok {
		t.Errorf("expected clean, outside = %v", outside)
	}
}

func TestCoversAllOutside(t *testing.T) {
	s := New([]string{"src/"})
	ok, outside := s.CoversAll([]string{"src/main.go", "cmd/cli.go"})
	if ok {
		t.Error("expected dirty, got clean")
	}
	if len(outside) != 1 || outside[0] != "cmd/cli.go" {
		t.Errorf("outside = %v, want [cmd/cli.go]", outside)
	}
}

func TestCoversAllEmptyPaths(t *testing.T) {
	s := New([]string{"src/"})
	ok, outside := s.CoversAll([]string{})
	if !ok {
		t.Errorf("expected clean for empty paths, outside = %v", outside)
	}
}

func TestCoversAllEmptySet(t *testing.T) {
	s := New([]string{})
	ok, outside := s.CoversAll([]string{"anything"})
	if ok {
		t.Error("expected dirty for empty set with paths")
	}
	if len(outside) != 1 {
		t.Errorf("outside = %v, want 1 element", outside)
	}
}

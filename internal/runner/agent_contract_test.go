package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"global-build/internal/envelope"
)

// TestEnvelopeParseValid checks the canonical envelope parses and that the
// forwarded body contains only the five sections.
func TestEnvelopeParseValid(t *testing.T) {
	in := "---\nrun_id: run-abc-123\nadmitted_base: " + strings.Repeat("a", 40) + "\nwatch_surfaces:\n  - docs/\n  - README.md\n---\n\nGOAL\nDo the thing.\n\nSETTLED FACTS\nIt is settled.\n\nCHANGE BOUNDARY\ndocs/ only.\n\nPRIMARY PROOF\ngo test.\n\nSTOP CONDITIONS\nStop when done.\n"
	env, err := envelope.Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if env.RunID != "run-abc-123" {
		t.Errorf("RunID = %q", env.RunID)
	}
	if len(env.WatchSurfaces) != 2 || env.WatchSurfaces[0] != "docs/" || env.WatchSurfaces[1] != "README.md" {
		t.Errorf("WatchSurfaces = %v", env.WatchSurfaces)
	}
	body := env.Body
	for _, banned := range []string{"run_id", "admitted_base", "watch_surfaces"} {
		if strings.Contains(body, banned) {
			t.Errorf("body leaks frontmatter key %q:\n%s", banned, body)
		}
	}
	for _, section := range envelope.RequiredSections {
		if !strings.Contains(body, section+"\n") {
			t.Errorf("body missing section %q:\n%s", section, body)
		}
	}
}

func TestEnvelopeUnsafeRunIDRejected(t *testing.T) {
	base := strings.Repeat("a", 40)
	cases := []string{
		"../evil",
		"a..b",
		"ends.",
		"ends.lock",
		"-leading-dash",
		"",
		strings.Repeat("x", 200),
		"has space",
	}
	for _, id := range cases {
		in := "---\nrun_id: " + id + "\nadmitted_base: " + base + "\nwatch_surfaces:\n  - docs/\n---\nGOAL\nx\nSETTLED FACTS\nx\nCHANGE BOUNDARY\nx\nPRIMARY PROOF\nx\nSTOP CONDITIONS\nx\n"
		if _, err := envelope.Parse(in); err == nil {
			t.Errorf("run_id %q accepted", id)
		}
	}
}

func TestEnvelopeBodySectionValidation(t *testing.T) {
	fm := "---\nrun_id: ok1\nadmitted_base: " + strings.Repeat("b", 40) + "\nwatch_surfaces:\n  - docs/\n---\n"
	full := "GOAL\ng\n\nSETTLED FACTS\ns\n\nCHANGE BOUNDARY\nc\n\nPRIMARY PROOF\np\n\nSTOP CONDITIONS\nst\n"

	if _, err := envelope.Parse(fm + full); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}

	// missing section
	missing := "GOAL\ng\n\nSETTLED FACTS\ns\n\nPRIMARY PROOF\np\n\nSTOP CONDITIONS\nst\n"
	if _, err := envelope.Parse(fm + missing); err == nil {
		t.Error("missing section accepted")
	}

	// duplicate section
	dup := full + "GOAL\ng2\n"
	if _, err := envelope.Parse(fm + dup); err == nil {
		t.Error("duplicate section accepted")
	}

	// pre-header junk
	if _, err := envelope.Parse(fm + "some preamble text\n" + full); err == nil {
		t.Error("pre-header content accepted")
	}

	// empty section content
	empty := "GOAL\n\nSETTLED FACTS\ns\n\nCHANGE BOUNDARY\nc\n\nPRIMARY PROOF\np\n\nSTOP CONDITIONS\nst\n"
	if _, err := envelope.Parse(fm + empty); err == nil {
		t.Error("empty GOAL section accepted")
	}

	// unknown frontmatter field rejected (KnownFields)
	if _, err := envelope.Parse("---\nrun_id: ok2\nadmitted_base: " + strings.Repeat("c", 40) + "\nwatch_surfaces:\n  - docs/\nsurprise: 1\n---\n" + full); err == nil {
		t.Error("unknown frontmatter field accepted")
	}

	// unsafe watch surface rejected
	badSurf := "---\nrun_id: ok3\nadmitted_base: " + strings.Repeat("d", 40) + "\nwatch_surfaces:\n  - ../escape\n---\n" + full
	if _, err := envelope.Parse(badSurf); err == nil {
		t.Error("escaping watch surface accepted")
	}
}

// TestAgentContractSmoke guards the generated global worker agent against
// accidental drift: wrong mode, wrong steps value, weakened permissions, or a
// missing publication prohibition.
func TestAgentContractSmoke(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	path := filepath.Join(home, ".config", "opencode", "agents", "global-build-worker.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("worker agent file must exist at %s: %v", path, err)
	}
	text := string(raw)

	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		t.Fatalf("agent file lacks frontmatter delimiters")
	}
	frontmatterText, body := parts[1], parts[2]

	var fm struct {
		Description string `yaml:"description"`
		Mode        string `yaml:"mode"`
		Steps       int    `yaml:"steps"`
		Permission  struct {
			Edit      string            `yaml:"edit"`
			Webfetch  string            `yaml:"webfetch"`
			Websearch string            `yaml:"websearch"`
			Question  string            `yaml:"question"`
			Task      string            `yaml:"task"`
			Bash      map[string]string `yaml:"bash"`
		} `yaml:"permission"`
	}
	dec := newYAMLDecoder(frontmatterText)
	if err := dec.Decode(&fm); err != nil {
		t.Fatalf("frontmatter does not parse under yaml/v3: %v", err)
	}

	if fm.Mode != "primary" {
		t.Errorf("mode = %q, want primary", fm.Mode)
	}
	if fm.Steps != 35 {
		t.Errorf("steps = %d, want exactly 35", fm.Steps)
	}
	if fm.Permission.Edit != "allow" {
		t.Errorf("edit permission = %q, want allow", fm.Permission.Edit)
	}
	for _, key := range []string{"Webfetch", "Websearch", "Question", "Task"} {
		v := map[string]string{"Webfetch": fm.Permission.Webfetch, "Websearch": fm.Permission.Websearch, "Question": fm.Permission.Question, "Task": fm.Permission.Task}[key]
		if v != "deny" {
			t.Errorf("%s permission = %q, want deny", key, v)
		}
	}

	bash := fm.Permission.Bash
	if bash["*"] != "allow" {
		t.Errorf(`bash["*"] = %q, want "allow" for cross-repository build capability`, bash["*"])
	}
	for _, rule := range []string{
		"git push*", "git reset --hard*", "git clean*", "git worktree prune*",
		"git worktree remove*", "git update-ref*", "sudo*",
	} {
		if bash[rule] != "deny" {
			t.Errorf(`bash[%q] = %q, want deny`, rule, bash[rule])
		}
	}

	// Ordered rules: broad allow FIRST, narrow denies LAST (last-match-wins).
	broadIdx := strings.Index(frontmatterText, `"*": "allow"`)
	pushDenyIdx := strings.Index(frontmatterText, `"git push*": "deny"`)
	if broadIdx < 0 || pushDenyIdx < 0 || broadIdx > pushDenyIdx {
		t.Errorf("bash rule ordering broken: broad allow at %d, git push deny at %d", broadIdx, pushDenyIdx)
	}

	// Publication prohibition and exact protocol blocks must be present.
	mustContain := []string{
		"Never push",
		"RESULT: COMPLETE",
		"PRIMARY_PROOF: PASS",
		"RESULT: CONTINUABLE",
		"ADMITTED_BASE:",
		"WORKTREE_STATE:",
		"RESULT: BLOCKED",
		"BLOCKER:",
		"EVIDENCE:",
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Errorf("agent body missing required contract text %q", s)
		}
	}
}

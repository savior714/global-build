package runner

import (
	"strings"
	"testing"

	"global-build/internal/envelope"
	"global-build/internal/opencode"
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

// TestAgentContractSmoke guards the repo-owned canonical worker agent against
// accidental drift: wrong mode, reintroduction of a fixed steps budget,
// weakened permissions, or a missing publication prohibition. The source of
// truth is the embedded file at internal/opencode/global-build-worker.md, not
// the installed home-directory copy.
func TestAgentContractSmoke(t *testing.T) {
	raw, err := opencode.EmbeddedWorkerSource()
	if err != nil {
		t.Fatalf("cannot read embedded worker source: %v", err)
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
			Task      map[string]string `yaml:"task"`
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
	// The canonical worker must NOT declare a fixed model-steering step budget.
	// Removing it is the root-cause change that lets the worker terminate on its
	// own (model-directed) instead of on a fixed step counter.
	if strings.Contains(frontmatterText, "steps:") {
		t.Errorf("canonical worker frontmatter must not declare a steps budget:\n%s", frontmatterText)
	}
	if fm.Steps != 0 {
		t.Errorf("canonical worker steps = %d, want 0 (no step budget)", fm.Steps)
	}
	if fm.Permission.Edit != "allow" {
		t.Errorf("edit permission = %q, want allow", fm.Permission.Edit)
	}
	if fm.Permission.Webfetch != "allow" {
		t.Errorf("webfetch permission = %q, want allow", fm.Permission.Webfetch)
	}
	if fm.Permission.Websearch != "allow" {
		t.Errorf("websearch permission = %q, want allow", fm.Permission.Websearch)
	}
	if fm.Permission.Question != "deny" {
		t.Errorf("question permission = %q, want deny", fm.Permission.Question)
	}

	task := fm.Permission.Task
	if task["*"] != "deny" {
		t.Errorf(`task["*"] = %q, want "deny"`, task["*"])
	}
	if task["global-build-explore"] != "allow" {
		t.Errorf(`task["global-build-explore"] = %q, want "allow"`, task["global-build-explore"])
	}
	if len(task) != 2 {
		t.Errorf("task permission must admit only global-build-explore; got %v", task)
	}

	// Ordered task rules: broad deny FIRST, narrow global-build-explore allow LAST
	// (OpenCode permission matching is last-match-wins).
	taskDenyIdx := strings.Index(frontmatterText, `"*": deny`)
	exploreAllowIdx := strings.Index(frontmatterText, `global-build-explore: allow`)
	if taskDenyIdx < 0 || exploreAllowIdx < 0 || taskDenyIdx > exploreAllowIdx {
		t.Errorf("task rule ordering broken: broad deny at %d, global-build-explore allow at %d", taskDenyIdx, exploreAllowIdx)
	}

	bash := fm.Permission.Bash
	if bash["*"] != "allow" {
		t.Errorf(`bash["*"] = %q, want "allow" for ordinary local shell capability`, bash["*"])
	}
	// Dangerous topology/ref/remote operations must remain denied.
	for _, rule := range []string{
		"git merge*", "git rebase*", "git cherry-pick*", "git fetch*",
		"git pull*", "git push*", "git branch*", "git tag*",
		"git update-ref*", "git remote*", "git clone*", "git reset*",
		"git stash*", "git worktree*", "git clean*",
		"git gc*", "git prune*", "git reflog*", "git filter-branch*",
		"git replace*", "sudo*",
	} {
		if bash[rule] != "deny" {
			t.Errorf(`bash[%q] = %q, want deny`, rule, bash[rule])
		}
	}

	// Ordered rules: broad allow FIRST, narrow denies LAST (last-match-wins).
	broadIdx := strings.Index(frontmatterText, `"*": "allow"`)
	pushDenyIdx := strings.Index(frontmatterText, `"git push*": "deny"`)
	if broadIdx < 0 || pushDenyIdx < 0 {
		t.Errorf("bash rule ordering incomplete: broad allow at %d, git push deny at %d", broadIdx, pushDenyIdx)
	}
	if broadIdx > pushDenyIdx {
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

	// The worker must not carry step-budget or fixed-investigator-budget language.
	// Delegation is optional and model-directed, not capped by a fixed count, and
	// the worker terminates on its own rather than on a step counter.
	mustNotContain := []string{
		"step exhaustion",
		"hard maximum of three",
		"maximum of three Explore",
		"steps:",
		"Bounded",
	}
	for _, s := range mustNotContain {
		if strings.Contains(frontmatterText, s) {
			t.Errorf("agent frontmatter must not contain step/investigator budget language %q", s)
		}
		if strings.Contains(body, s) {
			t.Errorf("agent body must not contain step/investigator budget language %q", s)
		}
	}
}

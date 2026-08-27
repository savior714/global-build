// Package envelope parses and validates the stdin-only task envelope consumed
// by the global-build runner.
//
// Envelope shape:
//
//	---
//	run_id: <collision-resistant-id>
//	admitted_base: <exact-git-oid>
//	watch_surfaces:
//	  - <path-or-bounded-surface>
//	---
//
//	GOAL
//	...
//
//	SETTLED FACTS
//	...
//
//	CHANGE BOUNDARY
//	...
//
//	PRIMARY PROOF
//	...
//
//	STOP CONDITIONS
//	...
//
// The runner forwards ONLY the five body sections to the model worker.
// run_id, admitted_base and watch_surfaces never reach the model.
package envelope

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"global-build/internal/watches"
)

// Section headers required in the body, exactly these five, no more, no less.
var RequiredSections = []string{
	"GOAL",
	"SETTLED FACTS",
	"CHANGE BOUNDARY",
	"PRIMARY PROOF",
	"STOP CONDITIONS",
}

var (
	runIDRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	admittedRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// Envelope is the validated task input.
type Envelope struct {
	RunID         string
	AdmittedBase  string
	WatchSurfaces []string // normalized: dir surfaces carry a trailing "/"
	Body          string   // forwarded to the model: only the five sections
}

type frontmatter struct {
	RunID         string   `yaml:"run_id"`
	AdmittedBase  string   `yaml:"admitted_base"`
	WatchSurfaces []string `yaml:"watch_surfaces"`
}

// Parse validates raw stdin input and returns the envelope. It never repairs
// malformed input; any deviation is an error.
func Parse(input string) (*Envelope, error) {
	lines := strings.Split(input, "\n")
	if len(lines) < 3 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return nil, errors.New("envelope: input must start with '---' frontmatter delimiter")
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return nil, errors.New("envelope: unterminated frontmatter (no closing '---')")
	}

	fmText := strings.Join(lines[1:closeIdx], "\n")
	var fm frontmatter
	dec := yaml.NewDecoder(strings.NewReader(fmText))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil {
		return nil, fmt.Errorf("envelope: invalid frontmatter: %w", err)
	}
	// Reject any second YAML document hiding behind the closing delimiter.
	if err := dec.Decode(&struct{}{}); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("envelope: trailing content after frontmatter document: %w", err)
	}

	env := &Envelope{RunID: fm.RunID, AdmittedBase: fm.AdmittedBase}

	if !runIDRe.MatchString(fm.RunID) {
		return nil, fmt.Errorf("envelope: unsafe run_id %q: must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}", fm.RunID)
	}
	if strings.Contains(fm.RunID, "..") || strings.HasSuffix(fm.RunID, ".lock") || strings.HasSuffix(fm.RunID, ".") {
		return nil, fmt.Errorf("envelope: unsafe run_id %q: path/ref namespace escape", fm.RunID)
	}
	env.RunID = fm.RunID

	if !admittedRe.MatchString(fm.AdmittedBase) {
		return nil, fmt.Errorf("envelope: admitted_base %q is not a full git object id", fm.AdmittedBase)
	}

	if len(fm.WatchSurfaces) == 0 {
		return nil, errors.New("envelope: watch_surfaces must not be empty")
	}
	for _, s := range fm.WatchSurfaces {
		norm, err := watches.Normalize(s)
		if err != nil {
			return nil, fmt.Errorf("envelope: watch_surfaces entry %q: %w", s, err)
		}
		env.WatchSurfaces = append(env.WatchSurfaces, norm)
	}

	bodyLines := lines[closeIdx+1:]
	body, err := parseBody(bodyLines)
	if err != nil {
		return nil, err
	}
	env.Body = body
	return env, nil
}

// parseBody enforces exactly the five required sections: each present exactly
// once, no extra sections, non-empty contents, nothing before the first header.
// The returned value is the reconstructed body containing only the five
// sections (frontmatter can never leak into it).
func parseBody(lines []string) (string, error) {
	type section struct {
		name    string
		content []string
	}
	var order []section
	current := -1
	seen := map[string]bool{}

	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		isHeader := false
		for _, h := range RequiredSections {
			if line == h {
				isHeader = true
				if seen[h] {
					return "", fmt.Errorf("envelope: duplicate body section %q", h)
				}
				seen[h] = true
				order = append(order, section{name: h})
				current = len(order) - 1
				break
			}
		}
		if isHeader {
			continue
		}
		if current < 0 {
			if strings.TrimSpace(line) == "" {
				continue // blank preamble tolerated
			}
			return "", fmt.Errorf("envelope: body has content before first section header (expected one of: %s)", strings.Join(RequiredSections, ", "))
		}
		order[current].content = append(order[current].content, line)
	}

	for _, h := range RequiredSections {
		if !seen[h] {
			return "", fmt.Errorf("envelope: missing required body section %q", h)
		}
	}

	var b strings.Builder
	for i, sec := range order {
		content := trimBlank(sec.content)
		if len(content) == 0 {
			return "", fmt.Errorf("envelope: body section %q is empty", sec.name)
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(sec.name)
		b.WriteString("\n")
		for _, l := range content {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

func trimBlank(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}

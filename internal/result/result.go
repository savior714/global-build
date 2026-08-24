// Package result validates the final assistant response against the strict
// BUILD result protocol. It fails closed: almost-correct text is rejected,
// nothing is auto-filled, intent is never inferred from malformed output.
package result

import (
	"fmt"
	"strings"
)

// Kind is the terminal result kind declared by the worker.
type Kind string

const (
	Complete    Kind = "COMPLETE"
	Continuable Kind = "CONTINUABLE"
	Blocked     Kind = "BLOCKED"
)

// Required fields per kind (beyond RESULT itself).
var requiredFields = map[Kind][]string{
	Complete: {"PRIMARY_PROOF"},
	Continuable: {
		"ADMITTED_BASE",
		"COMPLETED",
		"REMAINING",
		"NEXT_ACTION",
		"DO_NOT_REOPEN",
		"VERIFICATION_ALREADY_DONE",
		"WORKTREE_STATE",
	},
	Blocked: {"BLOCKER", "EVIDENCE"},
}

// Result is a validated protocol response.
type Result struct {
	Kind   Kind
	Keys   []string          // field order as written
	Fields map[string]string // trimmed values; multi-line values contain \n
}

// Parse extracts and validates the final assistant text. The first meaningful
// line must be "RESULT: <KIND>"; every other line must belong to the exact
// schema of that kind (unknown or duplicated fields are rejected). Continuation
// lines that do not parse as KEY: VALUE are appended to the previous value.
func Parse(text string) (*Result, error) {
	lines := strings.Split(text, "\n")
	// Trim blank leading/trailing lines.
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	lines = lines[start:end]
	if len(lines) == 0 {
		return nil, fmt.Errorf("result: empty final response")
	}

	head := strings.TrimSpace(lines[0])
	var kind Kind
	for _, k := range []Kind{Complete, Continuable, Blocked} {
		if head == fmt.Sprintf("RESULT: %s", k) {
			kind = k
			break
		}
	}
	if kind == "" {
		return nil, fmt.Errorf("result: first line %q is not a valid RESULT declaration", head)
	}

	r := &Result{Kind: kind, Fields: map[string]string{}}
	lastKey := ""
	for _, raw := range lines[1:] {
		line := strings.TrimRight(raw, " \t\r")
		if key, val, ok := splitField(line); ok {
			if _, dup := r.Fields[key]; dup {
				return nil, fmt.Errorf("result: duplicate field %q", key)
			}
			if !allowedField(kind, key) {
				return nil, fmt.Errorf("result: unexpected field %q for RESULT: %s", key, kind)
			}
			r.Keys = append(r.Keys, key)
			r.Fields[key] = strings.TrimSpace(val)
			lastKey = key
			continue
		}
		if lastKey == "" {
			return nil, fmt.Errorf("result: stray line before any field: %q", line)
		}
		r.Fields[lastKey] += "\n" + strings.TrimSpace(line)
	}

	for _, req := range requiredFields[kind] {
		v, ok := r.Fields[req]
		if !ok || v == "" {
			return nil, fmt.Errorf("result: missing required field %q for RESULT: %s", req, kind)
		}
	}
	if kind == Complete && r.Fields["PRIMARY_PROOF"] != "PASS" {
		return nil, fmt.Errorf("result: PRIMARY_PROOF must be PASS for COMPLETE, got %q", r.Fields["PRIMARY_PROOF"])
	}
	return r, nil
}

func allowedField(kind Kind, key string) bool {
	for _, f := range requiredFields[kind] {
		if f == key {
			return true
		}
	}
	return false
}

func splitField(line string) (key, val string, ok bool) {
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	key = line[:i]
	if key != strings.ToUpper(key) { // keys are SCREAMING_SNAKE only
		return "", "", false
	}
	for _, c := range key {
		if !(c >= 'A' && c <= 'Z' || c == '_') {
			return "", "", false
		}
	}
	val = strings.TrimLeft(line[i+1:], " ")
	return key, val, true
}

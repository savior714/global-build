// Package watches implements the conservative machine-checkable upper bound
// for candidate change paths: exact file paths and directory-prefix surfaces
// only. No dependency graphs, no semantic path matching.
package watches

import (
	"sort"
	"strings"
)

// Set is the compiled WATCH_SURFACES bound.
type Set struct {
	exact    map[string]struct{}
	prefixes []string // each ends with "/"
}

func New(surfaces []string) *Set {
	s := &Set{exact: make(map[string]struct{}, len(surfaces))}
	for _, surf := range surfaces {
		if strings.HasSuffix(surf, "/") {
			s.prefixes = append(s.prefixes, surf)
			continue
		}
		s.exact[surf] = struct{}{}
	}
	sort.Strings(s.prefixes)
	return s
}

// Contains reports whether path (slash-separated, repository-relative, as
// emitted by git diff --name-only) falls inside the surface bound.
func (s *Set) Contains(p string) bool {
	if _, ok := s.exact[p]; ok {
		return true
	}
	for _, prefix := range s.prefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// CoversAll reports whether every changed path lies inside the bound and
// returns the offending paths otherwise.
func (s *Set) CoversAll(paths []string) (ok bool, outside []string) {
	for _, p := range paths {
		if !s.Contains(p) {
			outside = append(outside, p)
		}
	}
	return len(outside) == 0, outside
}

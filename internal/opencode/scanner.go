package opencode

import (
	"bufio"
	"io"
)

// newLargeScanner wraps r in a bufio.Scanner sized for NDJSON event lines
// containing large tool outputs (up to 8 MiB per line).
func newLargeScanner(r io.Reader, buf *[]byte) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	if *buf == nil {
		*buf = make([]byte, 0, 64*1024)
	}
	sc.Buffer(*buf, 8*1024*1024)
	return sc
}

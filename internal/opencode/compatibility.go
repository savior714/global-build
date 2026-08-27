package opencode

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// The current global-build runtime contract is proven against the OpenCode
	// 1.x legacy agent/permission generation. 1.18.23 is the latest explicitly
	// accepted runtime proof; 1.18 is therefore the minimum admitted minor line.
	SupportedMajor    = 1
	MinSupportedMinor = 18
	versionProbeLimit = 5 * time.Second
)

var versionRE = regexp.MustCompile(`(?m)(?:^|[^0-9])v?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)(?:$|[^0-9])`)

// CheckCompatibility proves that the installed OpenCode executable belongs to
// the configuration generation global-build currently understands. This is a
// fail-closed generation guard, not an exact patch pin: newer compatible 1.x
// minors remain admissible, while pre-1.18 and V2 must be independently
// accepted before global-build will execute a mutating BUILD with them.
func CheckCompatibility(ctx context.Context, bin string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, versionProbeLimit)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, bin, "--version")
	out, err := cmd.CombinedOutput()
	if probeCtx.Err() != nil {
		return "", fmt.Errorf("OpenCode compatibility probe timed out after %s", versionProbeLimit)
	}
	if err != nil {
		return "", fmt.Errorf("OpenCode compatibility probe failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	version, major, minor, err := parseVersion(string(out))
	if err != nil {
		return "", err
	}
	if major != SupportedMajor || minor < MinSupportedMinor {
		return "", fmt.Errorf("unsupported OpenCode version %s: global-build is accepted only for OpenCode %d.x at >= %d.%d; a different configuration generation must be independently accepted first", version, SupportedMajor, SupportedMajor, MinSupportedMinor)
	}
	return version, nil
}

func parseVersion(output string) (version string, major, minor int, err error) {
	match := versionRE.FindStringSubmatch(output)
	if len(match) < 2 {
		return "", 0, 0, fmt.Errorf("cannot determine OpenCode semantic version from %q", strings.TrimSpace(output))
	}
	version = match[1]
	core := strings.FieldsFunc(version, func(r rune) bool { return r == '-' || r == '+' })[0]
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return "", 0, 0, fmt.Errorf("cannot parse OpenCode semantic version %q", version)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return "", 0, 0, fmt.Errorf("cannot parse OpenCode major version %q: %w", version, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, 0, fmt.Errorf("cannot parse OpenCode minor version %q: %w", version, err)
	}
	return version, major, minor, nil
}

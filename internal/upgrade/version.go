// Package upgrade implements muxcat's self-update: querying GitHub
// releases, downloading release assets with retries, verifying them
// against checksums.txt, and atomically replacing the running binary.
package upgrade

import (
	"fmt"
	"strconv"
	"strings"
)

// DevVersion is the version of local (non-release) builds. A dev build is
// always considered upgradable.
const DevVersion = "dev"

// normalizeVersion strips whitespace and a leading "v" (tags are v0.1.0,
// assets and main.version carry 0.1.0).
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// compareVersions compares two x.y.z versions numerically, returning -1, 0
// or 1. Non-semver input yields an error; callers fall back to inequality.
func compareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}

func parseVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(normalizeVersion(v), ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("not a x.y.z version: %q", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("not a x.y.z version: %q", v)
		}
		out[i] = n
	}
	return out, nil
}

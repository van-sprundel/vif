package version

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Stability levels ordered from least to most stable.
type Stability int

const (
	Dev    Stability = iota // dev-*, x.y.z-dev
	Alpha                   // x.y.z-alpha1
	Beta                    // x.y.z-beta1
	RC                      // x.y.z-RC1
	Stable                  // x.y.z
	Patch                   // x.y.z-patch1, x.y.z-p1
)

// Version is a parsed Composer version.
type Version struct {
	Major        int
	Minor        int
	Patch        int
	Tweak        int // optional 4th segment
	Stability    Stability
	StabilityNum int    // e.g. 2 in -beta2
	Dev          bool   // true for dev-* branches
	DevBranch    string // e.g. "main" for dev-main
	Original     string // original input string
}

var (
	devBranchRe = regexp.MustCompile(`^dev-(.+)$`)
	versionRe   = regexp.MustCompile(`^[vV]?(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:\.(\d+))?(?:[-.]?(dev|alpha|beta|rc|RC|patch|p|stable)(\d*))?$`)
)

// Parse parses a Composer version string into a Version.
func Parse(s string) (Version, error) {
	if s == "" {
		return Version{}, fmt.Errorf("version: empty string")
	}

	// Dev branch.
	if m := devBranchRe.FindStringSubmatch(s); m != nil {
		branch := m[1]
		if branch == "" {
			return Version{}, fmt.Errorf("version: empty dev branch in %q", s)
		}
		return Version{
			Dev:       true,
			DevBranch: branch,
			Stability: Dev,
			Original:  s,
		}, nil
	}

	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("version: invalid %q", s)
	}

	v := Version{Original: s}

	v.Major, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		v.Minor, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
	}
	if m[4] != "" {
		v.Tweak, _ = strconv.Atoi(m[4])
	}

	// Stability suffix.
	if m[5] != "" {
		stabStr := strings.ToLower(m[5])
		switch stabStr {
		case "dev":
			v.Stability = Dev
		case "alpha":
			v.Stability = Alpha
		case "beta":
			v.Stability = Beta
		case "rc":
			v.Stability = RC
		case "patch", "p":
			v.Stability = Patch
		case "stable":
			v.Stability = Stable
		}
		if m[6] != "" {
			v.StabilityNum, _ = strconv.Atoi(m[6])
		}
	} else {
		v.Stability = Stable
	}

	return v, nil
}

// String returns a normalized version string.
func (v Version) String() string {
	if v.Dev {
		return "dev-" + v.DevBranch
	}

	var b strings.Builder
	if v.Tweak > 0 {
		fmt.Fprintf(&b, "%d.%d.%d.%d", v.Major, v.Minor, v.Patch, v.Tweak)
	} else {
		fmt.Fprintf(&b, "%d.%d.%d", v.Major, v.Minor, v.Patch)
	}

	switch v.Stability {
	case Dev:
		b.WriteString("-dev")
	case Alpha:
		fmt.Fprintf(&b, "-alpha%d", v.StabilityNum)
	case Beta:
		fmt.Fprintf(&b, "-beta%d", v.StabilityNum)
	case RC:
		fmt.Fprintf(&b, "-RC%d", v.StabilityNum)
	case Patch:
		fmt.Fprintf(&b, "-patch%d", v.StabilityNum)
	}

	return b.String()
}

// StabilityAtLeast reports whether v's stability is >= minStab.
func (v Version) StabilityAtLeast(minStab Stability) bool {
	return v.Stability >= minStab
}

// Compare returns -1, 0, or 1 comparing a and b.
// Dev branches are always less than any numbered version.
func Compare(a, b Version) int {
	// Dev branches are lowest priority.
	if a.Dev && b.Dev {
		return 0
	}
	if a.Dev {
		return -1
	}
	if b.Dev {
		return 1
	}

	// Compare numeric segments.
	if c := cmpInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	if c := cmpInt(a.Tweak, b.Tweak); c != 0 {
		return c
	}

	// Compare stability.
	if c := cmpInt(int(a.Stability), int(b.Stability)); c != 0 {
		return c
	}

	// Compare stability number.
	return cmpInt(a.StabilityNum, b.StabilityNum)
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

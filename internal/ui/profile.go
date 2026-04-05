package ui

import (
	"fmt"
	"io"
	"sort"
	"time"
)

type ProfileSection struct {
	Name     string
	Duration time.Duration
}

type ProfilePackage struct {
	Name     string
	Duration time.Duration
}

func PrintProfile(w io.Writer, total time.Duration, sections []ProfileSection, packages []ProfilePackage) {
	fmt.Fprintln(w, "\nProfile summary")
	fmt.Fprintf(w, "  total: %s\n", formatDuration(total))

	sort.Slice(sections, func(i, j int) bool {
		return sections[i].Duration > sections[j].Duration
	})
	for _, section := range sections {
		fmt.Fprintf(w, "  - %-20s %s\n", section.Name+":", formatDuration(section.Duration))
	}

	if len(packages) == 0 {
		return
	}

	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Duration == packages[j].Duration {
			return packages[i].Name < packages[j].Name
		}
		return packages[i].Duration > packages[j].Duration
	})

	const maxPackages = 8
	if len(packages) > maxPackages {
		packages = packages[:maxPackages]
	}

	fmt.Fprintln(w, "  slowest packages:")
	for i, p := range packages {
		fmt.Fprintf(w, "    %d. %s (%s)\n", i+1, p.Name, formatDuration(p.Duration))
	}
}

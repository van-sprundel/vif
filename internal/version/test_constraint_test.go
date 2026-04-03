package version_test

import (
	"fmt"
	"github.com/van-sprundel/vif/internal/version"
	"testing"
)

func TestCaretZero(t *testing.T) {
	testCases := []string{"^0", "^0.0", "^0.0.0", "^0.11", "^0.11.3"}
	for _, tc := range testCases {
		c, err := version.ParseConstraint(tc)
		if err != nil {
			fmt.Printf("%s -> ERROR: %v\n", tc, err)
			continue
		}
		fmt.Printf("%s -> %s\n", tc, c.String())
	}
}

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newSearchCmd returns the `vif search` command.
func newSearchCmd() *cobra.Command {
	var nameOnly bool

	cmd := &cobra.Command{
		Use:          "search <query>",
		Short:        "Search for packages on Packagist",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			return runSearch(query, nameOnly)
		},
	}

	cmd.Flags().BoolVarP(&nameOnly, "name-only", "N", false, "show only package names")

	return cmd
}

type packagistSearchResponse struct {
	Results []packagistSearchResult `json:"results"`
	Total   int                     `json:"total"`
}

type packagistSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Downloads   int    `json:"downloads"`
	Favers      int    `json:"favers"`
}

func runSearch(query string, nameOnly bool) error {
	w := os.Stdout

	searchURL := "https://packagist.org/search.json?q=" + url.QueryEscape(query)
	resp, err := http.Get(searchURL)
	if err != nil {
		return fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("packagist returned HTTP %d", resp.StatusCode)
	}

	var result packagistSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode search results: %w", err)
	}

	if len(result.Results) == 0 {
		fmt.Fprintln(os.Stderr, "No packages found.")
		return nil
	}

	if nameOnly {
		for _, r := range result.Results {
			fmt.Fprintln(w, r.Name)
		}
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range result.Results {
		desc := r.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\n", r.Name, desc)
	}
	tw.Flush()

	return nil
}

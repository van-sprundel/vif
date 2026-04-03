package autoload

import (
	"fmt"
	"sort"
	"strings"

	"github.com/van-sprundel/vif/internal/pkg"
)

type InstalledVersionsConfig struct {
	RootName        string
	RootVersion     string
	RootType        string
	RootReference   string
	DevMode         bool
	DevPackageNames map[string]bool
}

func generateInstalledPHP(packages []pkg.Package, cfg *InstalledVersionsConfig) string {
	var b strings.Builder
	b.WriteString("<?php return array(\n")

	rootName := ""
	rootPrettyVersion := "1.0.0"
	rootVersion := "1.0.0.0"
	rootReference := "null"
	rootType := "library"
	rootInstallPath := "__DIR__ . '/..'"
	devMode := false

	if cfg != nil {
		rootName = cfg.RootName
		rootType = cfg.RootType
		devMode = cfg.DevMode
		if cfg.RootVersion != "" {
			rootPrettyVersion = cfg.RootVersion
			rootVersion = normalizeVersion(cfg.RootVersion)
		}
		if cfg.RootReference != "" {
			rootReference = fmt.Sprintf("'%s'", phpEscape(cfg.RootReference))
		}
		if rootType == "" {
			rootType = "library"
		}
	}

	fmt.Fprintf(&b, "    'root' => array(\n")
	fmt.Fprintf(&b, "        'name' => '%s',\n", phpEscape(rootName))
	fmt.Fprintf(&b, "        'pretty_version' => '%s',\n", phpEscape(rootPrettyVersion))
	fmt.Fprintf(&b, "        'version' => '%s',\n", phpEscape(rootVersion))
	fmt.Fprintf(&b, "        'reference' => %s,\n", rootReference)
	fmt.Fprintf(&b, "        'type' => '%s',\n", phpEscape(rootType))
	fmt.Fprintf(&b, "        'install_path' => %s,\n", rootInstallPath)
	fmt.Fprintf(&b, "        'aliases' => array(),\n")
	fmt.Fprintf(&b, "        'dev' => %v,\n", devMode)
	fmt.Fprintf(&b, "    ),\n")

	b.WriteString("    'versions' => array(\n")

	names := make([]string, 0, len(packages))
	pkgMap := make(map[string]*pkg.Package, len(packages))
	for i := range packages {
		p := &packages[i]
		if _, exists := pkgMap[p.Name]; !exists {
			names = append(names, p.Name)
		}
		pkgMap[p.Name] = p
	}
	sort.Strings(names)

	for _, name := range names {
		p := pkgMap[name]
		isDev := false
		if cfg != nil && cfg.DevPackageNames != nil {
			isDev = cfg.DevPackageNames[name]
		}

		installPath := "__DIR__ . '/..' . '/" + phpEscape(name) + "'"
		if p.Type == "metapackage" {
			installPath = "null"
		}

		reference := "null"
		if p.Dist.Reference != "" {
			reference = fmt.Sprintf("'%s'", phpEscape(p.Dist.Reference))
		} else if p.Source.Reference != "" {
			reference = fmt.Sprintf("'%s'", phpEscape(p.Source.Reference))
		}

		prettyVersion := p.Version
		version := p.VersionNormalized
		if version == "" {
			version = normalizeVersion(prettyVersion)
		}

		fmt.Fprintf(&b, "        '%s' => array(\n", phpEscape(name))
		fmt.Fprintf(&b, "            'pretty_version' => '%s',\n", phpEscape(prettyVersion))
		fmt.Fprintf(&b, "            'version' => '%s',\n", phpEscape(version))
		fmt.Fprintf(&b, "            'reference' => %s,\n", reference)
		fmt.Fprintf(&b, "            'type' => '%s',\n", phpEscape(p.Type))
		fmt.Fprintf(&b, "            'install_path' => %s,\n", installPath)
		fmt.Fprintf(&b, "            'aliases' => array(),\n")
		fmt.Fprintf(&b, "            'dev_requirement' => %v,\n", isDev)

		if len(p.Provide) > 0 {
			provNames := sortedMapKeys(p.Provide)
			b.WriteString("            'provided' => array(\n")
			for _, provName := range provNames {
				fmt.Fprintf(&b, "                '%s',\n", phpEscape(p.Provide[provName]))
			}
			b.WriteString("            ),\n")
		}

		if len(p.Replace) > 0 {
			repNames := sortedMapKeys(p.Replace)
			b.WriteString("            'replaced' => array(\n")
			for _, repName := range repNames {
				fmt.Fprintf(&b, "                '%s',\n", phpEscape(p.Replace[repName]))
			}
			b.WriteString("            ),\n")
		}

		b.WriteString("        ),\n")
	}

	b.WriteString("    ),\n")
	b.WriteString(");\n")

	return b.String()
}

func normalizeVersion(v string) string {
	parts := strings.SplitN(v, ".", 4)
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	if len(parts) == 3 {
		parts = append(parts, "0")
	}
	return strings.Join(parts, ".")
}

func phpEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

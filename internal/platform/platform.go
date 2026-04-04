package platform

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/van-sprundel/vif/internal/pkg"
	"github.com/van-sprundel/vif/internal/version"
)

type Platform struct {
	PHPVersion string
	Extensions map[string]bool
}

func Detect() (*Platform, error) {
	phpVer, err := detectPHPVersion()
	if err != nil {
		return nil, fmt.Errorf("platform: detect PHP: %w", err)
	}
	exts, err := detectExtensions()
	if err != nil {
		return nil, fmt.Errorf("platform: detect extensions: %w", err)
	}
	return &Platform{
		PHPVersion: phpVer,
		Extensions: exts,
	}, nil
}

var phpVersionRe = regexp.MustCompile(`^PHP\s+(\S+)`)

func detectPHPVersion() (string, error) {
	out, err := exec.Command("php", "-r", "echo PHP_VERSION;").Output()
	if err != nil {
		return "", fmt.Errorf("php -r echo PHP_VERSION: %w", err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("empty PHP version")
	}
	return v, nil
}

func detectExtensions() (map[string]bool, error) {
	out, err := exec.Command("php", "-m").Output()
	if err != nil {
		return nil, fmt.Errorf("php -m: %w", err)
	}
	exts := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		exts["ext-"+strings.ToLower(line)] = true
	}
	return exts, nil
}

type RequirementError struct {
	PackageName string
	Requirement string
	Constraint  string
}

func (e RequirementError) Error() string {
	return fmt.Sprintf("%s requires %s %s but it is not satisfied", e.PackageName, e.Requirement, e.Constraint)
}

func VerifyPackages(platform *Platform, packages []pkg.Package) []error {
	var errs []error
	for _, p := range packages {
		if p.Require == nil {
			continue
		}
		for req, constraintStr := range p.Require {
			if !pkg.IsPlatformPackage(req) {
				continue
			}
			if err := verifyRequirement(platform, p.Name, req, constraintStr); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

func verifyRequirement(platform *Platform, pkgName, req, constraintStr string) error {
	switch {
	case req == "php":
		return verifyPHP(platform, pkgName, constraintStr)
	case strings.HasPrefix(req, "ext-"):
		return verifyExtension(platform, pkgName, req, constraintStr)
	case req == "php-64bit":
		return nil
	case req == "hhvm", req == "composer-plugin-api", req == "composer-runtime-api":
		return nil
	default:
		return nil
	}
}

func verifyPHP(platform *Platform, pkgName, constraintStr string) error {
	c, err := version.ParseConstraint(constraintStr)
	if err != nil {
		return fmt.Errorf("%s: parse php constraint %q: %w", pkgName, constraintStr, err)
	}
	v, err := version.Parse(platform.PHPVersion)
	if err != nil {
		return fmt.Errorf("%s: parse current PHP version %q: %w", pkgName, platform.PHPVersion, err)
	}
	if !c.Matches(v) {
		return RequirementError{
			PackageName: pkgName,
			Requirement: "php",
			Constraint:  constraintStr,
		}
	}
	return nil
}

func verifyExtension(platform *Platform, pkgName, extName, constraintStr string) error {
	if constraintStr == "*" {
		if !platform.Extensions[extName] {
			return RequirementError{
				PackageName: pkgName,
				Requirement: extName,
				Constraint:  "*",
			}
		}
		return nil
	}

	if !platform.Extensions[extName] {
		return RequirementError{
			PackageName: pkgName,
			Requirement: extName,
			Constraint:  constraintStr,
		}
	}
	return nil
}

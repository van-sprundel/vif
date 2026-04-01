package pkg

import "strings"

// IsPlatformPackage returns true if the package name is a PHP platform
// package (php, ext-*, lib-*, etc.) that should be filtered from resolution.
func IsPlatformPackage(name string) bool {
	return name == "php" ||
		strings.HasPrefix(name, "ext-") ||
		strings.HasPrefix(name, "lib-") ||
		name == "php-64bit" ||
		name == "php-ipv6" ||
		name == "hhvm" ||
		name == "composer-plugin-api" ||
		name == "composer-runtime-api"
}

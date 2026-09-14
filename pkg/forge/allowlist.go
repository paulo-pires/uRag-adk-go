package forge

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// AllowedPackages is the strict security allowlist for dependencies in generated apps.
// Arbitrary npm install is prohibited; all preview builds execute with fixed pre-installed toolchain.
var AllowedPackages = map[string]bool{
	"react":                         true,
	"react-dom":                     true,
	"lucide-react":                  true,
	"clsx":                          true,
	"tailwind-merge":                true,
	"vite":                          true,
	"@vitejs/plugin-react":          true,
	"tailwindcss":                   true,
	"postcss":                       true,
	"autoprefixer":                  true,
	"typescript":                    true,
	"@types/react":                  true,
	"@types/react-dom":              true,
	"@types/node":                   true,
	"@rollup/rollup-linux-x64-musl": true,
}

type packageJSON struct {
	Name            string            `json:"name"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// ValidatePackageJSON checks that all declared dependencies are within the security allowlist.
// Dependencies outside the allowlist are rejected with a log entry and error.
func ValidatePackageJSON(content []byte) error {
	if len(content) == 0 {
		return fmt.Errorf("package.json is empty")
	}

	var pkg packageJSON
	if err := json.Unmarshal(content, &pkg); err != nil {
		return fmt.Errorf("invalid package.json JSON: %w", err)
	}

	var unauthorized []string

	for dep := range pkg.Dependencies {
		depName := strings.TrimSpace(dep)
		if !AllowedPackages[depName] {
			unauthorized = append(unauthorized, depName)
		}
	}

	for dep := range pkg.DevDependencies {
		depName := strings.TrimSpace(dep)
		if !AllowedPackages[depName] {
			unauthorized = append(unauthorized, depName)
		}
	}

	if len(unauthorized) > 0 {
		errMsg := fmt.Sprintf("unauthorized dependencies in package.json: %s (rejected by security allowlist)", strings.Join(unauthorized, ", "))
		log.Printf("[SECURITY ALLOWLIST REJECTION] %s", errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	return nil
}

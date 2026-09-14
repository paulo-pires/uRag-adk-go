package forge

import (
	"fmt"
	"regexp"
	"strings"
)

// Diagnostic represents a compiler or build diagnostic item.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Code     string `json:"code"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

var (
	// Example: src/components/AppointmentForm.tsx:15:23 - error TS2304: Cannot find name 'Calendar'.
	tsErrorRegex = regexp.MustCompile(`(?m)^([^\s:]+\.tsx?):(\d+):(\d+)\s*-\s*(error|warning)\s+(TS\d+):\s*(.+)$`)
	// Example: [vite:esbuild] Transform failed with 1 error: src/App.tsx:5:10: ERROR: Could not resolve "missing-pkg"
	viteErrorRegex = regexp.MustCompile(`(?m)(?:\[vite:[^\]]+\]\s+)?([^\s:]+\.tsx?):(\d+):(\d+):\s*(ERROR|WARNING):\s*(.+)$`)
	// Example: Could not resolve "./Missing" from "src/App.tsx"
	rollupResolveRegex = regexp.MustCompile(`(?m)Could not resolve "([^"]+)" from "([^"]+)"`)
	// Example: Cannot find module 'xyz'
	cannotFindModuleRegex = regexp.MustCompile(`(?m)Cannot find module '([^']+)'`)
)

// ParseBuildOutput extracts structured diagnostics from compiler stderr and stdout.
func ParseBuildOutput(output string) []Diagnostic {
	var diagnostics []Diagnostic

	// Check TypeScript error pattern
	tsMatches := tsErrorRegex.FindAllStringSubmatch(output, -1)
	for _, m := range tsMatches {
		var line, col int
		fmt.Sscanf(m[2], "%d", &line)
		fmt.Sscanf(m[3], "%d", &col)
		diagnostics = append(diagnostics, Diagnostic{
			File:     strings.TrimSpace(m[1]),
			Line:     line,
			Column:   col,
			Category: strings.ToLower(m[4]),
			Code:     strings.TrimSpace(m[5]),
			Message:  strings.TrimSpace(m[6]),
		})
	}

	// Check Vite / esbuild error pattern
	viteMatches := viteErrorRegex.FindAllStringSubmatch(output, -1)
	for _, m := range viteMatches {
		var line, col int
		fmt.Sscanf(m[2], "%d", &line)
		fmt.Sscanf(m[3], "%d", &col)
		diagnostics = append(diagnostics, Diagnostic{
			File:     strings.TrimSpace(m[1]),
			Line:     line,
			Column:   col,
			Category: strings.ToLower(m[4]),
			Code:     "VITE_BUILD",
			Message:  strings.TrimSpace(m[5]),
		})
	}

	// Check Rollup module resolve error
	rollupMatches := rollupResolveRegex.FindAllStringSubmatch(output, -1)
	for _, m := range rollupMatches {
		diagnostics = append(diagnostics, Diagnostic{
			File:     strings.TrimSpace(m[2]),
			Category: "error",
			Code:     "UNRESOLVED_IMPORT",
			Message:  fmt.Sprintf("Could not resolve import %q from %s", m[1], m[2]),
		})
	}

	// Check Cannot find module
	modMatches := cannotFindModuleRegex.FindAllStringSubmatch(output, -1)
	for _, m := range modMatches {
		diagnostics = append(diagnostics, Diagnostic{
			File:     "module",
			Category: "error",
			Code:     "MODULE_NOT_FOUND",
			Message:  fmt.Sprintf("Cannot find module %q", m[1]),
		})
	}

	// If no structured pattern matched but output contains error indicators, create a generic diagnostic
	if len(diagnostics) == 0 && (strings.Contains(output, "error") || strings.Contains(output, "Error") || strings.Contains(output, "failed")) {
		diagnostics = append(diagnostics, Diagnostic{
			File:     "build",
			Category: "error",
			Code:     "BUILD_FAILURE",
			Message:  strings.TrimSpace(output),
		})
	}

	return diagnostics
}

// FormatDiagnosticsForPrompt creates a concise string representation of build failures for feedback.
func FormatDiagnosticsForPrompt(diagnostics []Diagnostic, rawOutput string) string {
	if len(diagnostics) == 0 {
		return strings.TrimSpace(rawOutput)
	}

	var sb strings.Builder
	for i, d := range diagnostics {
		if d.Line > 0 {
			sb.WriteString(fmt.Sprintf("%d. [%s] %s:%d:%d - %s\n", i+1, d.Code, d.File, d.Line, d.Column, d.Message))
		} else {
			sb.WriteString(fmt.Sprintf("%d. [%s] %s - %s\n", i+1, d.Code, d.File, d.Message))
		}
	}
	return sb.String()
}

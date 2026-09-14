package forge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SandboxConfig specifies resource and isolation constraints for the compilation sandbox.
type SandboxConfig struct {
	TemplateDir string
	MemoryLimit string        // e.g. "512m"
	CPUs        string        // e.g. "1.0"
	Timeout     time.Duration // e.g. 30s
	UseDocker   bool
	DockerImage string
}

// DefaultSandboxConfig returns standard security-hardened configuration with node:22-alpine.
func DefaultSandboxConfig(templateDir string) SandboxConfig {
	if templateDir == "" {
		templateDir = findTemplateDir()
	}
	return SandboxConfig{
		TemplateDir: templateDir,
		MemoryLimit: "512m",
		CPUs:        "1.0",
		Timeout:     30 * time.Second,
		UseDocker:   isDockerAvailable(),
		DockerImage: "node:22-alpine",
	}
}

func findTemplateDir() string {
	candidates := []string{
		"template",
		"../template",
		"../../template",
		"../../../urag-forge-go/template",
		"/home/usuarioftp/urag-stack/urag-forge-go/template",
		"/home/usuarioftp/urag-stack/poc-forge-agentico/template",
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	return "template"
}

func isDockerAvailable() bool {
	cmd := exec.Command("docker", "version")
	return cmd.Run() == nil
}

// Sandbox manages an ephemeral workspace mounted directly into the Docker container.
type Sandbox struct {
	cfg       SandboxConfig
	workDir   string
	mu        sync.Mutex
	files     map[string]string
}

// NewSandbox creates an ephemeral workspace directory initialized from template.
func NewSandbox(cfg SandboxConfig) (*Sandbox, error) {
	tmpDir, err := os.MkdirTemp("", "forge-agent-sandbox-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox tmpdir: %w", err)
	}

	sb := &Sandbox{
		cfg:     cfg,
		workDir: tmpDir,
		files:   make(map[string]string),
	}

	if err := sb.initFromTemplate(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("failed to initialize template in sandbox: %w", err)
	}

	return sb, nil
}

func (s *Sandbox) initFromTemplate() error {
	tDir := s.cfg.TemplateDir
	if fi, err := os.Stat(tDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("template directory not found at %q", tDir)
	}

	entries, err := os.ReadDir(tDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(tDir, entry.Name())
		dstPath := filepath.Join(s.workDir, entry.Name())

		if entry.Name() == "node_modules" {
			if s.cfg.UseDocker {
				// Create empty mountpoint for container volume
				if err := os.MkdirAll(dstPath, 0755); err != nil {
					return err
				}
			} else {
				if err := os.Symlink(srcPath, dstPath); err != nil {
					_ = copyDir(srcPath, dstPath)
				}
			}
			continue
		}

		if entry.Name() == "dist" {
			continue
		}

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteFiles writes files into the sandbox filesystem (which is bind-mounted in Docker).
func (s *Sandbox) WriteFiles(files map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate package.json if being written
	if pkgJSON, ok := files["package.json"]; ok && strings.TrimSpace(pkgJSON) != "" {
		if err := ValidatePackageJSON([]byte(pkgJSON)); err != nil {
			return err
		}
	}

	for relPath, content := range files {
		if relPath == "" || strings.TrimSpace(content) == "" {
			continue
		}
		fullPath := filepath.Join(s.workDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", relPath, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", relPath, err)
		}
		s.files[relPath] = content
	}
	return nil
}

// ReadFile reads a file from the workspace.
func (s *Sandbox) ReadFile(relPath string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fullPath := filepath.Join(s.workDir, relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Files returns a copy of current workspace files.
func (s *Sandbox) Files() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]string, len(s.files))
	for k, v := range s.files {
		out[k] = v
	}
	return out
}

// Close cleans up the ephemeral workspace directory.
func (s *Sandbox) Close() error {
	return os.RemoveAll(s.workDir)
}

// WorkDir returns the workspace directory path on host.
func (s *Sandbox) WorkDir() string {
	return s.workDir
}

// BuildResult encapsulates the compilation outcome.
type BuildResult struct {
	Success     bool              `json:"success"`
	ExitCode    int               `json:"exit_code"`
	RawOutput   string            `json:"raw_output"`
	Error       string            `json:"error,omitempty"`
	Diagnostics []Diagnostic      `json:"diagnostics,omitempty"`
	Duration    time.Duration     `json:"duration"`
	OutputFiles map[string]string `json:"output_files,omitempty"`
}

// Compile executes TypeScript typecheck and Vite build inside the Docker container.
func (s *Sandbox) Compile(ctx context.Context) (*BuildResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	start := time.Now()

	// Validate package.json if present
	if pkgJSON, ok := s.files["package.json"]; ok && strings.TrimSpace(pkgJSON) != "" {
		if err := ValidatePackageJSON([]byte(pkgJSON)); err != nil {
			dur := time.Since(start)
			return &BuildResult{
				Success:     false,
				ExitCode:    1,
				Error:       fmt.Sprintf("Security allowlist violation: %v", err),
				RawOutput:   err.Error(),
				Diagnostics: []Diagnostic{{File: "package.json", Category: "error", Code: "ALLOWLIST_VIOLATION", Message: err.Error()}},
				Duration:    dur,
			}, nil
		}
	}

	timeout := s.cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	buildCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	var cmd *exec.Cmd

	if s.cfg.UseDocker {
		nodeModulesHost := filepath.Join(s.cfg.TemplateDir, "node_modules")
		args := []string{
			"run", "--rm",
			"--memory=" + s.cfg.MemoryLimit,
			"--cpus=" + s.cfg.CPUs,
			"--pids-limit=100",
			"-v", fmt.Sprintf("%s:/app", s.workDir),
			"-v", fmt.Sprintf("%s:/app/node_modules", nodeModulesHost),
			"-w", "/app",
			s.cfg.DockerImage,
			"sh", "-c", "./node_modules/.bin/tsc --noEmit && ./node_modules/.bin/vite build",
		}
		cmd = exec.CommandContext(buildCtx, "docker", args...)
	} else {
		cmd = exec.CommandContext(buildCtx, "sh", "-c", "./node_modules/.bin/tsc --noEmit && ./node_modules/.bin/vite build")
		cmd.Dir = s.workDir
	}

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	duration := time.Since(start)
	combinedOutput := strings.TrimSpace(stdout.String() + "\n" + stderr.String())

	if runErr != nil {
		exitCode := -1
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		diags := ParseBuildOutput(combinedOutput)
		formattedErr := FormatDiagnosticsForPrompt(diags, combinedOutput)
		return &BuildResult{
			Success:     false,
			ExitCode:    exitCode,
			RawOutput:   combinedOutput,
			Error:       formattedErr,
			Diagnostics: diags,
			Duration:    duration,
		}, nil
	}

	// Read output files from dist/
	distDir := filepath.Join(s.workDir, "dist")
	distFiles := make(map[string]string)
	_ = filepath.WalkDir(distDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.workDir, path)
		if err != nil {
			return nil
		}
		content, err := os.ReadFile(path)
		if err == nil {
			distFiles[rel] = string(content)
		}
		return nil
	})

	return &BuildResult{
		Success:     true,
		ExitCode:    0,
		RawOutput:   combinedOutput,
		Diagnostics: nil,
		Duration:    duration,
		OutputFiles: distFiles,
	}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		s := filepath.Join(src, entry.Name())
		d := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
		} else {
			if err := copyFile(s, d); err != nil {
				return err
			}
		}
	}
	return nil
}

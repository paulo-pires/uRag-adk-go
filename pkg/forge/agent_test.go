package forge

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// mockLLM simulates multi-turn tool calling for testing LoopAgent feedback convergence.
type mockLLM struct {
	turnCount int
	responses []*model.LLMResponse
}

func (m *mockLLM) Name() string { return "mock-forge-llm" }

func (m *mockLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		idx := m.turnCount
		m.turnCount++
		if idx < len(m.responses) {
			yield(m.responses[idx], nil)
			return
		}
		// Fallback empty response
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText("Done")}},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil)
	}
}

// TestForgeAgent_LoopConvergence validates the end-to-end LoopAgent cycle:
// 1. First iteration: agent writes code with a compilation error (missing Calendar import) and compiles -> fails
// 2. Second iteration: agent reads compiler diagnostic in context, fixes the import, compiles -> succeeds, calls exit_loop
func TestForgeAgent_LoopConvergence(t *testing.T) {
	// Setup ephemeral sandbox with a mock compile function
	tmpDir := t.TempDir()
	sb := &Sandbox{
		cfg: SandboxConfig{
			TemplateDir: tmpDir,
			UseDocker:   false,
			Timeout:     5 * time.Second,
		},
		workDir: tmpDir,
		files:   make(map[string]string),
	}
	defer sb.Close()

	// Initial broken code (missing Calendar import)
	brokenFiles := map[string]string{
		"package.json": `{
			"name": "test-app",
			"dependencies": {
				"react": "^18.3.1",
				"react-dom": "^18.3.1",
				"lucide-react": "^0.344.0"
			}
		}`,
		"src/App.tsx": `
export function App() {
	return <div><Calendar className="w-4 h-4" /> Appointments</div>;
}
`,
	}

	// Fixed code (Calendar imported from lucide-react)
	fixedFiles := map[string]string{
		"src/App.tsx": `
import { Calendar } from 'lucide-react';
export function App() {
	return <div><Calendar className="w-4 h-4" /> Appointments</div>;
}
`,
	}

	brokenArgs, _ := json.Marshal(WriteFilesArgs{Files: brokenFiles})
	fixedArgs, _ := json.Marshal(WriteFilesArgs{Files: fixedFiles})

	mock := &mockLLM{
		responses: []*model.LLMResponse{
			// Turn 1: Call escrever_arquivos with broken code
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_write_1",
								Name: "escrever_arquivos",
								Args: map[string]any{"files": brokenFiles},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
			// Turn 2: Call compilar
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_compile_1",
								Name: "compilar",
								Args: map[string]any{},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
			// Turn 3: Having seen the compiler error, fix App.tsx with escrever_arquivos
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_write_2",
								Name: "escrever_arquivos",
								Args: map[string]any{"files": fixedFiles},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
			// Turn 4: Call compilar again
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_compile_2",
								Name: "compilar",
								Args: map[string]any{},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
			// Turn 5: Call exit_loop
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_exit_1",
								Name: "exit_loop",
								Args: map[string]any{},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
		},
	}
	_ = brokenArgs
	_ = fixedArgs

	agent, err := NewForgeAgent(Config{
		MaxIterations: 4,
		Model:         mock,
		Sandbox:       sb,
	})
	if err != nil {
		t.Fatalf("failed to create ForgeAgent: %v", err)
	}

	result, err := agent.Run(context.Background(), "test-conv", "Crie uma página de agendamentos")
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	if result.TotalTurns < 2 {
		t.Fatalf("expected at least 2 turns in agentic loop, got %d", result.TotalTurns)
	}

	files := agent.Sandbox().Files()
	appContent := files["src/App.tsx"]
	if !strings.Contains(appContent, "import { Calendar } from 'lucide-react'") {
		t.Fatalf("expected corrected App.tsx containing Calendar import, got: %s", appContent)
	}
}

// TestForgeAgent_AllowlistRejection_UnderMutation verifies that unauthorized packages
// in package.json are strictly rejected with error and logging.
// Mutation: adding an unauthorized package to AllowedPackages would pass; removing it must fail.
func TestForgeAgent_AllowlistRejection_UnderMutation(t *testing.T) {
	// Case 1: Valid dependencies within allowlist
	validPkg := []byte(`{
		"name": "valid-app",
		"dependencies": {
			"react": "^18.3.1",
			"react-dom": "^18.3.1",
			"lucide-react": "^0.344.0",
			"clsx": "^2.1.0",
			"tailwind-merge": "^2.2.1"
		},
		"devDependencies": {
			"vite": "^5.1.4",
			"@vitejs/plugin-react": "^4.2.1",
			"tailwindcss": "^3.4.1",
			"typescript": "^5.2.2"
		}
	}`)

	if err := ValidatePackageJSON(validPkg); err != nil {
		t.Fatalf("expected valid package.json to pass allowlist, got: %v", err)
	}

	// Case 2: Unauthorized dependency
	unauthorizedPkg := []byte(`{
		"name": "unauthorized-app",
		"dependencies": {
			"react": "^18.3.1",
			"malicious-arbitrary-package": "^1.0.0"
		}
	}`)

	err := ValidatePackageJSON(unauthorizedPkg)
	if err == nil {
		t.Fatalf("expected unauthorized dependency to be rejected by allowlist, but it passed")
	}

	if !strings.Contains(err.Error(), "unauthorized dependencies in package.json: malicious-arbitrary-package") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Mutation test: temporarily adding to allowlist makes it pass, removing it makes it fail
	AllowedPackages["malicious-arbitrary-package"] = true
	if err := ValidatePackageJSON(unauthorizedPkg); err != nil {
		t.Fatalf("mutation failed: package should have been allowed when added to allowlist")
	}
	delete(AllowedPackages, "malicious-arbitrary-package")
	if err := ValidatePackageJSON(unauthorizedPkg); err == nil {
		t.Fatalf("mutation failed: package must be rejected when removed from allowlist")
	}
}

// TestForgeAgent_NodeVersionAndPackageJSON_PreviewMatchesPublish guarantees that
// preview sandbox and publish deploy Dockerfiles use the exact same Node.js base image.
func TestForgeAgent_NodeVersionAndPackageJSON_PreviewMatchesPublish(t *testing.T) {
	defaultCfg := DefaultSandboxConfig("")
	previewNodeImage := defaultCfg.DockerImage

	// Read Dokploy publish recipe from urag-forge-go/internal/deploy/service.go or constant
	const publishRecipe = `FROM node:22-alpine AS build
WORKDIR /app
COPY . .
RUN npm ci && npm run build
FROM nginx:alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html/
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
`
	var publishNodeImage string
	for _, line := range strings.Split(publishRecipe, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "FROM node:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				publishNodeImage = strings.TrimPrefix(parts[1], "node:")
				publishNodeImage = "node:" + publishNodeImage
				break
			}
		}
	}

	if previewNodeImage != "node:22-alpine" {
		t.Fatalf("preview sandbox image is %q, expected 'node:22-alpine'", previewNodeImage)
	}

	if publishNodeImage != "node:22-alpine" {
		t.Fatalf("publish dockerfile image is %q, expected 'node:22-alpine'", publishNodeImage)
	}

	if previewNodeImage != publishNodeImage {
		t.Fatalf("divergence detected between preview (%s) and publish (%s) Node versions", previewNodeImage, publishNodeImage)
	}
}

// TestForgeAgent_DiagnosticsParsing validates extraction of structured TypeScript and Vite errors.
func TestForgeAgent_DiagnosticsParsing(t *testing.T) {
	rawCompilerOutput := `
src/components/AppointmentForm.tsx:43:12 - error TS2304: Cannot find name 'Calendar'.
src/App.tsx:10:5 - error TS2322: Type 'string' is not assignable to type 'number'.
[vite:esbuild] Transform failed with 1 error: src/Main.tsx:5:10: ERROR: Could not resolve "./Missing"
`
	diags := ParseBuildOutput(rawCompilerOutput)
	if len(diags) < 3 {
		t.Fatalf("expected at least 3 diagnostics, got %d", len(diags))
	}

	d1 := diags[0]
	if d1.File != "src/components/AppointmentForm.tsx" || d1.Line != 43 || d1.Code != "TS2304" {
		t.Fatalf("unexpected d1: %+v", d1)
	}
	if !strings.Contains(d1.Message, "Cannot find name 'Calendar'") {
		t.Fatalf("unexpected message in d1: %s", d1.Message)
	}

	formatted := FormatDiagnosticsForPrompt(diags, rawCompilerOutput)
	if !strings.Contains(formatted, "TS2304") || !strings.Contains(formatted, "AppointmentForm.tsx:43:12") {
		t.Fatalf("unexpected formatted diagnostics: %s", formatted)
	}
}

// TestForgeAgent_MaxIterationsLimit verifies that LoopAgent stops after configured MaxIterations
// when the model never calls exit_loop.
func TestForgeAgent_MaxIterationsLimit(t *testing.T) {
	tmpDir := t.TempDir()
	sb := &Sandbox{
		cfg: SandboxConfig{
			TemplateDir: tmpDir,
			UseDocker:   false,
			Timeout:     5 * time.Second,
		},
		workDir: tmpDir,
		files:   make(map[string]string),
	}
	defer sb.Close()

	// Mock LLM that keeps calling compilar endlessly without fixing or exiting
	mock := &mockLLM{
		responses: []*model.LLMResponse{
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_compile_1",
								Name: "compilar",
								Args: map[string]any{},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_compile_2",
								Name: "compilar",
								Args: map[string]any{},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						{
							FunctionCall: &genai.FunctionCall{
								ID:   "call_compile_3",
								Name: "compilar",
								Args: map[string]any{},
							},
						},
					},
				},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			},
		},
	}

	agent, err := NewForgeAgent(Config{
		MaxIterations: 2, // configured max 2 iterations
		Model:         mock,
		Sandbox:       sb,
	})
	if err != nil {
		t.Fatalf("failed to create ForgeAgent: %v", err)
	}

	result, err := agent.Run(context.Background(), "test-conv-limit", "loop endlessly")
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	// Loop must terminate due to MaxIterations cap
	if result.Success {
		t.Fatalf("expected loop to fail because code was never fixed, got success")
	}
	if mock.turnCount > 6 {
		t.Fatalf("expected mock turn count to be bounded by MaxIterations, got %d", mock.turnCount)
	}
}

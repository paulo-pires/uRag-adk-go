package forge

import (
	"context"
	"fmt"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/exitlooptool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// Config defines the configuration for the Forge LoopAgent.
type Config struct {
	MaxIterations   uint          // Maximum feedback loop iterations (default: 4)
	MaxOutputTokens int32         // Max tokens per LLM call; 0 = use default (16000)
	Model           model.LLM     // OpenaiCompat model adapter calling urag_proxy
	Sandbox         *Sandbox      // Hardened container sandbox
	MCPToolsets     []tool.Toolset // MCP tools (ontology, search)
}

// WriteFilesArgs is the input structure for the escrever_arquivos tool.
type WriteFilesArgs struct {
	Files map[string]string `json:"files"`
}

// WriteFilesResult is the output structure of escrever_arquivos.
type WriteFilesResult struct {
	OK           bool   `json:"ok"`
	WrittenCount int    `json:"written_count"`
	Message      string `json:"message"`
}

// CompileArgs is the input structure for the compilar tool.
type CompileArgs struct{}

// CompileOutput is the output structure of compilar.
type CompileOutput struct {
	OK          bool         `json:"ok"`
	Error       string       `json:"error,omitempty"`
	Errors      []string     `json:"errors,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	DurationMs  int64        `json:"duration_ms"`
	Message     string       `json:"message"`
}

// ForgeResult contains the final output of the agentic loop execution.
type ForgeResult struct {
	Success        bool              `json:"success"`
	TotalTurns     int               `json:"total_turns"`
	Duration       time.Duration     `json:"duration"`
	GeneratedFiles map[string]string `json:"generated_files"`
	OutputFiles    map[string]string `json:"output_files,omitempty"`
	FinalResponse  string            `json:"final_response"`
	LastError      string            `json:"last_error,omitempty"`
	BuildResult    *BuildResult      `json:"build_result,omitempty"`
}

// ForgeAgent encapsulates the ADK LoopAgent and Runner for autonomous application generation.
type ForgeAgent struct {
	cfg     Config
	runner  *runner.Runner
	sandbox *Sandbox
	sessSvc session.Service
}

const ForgeBuilderInstruction = `Você é o Forge Builder, um agente especialista em construir e corrigir aplicações web modernas em React + TypeScript + Vite + Tailwind CSS.

Seu objetivo é gerar ou atualizar uma aplicação funcional completa que compile sem erros.

FLUXO DE TRABALHO OBRIGATÓRIO EM CADA TURNO:
1. Escreva os arquivos necessários chamando a ferramenta 'escrever_arquivos'.
2. Chame a ferramenta 'compilar' para verificar a tipagem TypeScript e o bundle Vite.
3. Se o compilador retornar erros ('ok: false'), analise os diagnósticos e mensagens de erro retornadas, corrija os arquivos problemáticos usando 'escrever_arquivos' e chame 'compilar' novamente.
4. Quando o compilador retornar sucesso ('ok: true'), você DEVE chamar a ferramenta 'exit_loop' para concluir o trabalho com sucesso.

REGRAS DE SEGURANÇA E AMBIENTE:
- O toolchain é fixo e pré-instalado (React 18, Vite, TypeScript, Tailwind CSS, Lucide-React).
- Não adicione dependências arbitrárias fora da allowlist padrão no package.json.
- Sempre garanta imports válidos para componentes como ícones do 'lucide-react' (ex: Calendar, Clock, User, CheckCircle).
`

// NewForgeAgent constructs an ADK LoopAgent with the forge_builder sub-agent and 3 tools.
func NewForgeAgent(cfg Config) (*ForgeAgent, error) {
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 4
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = 16000
	}
	if cfg.Sandbox == nil {
		sb, err := NewSandbox(DefaultSandboxConfig(""))
		if err != nil {
			return nil, fmt.Errorf("failed to create sandbox: %w", err)
		}
		cfg.Sandbox = sb
	}

	// 1. Tool: escrever_arquivos
	writeFilesTool, err := functiontool.New(
		functiontool.Config{
			Name:        "escrever_arquivos",
			Description: "Escreve ou atualiza múltiplos arquivos no workspace do projeto",
		},
		func(ctx adkagent.Context, args WriteFilesArgs) (WriteFilesResult, error) {
			if len(args.Files) == 0 {
				return WriteFilesResult{OK: false, Message: "Nenhum arquivo fornecido"}, nil
			}
			if err := cfg.Sandbox.WriteFiles(args.Files); err != nil {
				return WriteFilesResult{
					OK:      false,
					Message: fmt.Sprintf("Erro ao escrever arquivos: %v", err),
				}, nil
			}
			return WriteFilesResult{
				OK:           true,
				WrittenCount: len(args.Files),
				Message:      fmt.Sprintf("%d arquivos gravados com sucesso", len(args.Files)),
			}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create escrever_arquivos tool: %w", err)
	}

	// 2. Tool: compilar
	compileTool, err := functiontool.New(
		functiontool.Config{
			Name:        "compilar",
			Description: "Executa a compilação (tsc typecheck + vite build) no sandbox Docker",
		},
		func(ctx adkagent.Context, args CompileArgs) (CompileOutput, error) {
			buildRes, err := cfg.Sandbox.Compile(context.Background())
			if err != nil {
				return CompileOutput{
					OK:         false,
					Error:      err.Error(),
					Errors:     []string{err.Error()},
					DurationMs: 0,
					Message:    fmt.Sprintf("Falha na execução do sandbox: %v", err),
				}, nil
			}

			if !buildRes.Success {
				var errStrs []string
				for _, d := range buildRes.Diagnostics {
					if d.Line > 0 {
						errStrs = append(errStrs, fmt.Sprintf("[%s] %s:%d:%d: %s", d.Code, d.File, d.Line, d.Column, d.Message))
					} else {
						errStrs = append(errStrs, fmt.Sprintf("[%s] %s: %s", d.Code, d.File, d.Message))
					}
				}
				if len(errStrs) == 0 && buildRes.Error != "" {
					errStrs = append(errStrs, buildRes.Error)
				}
				return CompileOutput{
					OK:          false,
					Error:       buildRes.Error,
					Errors:      errStrs,
					Diagnostics: buildRes.Diagnostics,
					DurationMs:  buildRes.Duration.Milliseconds(),
					Message:     fmt.Sprintf("Build falhou com %d erros de compilação. Corrija os arquivos e tente novamente.", len(errStrs)),
				}, nil
			}

			return CompileOutput{
				OK:         true,
				DurationMs: buildRes.Duration.Milliseconds(),
				Message:    fmt.Sprintf("Compilação concluída com sucesso em %s! Chame agora a ferramenta exit_loop para finalizar.", buildRes.Duration.Round(time.Millisecond)),
			}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create compilar tool: %w", err)
	}

	// 3. Tool: exitlooptool
	exitTool, err := exitlooptool.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create exitlooptool: %w", err)
	}

	tools := []tool.Tool{writeFilesTool, compileTool, exitTool}

	// Create sub-agent: forge_builder
	builderAgent, err := llmagent.New(llmagent.Config{
		Name:        "forge_builder",
		Model:       cfg.Model,
		Description: "Forge application code builder and compiler feedback fixer",
		Instruction: ForgeBuilderInstruction,
		Tools:       tools,
		Toolsets:    cfg.MCPToolsets,
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: cfg.MaxOutputTokens,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create forge_builder agent: %w", err)
	}

	// Create root agent: LoopAgent
	loopAg, err := loopagent.New(loopagent.Config{
		AgentConfig: adkagent.Config{
			Name:        "forge_loop",
			Description: "LoopAgent that iterates forge_builder until compilation succeeds",
			SubAgents:   []adkagent.Agent{builderAgent},
		},
		MaxIterations: cfg.MaxIterations,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create loopagent: %w", err)
	}

	sessSvc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "urag-forge",
		Agent:          loopAg,
		SessionService: sessSvc,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create runner: %w", err)
	}

	return &ForgeAgent{
		cfg:     cfg,
		runner:  r,
		sandbox: cfg.Sandbox,
		sessSvc: sessSvc,
	}, nil
}

// Run executes the agent loop on a prompt and returns the result.
func (a *ForgeAgent) Run(ctx context.Context, sessionID, prompt string) (*ForgeResult, error) {
	start := time.Now()
	userID := "forge-user"

	if sessionID == "" {
		sessionID = fmt.Sprintf("forge-sess-%d", time.Now().UnixNano())
	}

	_, err := a.sessSvc.Create(ctx, &session.CreateRequest{
		AppName:   "urag-forge",
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		// Session might already exist
	}

	var finalResponse strings.Builder
	turns := 0

	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	for ev, err := range a.runner.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{}) {
		if err != nil {
			return nil, fmt.Errorf("loop agent error: %w", err)
		}
		if ev == nil {
			continue
		}
		turns++
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p.Text != "" {
					finalResponse.WriteString(p.Text)
				}
			}
		}
	}

	duration := time.Since(start)
	buildRes, _ := a.sandbox.Compile(ctx)

	files := a.sandbox.Files()
	outFiles := map[string]string{}
	if buildRes != nil && buildRes.Success {
		outFiles = buildRes.OutputFiles
	}

	success := buildRes != nil && buildRes.Success

	return &ForgeResult{
		Success:        success,
		TotalTurns:     turns,
		Duration:       duration,
		GeneratedFiles: files,
		OutputFiles:    outFiles,
		FinalResponse:  finalResponse.String(),
		BuildResult:    buildRes,
	}, nil
}

// Sandbox returns the active sandbox instance.
func (a *ForgeAgent) Sandbox() *Sandbox {
	return a.sandbox
}

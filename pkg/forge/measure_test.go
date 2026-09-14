package forge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/exitlooptool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"urag-adk-go/model/openaicompat"
)

// ─────────────────────────────────────────────────────────────────────────────
// Resultado de uma única rodada (1 braço × 1 tentativa de conjunto)
// ─────────────────────────────────────────────────────────────────────────────

type runResult struct {
	// Qual iteração do laço compilou (1=1a tentativa, 2=2a, etc.; 0=não compilou em maxIter)
	CompilationIter int
	Success         bool
	Duration        time.Duration
	TokensIn        int64
	TokensOut       int64
}

// ─────────────────────────────────────────────────────────────────────────────
// Prompt inicial quebrado — mesmo para os dois braços
// ─────────────────────────────────────────────────────────────────────────────

const brokenAppTSX = `import React from 'react';

// ERRO INTENCIONAL: Calendar não importado de lucide-react
export function App() {
  return (
    <div className="p-4">
      <h1>Agenda</h1>
      <Calendar className="w-6 h-6" />
    </div>
  );
}

export default App;
`

// O agente recebe este prompt para CORRIGIR o arquivo já quebrado.
// (A intenção é medir se o diagnóstico do compilador ajuda na correção.)
const measurePrompt = `O arquivo src/App.tsx já existe no workspace com um erro de compilação.
Sua tarefa:
1. Chame compilar para ver o erro.
2. Corrija o arquivo usando escrever_arquivos.
3. Chame compilar novamente até obter sucesso.
4. Quando compilar retornar ok:true, chame exit_loop.
`

// ─────────────────────────────────────────────────────────────────────────────
// runBraco executa N rodadas de um braço (com ou sem diagnóstico detalhado)
// ─────────────────────────────────────────────────────────────────────────────

type braco struct {
	nome        string
	comDiag     bool // true = braço A (diagnóstico real); false = braço B (mensagem genérica)
	modelo      *openaicompat.Model
	templateDir string
	maxIter     uint
	httpCounter *int64
	counterMu   *sync.Mutex
}

// intercalarDiagnostico é um wrapper de Sandbox.Compile que, para braço B,
// substitui o diagnóstico real pela mensagem genérica antes de devolver.
type sandboxDiagBraco struct {
	*Sandbox
	comDiag bool
}

func (s *sandboxDiagBraco) Compile(ctx context.Context) (*BuildResult, error) {
	res, err := s.Sandbox.Compile(ctx)
	if err != nil {
		return res, err
	}
	if !res.Success && !s.comDiag {
		// Braço B: apaga o diagnóstico real, substitui por mensagem genérica
		return &BuildResult{
			Success:     false,
			ExitCode:    res.ExitCode,
			RawOutput:   res.RawOutput,
			Error:       "Build failed. Try again.",
			Diagnostics: nil,
			Duration:    res.Duration,
		}, nil
	}
	return res, nil
}

func executarRodada(ctx context.Context, b braco, n int) (runResult, error) {
	// Cada rodada tem seu próprio sandbox (estado limpo)
	sbCfg := DefaultSandboxConfig(b.templateDir)
	sbCfg.UseDocker = true
	rawSb, err := NewSandbox(sbCfg)
	if err != nil {
		return runResult{}, fmt.Errorf("rodada %d: NewSandbox: %w", n, err)
	}
	defer rawSb.Close()

	// Escreve o app quebrado de partida
	if err := rawSb.WriteFiles(map[string]string{
		"src/App.tsx": brokenAppTSX,
	}); err != nil {
		return runResult{}, fmt.Errorf("rodada %d: WriteFiles: %w", n, err)
	}

	sb := &sandboxDiagBraco{Sandbox: rawSb, comDiag: b.comDiag}

	// Acumulador de tokens para medir custo
	acc := &openaicompat.UsageAccumulator{}
	ctx = openaicompat.WithUsageAccumulator(ctx, acc)

	// Contador HTTP (compartilhado por todos no braço)
	compileCallCount := 0

	// Ferramentas
	writeTool, err := functiontool.New(
		functiontool.Config{
			Name:        "escrever_arquivos",
			Description: "Escreve ou atualiza múltiplos arquivos no workspace do projeto",
		},
		func(ctx agent.Context, args WriteFilesArgs) (WriteFilesResult, error) {
			if len(args.Files) == 0 {
				return WriteFilesResult{OK: false, Message: "Nenhum arquivo fornecido"}, nil
			}
			if err := rawSb.WriteFiles(args.Files); err != nil {
				return WriteFilesResult{OK: false, Message: fmt.Sprintf("Erro: %v", err)}, nil
			}
			return WriteFilesResult{OK: true, WrittenCount: len(args.Files),
				Message: fmt.Sprintf("%d arquivos gravados", len(args.Files))}, nil
		},
	)
	if err != nil {
		return runResult{}, fmt.Errorf("rodada %d: writeTool: %w", n, err)
	}

	compileTool, err := functiontool.New(
		functiontool.Config{
			Name:        "compilar",
			Description: "Executa a compilação (tsc + vite build) no sandbox Docker",
		},
		func(ctx agent.Context, args CompileArgs) (CompileOutput, error) {
			compileCallCount++
			buildRes, err := sb.Compile(context.Background())
			if err != nil {
				return CompileOutput{OK: false, Error: err.Error(),
					Errors: []string{err.Error()}, Message: fmt.Sprintf("Erro sandbox: %v", err)}, nil
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
					Message:     fmt.Sprintf("Build falhou. Erros: %v", errStrs),
				}, nil
			}
			return CompileOutput{
				OK:         true,
				DurationMs: buildRes.Duration.Milliseconds(),
				Message:    "Compilação concluída com sucesso! Chame exit_loop.",
			}, nil
		},
	)
	if err != nil {
		return runResult{}, fmt.Errorf("rodada %d: compileTool: %w", n, err)
	}

	exitTool, err := exitlooptool.New()
	if err != nil {
		return runResult{}, fmt.Errorf("rodada %d: exitTool: %w", n, err)
	}

	builderAgent, err := llmagent.New(llmagent.Config{
		Name:        fmt.Sprintf("forge_builder_%s_%d", b.nome, n),
		Model:       b.modelo,
		Description: "Forge builder agent",
		Instruction: ForgeBuilderInstruction,
		Tools:       []tool.Tool{writeTool, compileTool, exitTool},
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: 16000,
		},
	})
	if err != nil {
		return runResult{}, fmt.Errorf("rodada %d: builderAgent: %w", n, err)
	}

	loopAg, err := loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:        fmt.Sprintf("forge_loop_%s_%d", b.nome, n),
			Description: "LoopAgent forge measure",
			SubAgents:   []agent.Agent{builderAgent},
		},
		MaxIterations: b.maxIter,
	})
	if err != nil {
		return runResult{}, fmt.Errorf("rodada %d: loopAg: %w", n, err)
	}

	sessSvc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "urag-forge-measure",
		Agent:          loopAg,
		SessionService: sessSvc,
	})
	if err != nil {
		return runResult{}, fmt.Errorf("rodada %d: runner: %w", n, err)
	}

	sessID := fmt.Sprintf("measure-%s-%d-%d", b.nome, n, time.Now().UnixNano())
	_, _ = sessSvc.Create(ctx, &session.CreateRequest{
		AppName:   "urag-forge-measure",
		UserID:    "measure-user",
		SessionID: sessID,
	})

	start := time.Now()
	msg := genai.NewContentFromText(measurePrompt, genai.RoleUser)
	for _, err := range r.Run(ctx, "measure-user", sessID, msg, agent.RunConfig{}) {
		if err != nil {
			// Erro do laço (ex: modelo retornou vazio) — contamos como falha desta rodada
			break
		}
	}
	dur := time.Since(start)

	// Verifica se compilou ao final
	finalBuild, _ := rawSb.Compile(ctx)
	success := finalBuild != nil && finalBuild.Success

	// compileCallCount aproxima o número de iteração em que compilou com sucesso
	// (cada iteração do loop faz pelo menos 1 chamada a compilar)
	compilationIter := 0
	if success {
		compilationIter = compileCallCount
		if compilationIter == 0 {
			compilationIter = 1
		}
	}

	// Atualiza contador HTTP compartilhado
	b.counterMu.Lock()
	*b.httpCounter += acc.In + acc.Out // tokens como proxy de chamadas (cada call ≥1 turn)
	b.counterMu.Unlock()

	return runResult{
		CompilationIter: compilationIter,
		Success:         success,
		Duration:        dur,
		TokensIn:        acc.In,
		TokensOut:       acc.Out,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// TestForge_MeasureConvergenceWithVsWithoutDiagnostics — MEDIÇÃO REAL
// ─────────────────────────────────────────────────────────────────────────────

func TestForge_MeasureConvergenceWithVsWithoutDiagnostics(t *testing.T) {
	apiKey := os.Getenv("FORGE_AGENT_KEY")
	if apiKey == "" {
		t.Skip("FORGE_AGENT_KEY não definida; skip medição real")
	}

	proxyURL := os.Getenv("FORGE_AGENT_PROXY_URL")
	if proxyURL == "" {
		proxyURL = "http://192.168.0.151:8095/v1"
	}
	modelName := os.Getenv("FORGE_AGENT_MODEL")
	if modelName == "" {
		modelName = "deepseek-v4-flash"
	}
	provider := os.Getenv("FORGE_AGENT_PROVIDER")
	if provider == "" {
		provider = "opencode-go"
	}

	// Verifica que Docker está disponível
	if !isDockerAvailable() {
		t.Skip("Docker não disponível; skip medição que exige sandbox real")
	}

	// Template dir
	templateDir := ""
	for _, c := range []string{
		"../../urag-forge-go/template",
		"/home/usuarioftp/urag-stack/urag-forge-go/template",
		"/home/usuarioftp/urag-stack/poc-forge-agentico/template",
	} {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			templateDir = c
			break
		}
	}
	if templateDir == "" {
		t.Skip("Template dir não encontrado; skip medição")
	}

	const N = 10 // rodadas por braço
	const maxIter = 4

	t.Logf("=== MEDIÇÃO FORGE: com vs sem diagnóstico ===")
	t.Logf("Modelo: %s via %s (provider=%s)", modelName, proxyURL, provider)
	t.Logf("Template: %s", templateDir)
	t.Logf("N=%d rodadas por braço, maxIter=%d", N, maxIter)
	t.Logf("Início: %s", time.Now().UTC().Format(time.RFC3339))

	type bracoResult struct {
		nome       string
		resultados []runResult
		httpTokens int64
	}

	runBraco := func(nome string, comDiag bool) *bracoResult {
		br := &bracoResult{nome: nome}

		httpTokens := int64(0)
		var httpMu sync.Mutex

		modelo := openaicompat.NewWithProvider(proxyURL, modelName, apiKey, provider)

		b := braco{
			nome:        nome,
			comDiag:     comDiag,
			modelo:      modelo,
			templateDir: templateDir,
			maxIter:     maxIter,
			httpCounter: &httpTokens,
			counterMu:   &httpMu,
		}

		for i := 0; i < N; i++ {
			t.Logf("[%s] Rodada %d/%d ...", nome, i+1, N)
			res, err := executarRodada(context.Background(), b, i+1)
			if err != nil {
				t.Logf("[%s] Rodada %d ERRO: %v", nome, i+1, err)
				res = runResult{Success: false, CompilationIter: 0}
			}
			br.resultados = append(br.resultados, res)
			t.Logf("[%s] Rodada %d: sucesso=%v iter=%d dur=%s tokIn=%d tokOut=%d",
				nome, i+1, res.Success, res.CompilationIter, res.Duration.Round(time.Millisecond),
				res.TokensIn, res.TokensOut)
		}

		br.httpTokens = httpTokens
		return br
	}

	t.Logf("\n--- Executando Braço A (com diagnóstico real) ---")
	bracoA := runBraco("A-com-diag", true)

	t.Logf("\n--- Executando Braço B (sem diagnóstico, mensagem genérica) ---")
	bracoB := runBraco("B-sem-diag", false)

	// ── Calcula métricas ──────────────────────────────────────────────────────

	computeMetrics := func(resultados []runResult) (successRate float64, dist map[int]int, failIn4 int, avgDur time.Duration, tokIn, tokOut int64) {
		dist = map[int]int{1: 0, 2: 0, 3: 0, 4: 0}
		var totalDur time.Duration
		for _, r := range resultados {
			if r.Success {
				key := r.CompilationIter
				if key < 1 {
					key = 1
				}
				if key > 4 {
					key = 4
				}
				dist[key]++
			} else {
				failIn4++
			}
			totalDur += r.Duration
			tokIn += r.TokensIn
			tokOut += r.TokensOut
		}
		successRate = float64(len(resultados)-failIn4) / float64(len(resultados))
		if len(resultados) > 0 {
			avgDur = totalDur / time.Duration(len(resultados))
		}
		return
	}

	srA, distA, failA, avgDurA, tokInA, tokOutA := computeMetrics(bracoA.resultados)
	srB, distB, failB, avgDurB, tokInB, tokOutB := computeMetrics(bracoB.resultados)

	// Custo estimado: deepseek-v4-flash ≈ $0.27/M input, $1.10/M output (valores de referência)
	const costPerMIn = 0.27 / 1_000_000
	const costPerMOut = 1.10 / 1_000_000
	costA := float64(tokInA)*costPerMIn + float64(tokOutA)*costPerMOut
	costB := float64(tokInB)*costPerMIn + float64(tokOutB)*costPerMOut

	// ── Reporta ───────────────────────────────────────────────────────────────

	t.Logf("\n══════════════════════════════════════════════════")
	t.Logf("RESULTADOS — %s", time.Now().UTC().Format(time.RFC3339))
	t.Logf("══════════════════════════════════════════════════")
	t.Logf("Modelo: %s | N=%d por braço | maxIter=%d", modelName, N, maxIter)
	t.Logf("")
	t.Logf("BRAÇO A (com diagnóstico real do compilador):")
	t.Logf("  Taxa de sucesso:             %.1f%% (%d/%d)", srA*100, N-failA, N)
	t.Logf("  Falhou em 4 tentativas:      %d (%.1f%%)", failA, float64(failA)/float64(N)*100)
	t.Logf("  Distribuição de iterações:   1=%d 2=%d 3=%d 4=%d", distA[1], distA[2], distA[3], distA[4])
	t.Logf("  Duração média por rodada:    %s", avgDurA.Round(time.Millisecond))
	t.Logf("  Tokens: in=%d out=%d | Custo estimado: $%.5f", tokInA, tokOutA, costA)
	t.Logf("")
	t.Logf("BRAÇO B (sem diagnóstico — 'Build failed. Try again.'):")
	t.Logf("  Taxa de sucesso:             %.1f%% (%d/%d)", srB*100, N-failB, N)
	t.Logf("  Falhou em 4 tentativas:      %d (%.1f%%)", failB, float64(failB)/float64(N)*100)
	t.Logf("  Distribuição de iterações:   1=%d 2=%d 3=%d 4=%d", distB[1], distB[2], distB[3], distB[4])
	t.Logf("  Duração média por rodada:    %s", avgDurB.Round(time.Millisecond))
	t.Logf("  Tokens: in=%d out=%d | Custo estimado: $%.5f", tokInB, tokOutB, costB)
	t.Logf("")

	diff := srA - srB
	switch {
	case diff > 0.10:
		t.Logf("CONCLUSÃO: Diagnóstico ajuda — Braço A %.1f pp melhor que B.", diff*100)
	case diff < -0.10:
		t.Logf("CONCLUSÃO: Surpreendente — Braço B %.1f pp melhor que A (sem diagnóstico).", -diff*100)
	default:
		t.Logf("CONCLUSÃO: sem diferença na TAXA DE SUCESSO (diff=%.1f pp).", diff*100)
		// Empate na taxa de sucesso NÃO significa que o laço está quebrado. Se
		// todas as rodadas dos dois braços convergirem no mesmo número de
		// iterações, o que a medição mostrou é que o CASO DE TESTE não
		// discrimina — e concluir "o laço não funciona" a partir disso é ler o
		// experimento ao contrário. Foi o que a primeira versão fazia.
		if degenerada(distA) && degenerada(distB) {
			t.Logf("  → ATENÇÃO: distribuição degenerada — todas as rodadas dos DOIS braços")
			t.Logf("    convergiram no mesmo número de iterações. O caso de teste não")
			t.Logf("    discrimina: o modelo resolve sem precisar do diagnóstico.")
			t.Logf("    Isto NÃO é evidência de que o laço falhou; é evidência de que o")
			t.Logf("    caso é fácil demais. Use um erro que não se veja relendo o arquivo.")
		} else {
			t.Logf("  → O diagnóstico não mudou o acerto neste caso. Compare o custo abaixo.")
		}
	}
	// O custo é a outra metade da resposta, e some se só se olhar acerto.
	if tokInA > 0 && tokOutA > 0 {
		t.Logf("CUSTO: B gasta %.0f%% mais tokens de entrada e %.0f%% mais de saída que A.",
			100*(float64(tokInB)/float64(tokInA)-1), 100*(float64(tokOutB)/float64(tokOutA)-1))
	}
	t.Logf("══════════════════════════════════════════════════")

	// ── Grava MEDICAO.md ──────────────────────────────────────────────────────

	report := buildMedicaoReport(modelName, proxyURL, N, maxIter,
		srA, distA, failA, avgDurA, tokInA, tokOutA, costA,
		srB, distB, failB, avgDurB, tokInB, tokOutB, costB,
		bracoA.resultados, bracoB.resultados)

	medicaoPath := "MEDICAO.md"
	if err := os.WriteFile(medicaoPath, []byte(report), 0644); err != nil {
		t.Logf("AVISO: não conseguiu gravar MEDICAO.md: %v", err)
	} else {
		t.Logf("MEDICAO.md gravado em %s", medicaoPath)
	}
}

func buildMedicaoReport(
	modelName, proxyURL string,
	N, maxIter int,
	srA float64, distA map[int]int, failA int, avgDurA time.Duration, tokInA, tokOutA int64, costA float64,
	srB float64, distB map[int]int, failB int, avgDurB time.Duration, tokInB, tokOutB int64, costB float64,
	resultsA, resultsB []runResult,
) string {
	now := time.Now().UTC().Format(time.RFC3339)
	diff := srA - srB

	var conclusao string
	switch {
	case diff > 0.10:
		conclusao = fmt.Sprintf("Diagnóstico ajuda: Braço A %.1f pp melhor que B.", diff*100)
	case diff < -0.10:
		conclusao = fmt.Sprintf("Surpreendente: Braço B %.1f pp melhor que A (sem diagnóstico).", -diff*100)
	default:
		conclusao = fmt.Sprintf("**SEM DIFERENÇA SIGNIFICATIVA** (diff=%.1f pp). "+
			"O laço agêntico pode não estar funcionando como esperado.", diff*100)
	}

	// Linha por linha dos resultados individuais
	var rowsA, rowsB strings.Builder
	for i, r := range resultsA {
		rowsA.WriteString(fmt.Sprintf("| %d | %v | %d | %s | %d | %d |\n",
			i+1, r.Success, r.CompilationIter, r.Duration.Round(time.Millisecond), r.TokensIn, r.TokensOut))
	}
	for i, r := range resultsB {
		rowsB.WriteString(fmt.Sprintf("| %d | %v | %d | %s | %d | %d |\n",
			i+1, r.Success, r.CompilationIter, r.Duration.Round(time.Millisecond), r.TokensIn, r.TokensOut))
	}

	var sb strings.Builder
	sb.WriteString("# MEDICAO.md — Forge Agentic Loop: Com vs Sem Diagnostico\n\n")
	sb.WriteString(fmt.Sprintf("**Data:** %s\n", now))
	sb.WriteString(fmt.Sprintf("**Modelo:** %s\n", modelName))
	sb.WriteString(fmt.Sprintf("**Proxy:** %s\n", proxyURL))
	sb.WriteString(fmt.Sprintf("**N por braco:** %d\n", N))
	sb.WriteString(fmt.Sprintf("**MaxIteracoes:** %d\n", maxIter))
	sb.WriteString("**Sandbox:** Docker real (UseDocker=true), build tsc+vite real\n\n---\n\n")
	sb.WriteString("## Resumo\n\n")
	sb.WriteString("| Metrica | Braco A (com diagnostico) | Braco B (sem diagnostico) |\n")
	sb.WriteString("|---|---|---|\n")
	sb.WriteString(fmt.Sprintf("| Taxa de sucesso | %.1f%% (%d/%d) | %.1f%% (%d/%d) |\n",
		srA*100, N-failA, N, srB*100, N-failB, N))
	sb.WriteString(fmt.Sprintf("| Falhou em %d iter | %d (%.1f%%) | %d (%.1f%%) |\n",
		maxIter, failA, float64(failA)/float64(N)*100, failB, float64(failB)/float64(N)*100))
	sb.WriteString(fmt.Sprintf("| Dist. iter 1/2/3/4 | %d/%d/%d/%d | %d/%d/%d/%d |\n",
		distA[1], distA[2], distA[3], distA[4],
		distB[1], distB[2], distB[3], distB[4]))
	sb.WriteString(fmt.Sprintf("| Duracao media | %s | %s |\n",
		avgDurA.Round(time.Millisecond), avgDurB.Round(time.Millisecond)))
	sb.WriteString(fmt.Sprintf("| Tokens in/out | %d/%d | %d/%d |\n",
		tokInA, tokOutA, tokInB, tokOutB))
	sb.WriteString(fmt.Sprintf("| Custo estimado USD | $%.5f | $%.5f |\n\n",
		costA, costB))
	sb.WriteString(fmt.Sprintf("**Conclusao:** %s\n\n---\n\n", conclusao))
	sb.WriteString("## Braco A -- Com diagnostico real (arquivo, linha, codigo TS)\n\n")
	sb.WriteString("| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	sb.WriteString(rowsA.String())
	sb.WriteString("\n## Braco B -- Sem diagnostico (\"Build failed. Try again.\")\n\n")
	sb.WriteString("| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |\n")
	sb.WriteString("|---|---|---|---|---|---|\n")
	sb.WriteString(rowsB.String())
	sb.WriteString("\n---\n\n## Notas metodologicas\n\n")
	sb.WriteString("- Ambos os bracos partem do mesmo app quebrado: src/App.tsx com Calendar nao importado.\n")
	sb.WriteString("- O prompt inicial e identico; so o retorno da ferramenta compilar difere entre os bracos.\n")
	sb.WriteString("- Braco A: compilar devolve diagnosticos estruturados (arquivo, linha, codigo TS2304, mensagem).\n")
	sb.WriteString("- Braco B: compilar devolve apenas \"Build failed. Try again.\" -- sem arquivo, sem linha, sem simbolo.\n")
	sb.WriteString("- \"Iter compilou\" = numero da chamada a compilar quando retornou ok:true. 0 = nao compilou.\n")
	sb.WriteString("- Custo estimado com tarifa deepseek-v4-flash: $0.27/M input, $1.10/M output.\n")
	sb.WriteString("- Se nao houver diferenca entre bracos, esse e o resultado principal.\n")
	return sb.String()
}

// degenerada diz se TODAS as rodadas caíram no mesmo número de iterações. Uma
// distribuição assim não tem variância, e comparar dois braços degenerados não
// distingue nada: o experimento mediu a facilidade do caso, não o efeito da
// condição testada.
func degenerada(dist map[int]int) bool {
	naoVazios := 0
	for _, n := range dist {
		if n > 0 {
			naoVazios++
		}
	}
	return naoVazios <= 1
}

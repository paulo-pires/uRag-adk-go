// uRag ADK agent — substituto drop-in do uRag-agent-go.
// Expõe MCP real (mcp.NewStreamableHTTPHandler) em :8081
// com a tool agent_ask(question, system_prompt, session_id, user_id).
//
// Env vars:
//
//	LM_STUDIO_URL         (default http://localhost:1234/v1)
//	LM_STUDIO_MODEL       (default gemma-4-e2b-it)
//	URAG_MCP_URL          URL do uRag-go MCP (default http://localhost:8080)
//	URAG_RAG_TOKEN        token de autenticação do uRag-go (opcional)
//	URAG_VECTOR_MEMORY    se "true", usa uRag-go como backend semântico de memória (D2)
//	GUARD_URL             uRag-guard-go — vazio = reporte desligado
//	GUARD_INGEST_TOKEN
//	MEMORY_DIR            diretório para memória por keyword (fallback se URAG_VECTOR_MEMORY não definido)
//	AGENT_HTTP_ADDR       (default :8081)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	sessiondb "google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"urag-adk-go/internal/guard"
	filemem "urag-adk-go/internal/memory"
	"urag-adk-go/model/openaicompat"
)

// ── span buffer: accumulates tool-call spans during r.Run() ──────────────────

type spanEntry struct {
	toolName  string
	args      map[string]any
	result    map[string]any
	startedAt time.Time
	endedAt   time.Time
	errStr    string
}

type spanBuffer struct {
	mu     sync.Mutex
	starts map[string]time.Time // key: FunctionCallID
	spans  []spanEntry
}

func newSpanBuffer() *spanBuffer {
	return &spanBuffer{starts: make(map[string]time.Time)}
}

type spanBufKey struct{}

func withSpanBuffer(ctx context.Context, sb *spanBuffer) context.Context {
	return context.WithValue(ctx, spanBufKey{}, sb)
}

func spanBufFromCtx(ctx context.Context) *spanBuffer {
	sb, _ := ctx.Value(spanBufKey{}).(*spanBuffer)
	return sb
}

type askArgs struct {
	Question     string   `json:"question"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	UserID       string   `json:"user_id,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"` // nil = model default; 0.0 = determinístico (pesquisa)
}

type askOut struct {
	Answer    string `json:"answer"`
	SessionID string `json:"session_id"`
}

// buildMemoryService retorna o backend de memória de acordo com as env vars:
//
//	URAG_VECTOR_MEMORY=true → VectorMemoryService (semântico via uRag-go MCP) — D2
//	MEMORY_DIR definido     → FileService (keyword matching, legado)
//	sem config              → InMemoryService do ADK (sem persistência)
func buildMemoryService(mcpURL string) adkmemory.Service {
	if os.Getenv("URAG_VECTOR_MEMORY") == "true" {
		log.Printf("  Memory: VectorMemoryService (semântico) → %s", mcpURL)
		return filemem.NewVectorService(mcpURL, os.Getenv("URAG_RAG_TOKEN"))
	}
	if dir := os.Getenv("MEMORY_DIR"); dir != "" {
		log.Printf("  Memory: FileService (keyword) → %s", dir)
		return filemem.New(dir)
	}
	log.Printf("  Memory: InMemory (sem persistência)")
	return adkmemory.InMemoryService()
}

// buildSessionService retorna Postgres (SESSION_DSN definido) ou in-memory.
func buildSessionService() session.Service {
	dsn := os.Getenv("SESSION_DSN")
	if dsn == "" {
		return session.InMemoryService()
	}
	svc, err := sessiondb.NewSessionService(
		postgres.Open(dsn),
		&gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Silent),
			NamingStrategy: schema.NamingStrategy{
				TablePrefix: "adk_",
			},
		},
	)
	if err != nil {
		log.Fatalf("session db: %v", err)
	}
	if err := sessiondb.AutoMigrate(svc); err != nil {
		log.Fatalf("session db migrate: %v", err)
	}
	log.Printf("  Sessions: Postgres (%s)", maskDSN(dsn))
	return svc
}

func maskDSN(dsn string) string {
	// exibe só host:port/db — oculta credenciais no log
	if i := strings.Index(dsn, "@"); i >= 0 {
		return dsn[i+1:]
	}
	return dsn
}

func main() {
	llm := openaicompat.New(
		getenv("LM_STUDIO_URL", "http://localhost:1234/v1"),
		getenv("LM_STUDIO_MODEL", "gemma-4-e2b-it"),
	)

	mcpURL := getenv("URAG_MCP_URL", "http://localhost:8080")
	mcpToolSet, err := mcptoolset.New(mcptoolset.Config{
		Transport: &mcp.StreamableClientTransport{
			Endpoint: mcpURL,
		},
	})
	if err != nil {
		log.Fatalf("mcptoolset: %v", err)
	}

	// Science MCP server (RDKit / DeepChem) — opcional; ativo se SCIENCE_MCP_URL estiver definido.
	toolsets := []tool.Toolset{mcpToolSet}
	if sciURL := os.Getenv("SCIENCE_MCP_URL"); sciURL != "" {
		sciToolSet, err := mcptoolset.New(mcptoolset.Config{
			Transport: &mcp.StreamableClientTransport{Endpoint: sciURL},
		})
		if err != nil {
			log.Fatalf("science mcptoolset: %v", err)
		}
		toolsets = append(toolsets, sciToolSet)
		log.Printf("  Science MCP: %s", sciURL)
	}

	sessionSvc := buildSessionService()
	memSvc := buildMemoryService(mcpURL)
	gc := guard.New(os.Getenv("GUARD_URL"), os.Getenv("GUARD_INGEST_TOKEN"))

	// sessions mapeia session_id externo (caller) → session_id interno do ADK.
	var sessions sync.Map

	// runnerCache: runners reutilizáveis por valor de temperature.
	// Na prática há no máximo 2-3 chaves (nil, 0.0, 0.7…).
	// Callbacks lêem spanbuf e instruction do context — sem estado per-call no runner.
	var runnerCache sync.Map // string → *runner.Runner

	buildOrGetRunner := func(temperature *float64) (*runner.Runner, error) {
		key := "nil"
		if temperature != nil {
			key = fmt.Sprintf("%g", *temperature)
		}
		if cached, ok := runnerCache.Load(key); ok {
			return cached.(*runner.Runner), nil
		}

		var genCfg *genai.GenerateContentConfig
		if temperature != nil {
			t := float32(*temperature)
			genCfg = &genai.GenerateContentConfig{Temperature: &t}
		}

		a, err := llmagent.New(llmagent.Config{
			Name:                  "urag_adk_agent",
			Model:                 llm,
			Description:           "uRag ADK agent.",
			Instruction:           "You are a helpful assistant with access to uRag knowledge base tools. Use them to answer questions.",
			Toolsets:              toolsets,
			GenerateContentConfig: genCfg,
			BeforeToolCallbacks: []llmagent.BeforeToolCallback{
				func(ctx adkagent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
					if sb := spanBufFromCtx(ctx); sb != nil {
						sb.mu.Lock()
						sb.starts[ctx.FunctionCallID()] = time.Now()
						sb.mu.Unlock()
					}
					return nil, nil
				},
			},
			AfterToolCallbacks: []llmagent.AfterToolCallback{
				func(ctx adkagent.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
					if sb := spanBufFromCtx(ctx); sb != nil {
						sb.mu.Lock()
						start := sb.starts[ctx.FunctionCallID()]
						delete(sb.starts, ctx.FunctionCallID())
						errStr := ""
						if err != nil {
							errStr = err.Error()
						}
						sb.spans = append(sb.spans, spanEntry{
							toolName:  t.Name(),
							args:      args,
							result:    result,
							startedAt: start,
							endedAt:   time.Now(),
							errStr:    errStr,
						})
						sb.mu.Unlock()
					}
					return nil, nil
				},
			},
			OnToolErrorCallbacks: []llmagent.OnToolErrorCallback{
				func(ctx adkagent.Context, t tool.Tool, args map[string]any, err error) (map[string]any, error) {
					if sb := spanBufFromCtx(ctx); sb != nil {
						sb.mu.Lock()
						start := sb.starts[ctx.FunctionCallID()]
						delete(sb.starts, ctx.FunctionCallID())
						sb.spans = append(sb.spans, spanEntry{
							toolName:  t.Name(),
							args:      args,
							startedAt: start,
							endedAt:   time.Now(),
							errStr:    err.Error(),
						})
						sb.mu.Unlock()
					}
					return nil, nil
				},
			},
		})
		if err != nil {
			return nil, err
		}
		r, err := runner.New(runner.Config{
			AppName:        "urag-adk",
			Agent:          a,
			SessionService: sessionSvc,
			MemoryService:  memSvc,
		})
		if err != nil {
			return nil, err
		}
		runnerCache.Store(key, r)
		return r, nil
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "urag-adk-agent", Version: "v0.1.0"}, nil)

	mcp.AddTool(srv,
		&mcp.Tool{
			Name:        "agent_ask",
			Description: "Executa o loop ReAct ADK com as tools do uRag-go e devolve a resposta.",
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, args askArgs) (*mcp.CallToolResult, askOut, error) {
			startedAt := time.Now()

			// ── guardrail: input ──────────────────────────────────────────────
			inputRules := gc.FetchRules("urag-adk-agent", "input")
			inputViolations := checkRules(ctx, inputRules, args.Question, "input")
			for _, v := range inputViolations {
				if v.rule.Action == "block" {
					blockedID := gc.PushRun(guard.RunInput{
						Source:    "urag-adk-agent",
						Name:      "agent_ask",
						Question:  args.Question,
						Answer:    "[bloqueado por guardrail: " + v.rule.Name + "]",
						Status:    "blocked",
						StartedAt: startedAt,
						EndedAt:   time.Now(),
						UserID:    args.UserID,
						Model:     getenv("LM_STUDIO_MODEL", "gemma-4-e2b-it"),
						Provider:  "lmstudio",
					})
					gc.PushGuardrailEvent(blockedID, v.rule.ID, v.rule.Name, "input", "block", v.snippet)
					return nil, askOut{}, fmt.Errorf("guardrail block (input): %s", v.rule.Name)
				}
			}

			// ── sessão: reutiliza existente (multi-turn) ou cria nova ─────────
			userID := "workflow"
			if args.UserID != "" {
				userID = args.UserID
			}
			var adkSessionID string
			if args.SessionID != "" {
				if id, ok := sessions.Load(args.SessionID); ok {
					adkSessionID = id.(string)
				}
			}
			if adkSessionID == "" {
				sess, err := sessionSvc.Create(ctx, &session.CreateRequest{
					AppName: "urag-adk", UserID: userID,
				})
				if err != nil {
					return nil, askOut{}, err
				}
				adkSessionID = sess.Session.ID()
				if args.SessionID != "" {
					sessions.Store(args.SessionID, adkSessionID)
				}
			}
			returnSessionID := args.SessionID
			if returnSessionID == "" {
				returnSessionID = adkSessionID
			}

			// ── run LLM com acumulador de tokens e span buffer ────────────────
			// system_prompt e memória do usuário são injetados como prefixo da
			// mensagem — o runner é reutilizado entre calls (P1 fix).
			var msgPrefix strings.Builder
			if args.SystemPrompt != "" {
				msgPrefix.WriteString("<system>\n")
				msgPrefix.WriteString(args.SystemPrompt)
				msgPrefix.WriteString("\n</system>\n")
			}
			if args.UserID != "" {
				memResp, _ := memSvc.SearchMemory(ctx, &adkmemory.SearchRequest{
					AppName: "urag-adk",
					UserID:  userID,
					Query:   args.Question,
				})
				if len(memResp.Memories) > 0 {
					msgPrefix.WriteString("<past_context>\n")
					for _, m := range memResp.Memories {
						for _, p := range m.Content.Parts {
							if p.Text != "" {
								msgPrefix.WriteString(p.Text)
								msgPrefix.WriteString("\n---\n")
							}
						}
					}
					msgPrefix.WriteString("</past_context>\n")
				}
			}
			question := args.Question
			if msgPrefix.Len() > 0 {
				question = msgPrefix.String() + question
			}

			spanbuf := newSpanBuffer()
			r, err := buildOrGetRunner(args.Temperature)
			if err != nil {
				return nil, askOut{}, err
			}
			acc := &openaicompat.UsageAccumulator{}
			ctx = openaicompat.WithUsageAccumulator(ctx, acc)
			ctx = withSpanBuffer(ctx, spanbuf)

			var answerBuf, toolCtxBuf strings.Builder
			msg := genai.NewContentFromText(question, genai.RoleUser)
			for ev, err := range r.Run(ctx, userID, adkSessionID, msg, adkagent.RunConfig{}) {
				if err != nil {
					return nil, askOut{}, err
				}
				if ev == nil || ev.Content == nil {
					continue
				}
				if ev.IsFinalResponse() {
					for _, p := range ev.Content.Parts {
						answerBuf.WriteString(p.Text)
					}
				} else {
					for _, p := range ev.Content.Parts {
						if p.FunctionResponse != nil {
							if raw, err := json.Marshal(p.FunctionResponse.Response); err == nil {
								if toolCtxBuf.Len() > 0 {
									toolCtxBuf.WriteByte('\n')
								}
								toolCtxBuf.Write(raw)
							}
						}
					}
				}
			}
			answer := answerBuf.String()
			toolContext := toolCtxBuf.String()

			// ── persistir memória cross-session ───────────────────────────────
			go func() {
				resp, err := sessionSvc.Get(context.Background(), &session.GetRequest{
					AppName:   "urag-adk",
					UserID:    userID,
					SessionID: adkSessionID,
				})
				if err == nil {
					memSvc.AddSessionToMemory(context.Background(), resp.Session) //nolint:errcheck
				}
			}()

			// ── guardrail: output ─────────────────────────────────────────────
			outputRules := gc.FetchRules("urag-adk-agent", "output")
			outputViolations := checkRules(ctx, outputRules, answer, "output")

			status := "success"
			for _, v := range outputViolations {
				if v.rule.Action == "block" {
					status = "blocked"
					break
				}
			}

			// ── reportar run ao guard (síncrono → captura run_id para eventos) ─
			runID := gc.PushRun(guard.RunInput{
				Source:    "urag-adk-agent",
				Name:      "agent_ask",
				Question:  args.Question,
				Answer:    answer,
				Status:    status,
				StartedAt: startedAt,
				EndedAt:   time.Now(),
				UserID:    args.UserID,
				Model:     getenv("LM_STUDIO_MODEL", "gemma-4-e2b-it"),
				Provider:  "lmstudio",
				TokensIn:  acc.In,
				TokensOut: acc.Out,
			})

			// ── registrar eventos de guardrail ────────────────────────────────
			for _, v := range inputViolations { // flags que não bloquearam
				gc.PushGuardrailEvent(runID, v.rule.ID, v.rule.Name, "input", "flag", v.snippet)
			}
			for _, v := range outputViolations {
				gc.PushGuardrailEvent(runID, v.rule.ID, v.rule.Name, "output", v.rule.Action, v.snippet)
			}

			// ── spans de tool calls (batch: uma goroutine por run) ───────────
			if runID != "" {
				spanbuf.mu.Lock()
				spans := spanbuf.spans
				spanbuf.mu.Unlock()
				if len(spans) > 0 {
					go func() {
						for _, sp := range spans {
							gc.PushSpan(guard.SpanInput{
								RunID:     runID,
								ToolName:  sp.toolName,
								Args:      sp.args,
								Result:    sp.result,
								StartedAt: sp.startedAt,
								EndedAt:   sp.endedAt,
								Error:     sp.errStr,
							})
						}
					}()
				}
			}

			// ── eval em background ────────────────────────────────────────────
			if runID != "" {
				go runEvals(context.Background(), gc, llm, runID, args.Question, answer, toolContext)
			}

			// bloquear output (run já logada para auditoria)
			for _, v := range outputViolations {
				if v.rule.Action == "block" {
					return nil, askOut{}, fmt.Errorf("guardrail block (output): %s", v.rule.Name)
				}
			}

			return nil, askOut{Answer: answer, SessionID: returnSessionID}, nil
		},
	)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	addr := getenv("AGENT_HTTP_ADDR", ":8081")
	log.Printf("uRag ADK agent (MCP) ouvindo em %s", addr)
	log.Printf("  LLM:   %s @ %s", getenv("LM_STUDIO_MODEL", "gemma-4-e2b-it"), getenv("LM_STUDIO_URL", "http://localhost:1234/v1"))
	log.Printf("  MCP:   %s", getenv("URAG_MCP_URL", "http://localhost:8080"))
	if u := os.Getenv("GUARD_URL"); u != "" {
		log.Printf("  Guard: %s", u)
	}
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ── guardrail engine ──────────────────────────────────────────────────────────

type ruleViolation struct {
	rule    guard.GuardrailRule
	snippet string
}

func checkRules(ctx context.Context, rules []guard.GuardrailRule, text, stage string) []ruleViolation {
	var out []ruleViolation
	for _, rule := range rules {
		snippet, matched := applyRule(ctx, rule, text, stage)
		if matched {
			out = append(out, ruleViolation{rule: rule, snippet: snippet})
		}
	}
	return out
}

func applyRule(ctx context.Context, rule guard.GuardrailRule, text, stage string) (snippet string, matched bool) {
	switch rule.Type {
	case "regex":
		var cfg struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(rule.Config, &cfg) != nil || cfg.Pattern == "" {
			return "", false
		}
		re, err := regexp.Compile(cfg.Pattern)
		if err != nil {
			return "", false
		}
		m := re.FindString(text)
		return m, m != ""

	case "keyword":
		var cfg struct {
			Keywords []string `json:"keywords"`
		}
		if json.Unmarshal(rule.Config, &cfg) != nil {
			return "", false
		}
		lower := strings.ToLower(text)
		for _, kw := range cfg.Keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return kw, true
			}
		}
		return "", false

	case "external_webhook":
		var cfg struct {
			Endpoint  string `json:"endpoint"`
			TimeoutMs int    `json:"timeout_ms"`
		}
		if json.Unmarshal(rule.Config, &cfg) != nil || cfg.Endpoint == "" {
			return "", false
		}
		timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		payload, _ := json.Marshal(map[string]any{"text": text, "stage": stage})
		wCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(wCtx, http.MethodPost, cfg.Endpoint, bytes.NewReader(payload))
		if err != nil {
			return "", false
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", false
		}
		defer resp.Body.Close()
		var result struct {
			Verdict string `json:"verdict"` // "pass" | "flag" | "block"
		}
		json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
		if result.Verdict == "" || result.Verdict == "pass" {
			return "", false
		}
		return result.Verdict, true
	}
	return "", false
}

// ── eval ──────────────────────────────────────────────────────────────────────

const faithfulnessPrompt = `Rate from 0 to 10 how well grounded the answer is in the provided context.
Only base your rating on whether every claim in the answer is supported by the context.
Context: {{context}}
Question: {{question}}
Answer: {{answer}}
Score (0-10):`

func runEvals(ctx context.Context, gc *guard.Client, llm *openaicompat.Model, runID, question, answer, toolContext string) {
	configs := gc.FetchEvalConfigs("urag-adk-agent")
	for _, cfg := range configs {
		if rand.Float64() > cfg.SampleRate {
			continue
		}
		var promptTpl string
		switch cfg.Metric {
		case "faithfulness":
			promptTpl = faithfulnessPrompt
		case "custom":
			var c struct {
				PromptTemplate string `json:"prompt_template"`
			}
			if json.Unmarshal(cfg.Config, &c) != nil || c.PromptTemplate == "" {
				continue
			}
			promptTpl = c.PromptTemplate
		default:
			continue
		}
		prompt := strings.NewReplacer(
			"{{question}}", question,
			"{{answer}}", answer,
			"{{context}}", toolContext,
		).Replace(promptTpl)
		raw, err := llm.Generate(ctx, prompt)
		if err != nil {
			continue
		}
		gc.PushScore(runID, cfg.ID, cfg.Name, parseScore(raw))
	}
}

func parseScore(s string) float64 {
	var sb strings.Builder
	seenDigit := false
	for _, r := range s {
		if r >= '0' && r <= '9' || (r == '.' && sb.Len() > 0) {
			sb.WriteRune(r)
			seenDigit = true
			continue
		}
		if seenDigit {
			break
		}
	}
	f, err := strconv.ParseFloat(sb.String(), 64)
	if err != nil {
		return 0
	}
	if f < 0 {
		f = 0
	}
	if f > 10 {
		f = 10
	}
	return f / 10
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// uRag ADK agent — substituto drop-in do uRag-agent-go.
// Expõe o mesmo handler JSON-RPC manual em POST /,
// respondendo initialize / tools/list / tools/call → agent_ask.
//
// Env vars:
//
//	LM_STUDIO_URL         (default http://localhost:1234/v1)
//	LM_STUDIO_MODEL       (default gemma-4-e2b-it)
//	URAG_MCP_URL          URL do uRag-go MCP (default http://localhost:8080)
//	GUARD_URL             uRag-guard-go — vazio = reporte desligado
//	GUARD_INGEST_TOKEN
//	AGENT_HTTP_ADDR       (default :8081)
package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"urag-adk-go/internal/guard"
	"urag-adk-go/model/openaicompat"
)

func main() {
	llm := openaicompat.New(
		getenv("LM_STUDIO_URL", "http://localhost:1234/v1"),
		getenv("LM_STUDIO_MODEL", "gemma-4-e2b-it"),
	)

	mcpToolSet, err := mcptoolset.New(mcptoolset.Config{
		Transport: &mcp.StreamableClientTransport{
			Endpoint: getenv("URAG_MCP_URL", "http://localhost:8080"),
		},
	})
	if err != nil {
		log.Fatalf("mcptoolset: %v", err)
	}

	sessionSvc := session.InMemoryService()
	gc := guard.New(os.Getenv("GUARD_URL"), os.Getenv("GUARD_INGEST_TOKEN"))

	// buildRunner por chamada porque system_prompt varia por node do workflow.
	buildRunner := func(systemPrompt string) (*runner.Runner, error) {
		instruction := "You are a helpful assistant with access to uRag knowledge base tools. Use them to answer questions."
		if systemPrompt != "" {
			instruction = systemPrompt
		}
		a, err := llmagent.New(llmagent.Config{
			Name:        "urag_adk_agent",
			Model:       llm,
			Description: "uRag ADK agent.",
			Instruction: instruction,
			Toolsets:    []tool.Toolset{mcpToolSet},
		})
		if err != nil {
			return nil, err
		}
		return runner.New(runner.Config{
			AppName:        "urag-adk",
			Agent:          a,
			SessionService: sessionSvc,
		})
	}

	ask := func(ctx context.Context, question, systemPrompt string) (string, error) {
		r, err := buildRunner(systemPrompt)
		if err != nil {
			return "", err
		}
		sess, err := sessionSvc.Create(ctx, &session.CreateRequest{
			AppName: "urag-adk", UserID: "workflow",
		})
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		msg := genai.NewContentFromText(question, genai.RoleUser)
		for ev, err := range r.Run(ctx, "workflow", sess.Session.ID(), msg, agent.RunConfig{}) {
			if err != nil {
				return "", err
			}
			if ev == nil || ev.Content == nil || !ev.IsFinalResponse() {
				continue
			}
			for _, p := range ev.Content.Parts {
				sb.WriteString(p.Text)
			}
		}
		return sb.String(), nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", mcpHandler(ask, gc))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	addr := getenv("AGENT_HTTP_ADDR", ":8081")
	log.Printf("uRag ADK agent ouvindo em %s", addr)
	log.Printf("  LLM:   %s @ %s", getenv("LM_STUDIO_MODEL", "gemma-4-e2b-it"), getenv("LM_STUDIO_URL", "http://localhost:1234/v1"))
	log.Printf("  MCP:   %s", getenv("URAG_MCP_URL", "http://localhost:8080"))
	if u := os.Getenv("GUARD_URL"); u != "" {
		log.Printf("  Guard: %s", u)
	}
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ── JSON-RPC handler (mesmo protocolo do uRag-agent-go original) ─────────────

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type askFn func(ctx context.Context, question, systemPrompt string) (string, error)

func mcpHandler(ask askFn, gc *guard.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "método não permitido", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

		var req rpcReq
		if err := json.Unmarshal(body, &req); err != nil {
			writeRPC(w, nil, nil, &rpcError{-32700, "JSON inválido"})
			return
		}

		switch req.Method {
		case "initialize":
			writeRPC(w, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "urag-adk-agent", "version": "0.1.0"},
			}, nil)

		case "tools/list":
			writeRPC(w, req.ID, map[string]any{
				"tools": []any{map[string]any{
					"name":        "agent_ask",
					"description": "Executa o loop ReAct ADK para responder uma pergunta usando o uRag-go como motor RAG",
					"inputSchema": map[string]any{
						"type":     "object",
						"required": []string{"question"},
						"properties": map[string]any{
							"question":      map[string]any{"type": "string"},
							"system_prompt": map[string]any{"type": "string"},
						},
					},
				}},
			}, nil)

		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeRPC(w, req.ID, nil, &rpcError{-32602, "parâmetros inválidos"})
				return
			}
			if params.Name != "agent_ask" {
				writeRPC(w, req.ID, nil, &rpcError{-32602, "ferramenta desconhecida: " + params.Name})
				return
			}
			question, _ := params.Arguments["question"].(string)
			systemPrompt, _ := params.Arguments["system_prompt"].(string)
			if question == "" {
				writeRPC(w, req.ID, nil, &rpcError{-32602, "campo 'question' é obrigatório"})
				return
			}

			startedAt := time.Now()
			answer, err := ask(r.Context(), question, systemPrompt)
			if err != nil {
				writeRPC(w, req.ID, nil, &rpcError{-32000, err.Error()})
				return
			}

			go gc.PushRun(guard.RunInput{
				Source:    "urag-adk-agent",
				Name:      "agent_ask",
				Question:  question,
				Answer:    answer,
				Status:    "success",
				StartedAt: startedAt,
				EndedAt:   time.Now(),
			})

			writeRPC(w, req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": answer}},
			}, nil)

		default:
			writeRPC(w, req.ID, nil, &rpcError{-32601, "método desconhecido: " + req.Method})
		}
	}
}

func writeRPC(w http.ResponseWriter, id, result any, e *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: id, Result: result, Error: e})
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

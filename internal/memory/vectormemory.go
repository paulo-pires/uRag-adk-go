// Package memory — VectorMemoryService usa o uRag-go MCP (vector_add/vector_query)
// como backend de memória semântica, substituindo o keyword matching do FileService.
//
// Namespace por usuário: meta["mem_app"] + meta["mem_user"]
// Fallback: se o uRag-go estiver offline, log de aviso e retorna vazio (não quebra o agente).
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"google.golang.org/genai"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
)

// VectorMemoryService implementa adkmemory.Service usando o uRag-go MCP
// como backend vetorial com isolamento lógico por app+user via meta.
type VectorMemoryService struct {
	mcpURL   string
	ragToken string
	client   *http.Client
}

// NewVectorService cria um VectorMemoryService apontando para o MCP do uRag-go.
// ragToken pode ser vazio se o uRag-go não exigir autenticação (env URAG_RAG_TOKEN).
func NewVectorService(mcpURL, ragToken string) *VectorMemoryService {
	return &VectorMemoryService{
		mcpURL:   strings.TrimRight(mcpURL, "/"),
		ragToken: ragToken,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// ── MCP call helpers ──────────────────────────────────────────────────────────

type mcpRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type mcpResponse struct {
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (v *VectorMemoryService) callTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
	req := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  map[string]any{"name": toolName, "arguments": args},
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, v.mcpURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if v.ragToken != "" {
		httpReq.Header.Set("X-RAG-Token", v.ragToken)
	}
	resp, err := v.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("uRag-go offline: %w", err)
	}
	defer resp.Body.Close()

	var mcpResp mcpResponse
	if err := json.NewDecoder(resp.Body).Decode(&mcpResp); err != nil {
		return "", err
	}
	if mcpResp.Error != nil {
		return "", fmt.Errorf("mcp error: %s", mcpResp.Error.Message)
	}
	if mcpResp.Result == nil || mcpResp.Result.IsError {
		return "", fmt.Errorf("mcp tool error")
	}
	var sb strings.Builder
	for _, c := range mcpResp.Result.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}

// ── AddSessionToMemory ────────────────────────────────────────────────────────

// AddSessionToMemory extrai respostas LLM da sessão e indexa como vetores
// no uRag-go, usando meta {mem_app, mem_user} para isolamento lógico.
func (v *VectorMemoryService) AddSessionToMemory(ctx context.Context, sess session.Session) error {
	type docInput struct {
		ID      string            `json:"id"`
		Content string            `json:"content"`
		Source  string            `json:"source"`
		Meta    map[string]string `json:"meta"`
	}
	var docs []docInput

	for ev := range sess.Events().All() {
		if ev.LLMResponse.Content == nil {
			continue
		}
		var text strings.Builder
		for _, p := range ev.LLMResponse.Content.Parts {
			if p.Text != "" {
				text.WriteString(p.Text)
			}
		}
		t := text.String()
		if t == "" {
			continue
		}
		docs = append(docs, docInput{
			ID:      fmt.Sprintf("mem_%s_%s", sess.UserID(), ev.ID),
			Content: t,
			Source:  "memory",
			Meta: map[string]string{
				"mem_app":  sess.AppName(),
				"mem_user": sess.UserID(),
			},
		})
	}

	if len(docs) == 0 {
		return nil
	}

	_, err := v.callTool(ctx, "vector_add", map[string]any{"documents": docs})
	if err != nil {
		log.Printf("[VectorMemory] AddSessionToMemory warn: %v", err)
		// best-effort: não propaga erro para não interromper o agente
		return nil
	}
	return nil
}

// ── SearchMemory ──────────────────────────────────────────────────────────────

type vectorQueryResult struct {
	Results []struct {
		Document struct {
			ID      string            `json:"id"`
			Content string            `json:"content"`
			Meta    map[string]string `json:"meta"`
		} `json:"document"`
		Score      float32 `json:"score"`
		Confidence float64 `json:"confidence"`
	} `json:"results"`
	Confidence float64 `json:"confidence"`
}

// SearchMemory busca memórias semanticamente similares à query do usuário
// chamando vector_query no uRag-go filtrado por {mem_app, mem_user}.
func (v *VectorMemoryService) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	rawJSON, err := v.callTool(ctx, "vector_query", map[string]any{
		"question": req.Query,
		"top_k":    5,
		"where": map[string]string{
			"mem_app":  req.AppName,
			"mem_user": req.UserID,
		},
	})
	if err != nil {
		log.Printf("[VectorMemory] SearchMemory warn: %v", err)
		return &adkmemory.SearchResponse{}, nil // fallback silencioso
	}

	var qr vectorQueryResult
	if err := json.Unmarshal([]byte(rawJSON), &qr); err != nil {
		return &adkmemory.SearchResponse{}, nil
	}

	resp := &adkmemory.SearchResponse{}
	for _, r := range qr.Results {
		resp.Memories = append(resp.Memories, adkmemory.Entry{
			ID:      r.Document.ID,
			Author:  "model",
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(r.Document.Content)}},
		})
	}
	return resp, nil
}

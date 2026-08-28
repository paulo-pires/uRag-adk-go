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
	"sort"
	"strings"
	"sync"
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
	gate     *Gate
	policy   Policy
	stats    statsSlot

	mu     sync.Mutex
	ledger map[string]Item // GAP-05/07 hits + last access
}

func NewVectorService(mcpURL, ragToken string) *VectorMemoryService {
	return &VectorMemoryService{
		mcpURL:   strings.TrimRight(mcpURL, "/"),
		ragToken: ragToken,
		client:   &http.Client{Timeout: 10 * time.Second},
		policy:   DefaultPolicy(),
		ledger:   map[string]Item{},
	}
}

func (v *VectorMemoryService) WithGate(g *Gate) *VectorMemoryService {
	v.gate = g
	return v
}

func (v *VectorMemoryService) WithPolicy(p Policy) *VectorMemoryService {
	v.policy = p
	return v
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
		if v.gate != nil {
			filtered, allow, _ := v.gate.FilterForStore(ctx, t)
			if !allow {
				continue
			}
			t = filtered
		}
		id := fmt.Sprintf("mem_%s_%s", sess.UserID(), ev.ID)
		now := time.Now().UTC()
		v.mu.Lock()
		v.ledger[id] = Item{ID: id, LastAccess: now, Hits: 0}
		v.mu.Unlock()
		docs = append(docs, docInput{
			ID:      id,
			Content: t,
			Source:  "memory",
			Meta: map[string]string{
				"mem_app":      sess.AppName(),
				"mem_user":     sess.UserID(),
				"mem_scanned":  "true",
				"mem_source":   "session",
				"mem_created":  now.Format(time.RFC3339),
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
	started := time.Now()
	rawJSON, err := v.callTool(ctx, "vector_query", map[string]any{
		"question": req.Query,
		"top_k":    20,
		"where": map[string]string{
			"mem_app":  req.AppName,
			"mem_user": req.UserID,
		},
	})
	if err != nil {
		log.Printf("[VectorMemory] SearchMemory warn: %v", err)
		v.stats.store(SearchStats{QueryTokens: EstimateTokens(req.Query), Latency: time.Since(started)})
		return &adkmemory.SearchResponse{}, nil
	}

	var qr vectorQueryResult
	if err := json.Unmarshal([]byte(rawJSON), &qr); err != nil {
		v.stats.store(SearchStats{QueryTokens: EstimateTokens(req.Query), Latency: time.Since(started)})
		return &adkmemory.SearchResponse{}, nil
	}

	now := time.Now().UTC()
	type ranked struct {
		id, text string
		score    float64
	}
	var keep []ranked
	dropped := 0
	for _, r := range qr.Results {
		if v.gate != nil && !v.gate.FilterForRecall(ctx, r.Document.Content) {
			dropped++
			continue
		}
		it := v.itemFor(r.Document.ID, r.Document.Meta)
		age := now.Sub(it.LastAccess)
		if v.policy.TTL > 0 && age > v.policy.TTL {
			dropped++
			continue
		}
		ds := DecayScore(age, it.Hits, v.policy.HalfLife)
		if it.Hits == 0 && v.policy.MinScore > 0 && ds < v.policy.MinScore {
			dropped++
			continue
		}
		keep = append(keep, ranked{
			id: r.Document.ID, text: r.Document.Content,
			score: ds * (1 + float64(r.Score)),
		})
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].score > keep[j].score })
	if len(keep) > 5 {
		dropped += len(keep) - 5
		keep = keep[:5]
	}

	resp := &adkmemory.SearchResponse{}
	var ids []string
	resultTokens := 0
	for _, k := range keep {
		ids = append(ids, k.id)
		resultTokens += EstimateTokens(k.text)
		v.touch(k.id)
		resp.Memories = append(resp.Memories, adkmemory.Entry{
			ID:      k.id,
			Author:  "model",
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(k.text)}},
		})
	}
	v.stats.store(SearchStats{
		QueryTokens:  EstimateTokens(req.Query),
		ResultTokens: resultTokens,
		Hits:         len(ids),
		Dropped:      dropped,
		MemoryIDs:    ids,
		Latency:      time.Since(started),
	})
	return resp, nil
}

func (v *VectorMemoryService) itemFor(id string, meta map[string]string) Item {
	v.mu.Lock()
	defer v.mu.Unlock()
	if it, ok := v.ledger[id]; ok {
		return it
	}
	it := Item{ID: id, LastAccess: time.Now().UTC()}
	if meta != nil {
		if s := meta["mem_created"]; s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				it.LastAccess = t
			}
		}
	}
	v.ledger[id] = it
	return it
}

func (v *VectorMemoryService) touch(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	it := v.ledger[id]
	it.ID = id
	it.LastAccess = time.Now().UTC()
	v.ledger[id] = it
}

func (v *VectorMemoryService) TakeLastSearch() SearchStats { return v.stats.take() }

func (v *VectorMemoryService) Credit(ids []string, success bool) {
	if !success || len(ids) == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now().UTC()
	for _, id := range ids {
		it := v.ledger[id]
		it.ID = id
		it.Hits++
		it.LastAccess = now
		v.ledger[id] = it
	}
}

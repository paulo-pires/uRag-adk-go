// Package guard é um cliente fino do uRag-guard-go: reporta a run do agente
// (pergunta/resposta/timing) e o score de faithfulness calculado sobre ela.
// Desligado (todo método vira no-op) se URL ou Token estiverem vazios —
// nunca bloqueia nem falha o loop do agente.
package guard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const rulesCacheTTL = 30 * time.Second

type rulesCacheEntry struct {
	rules []GuardrailRule
	at    time.Time
}

type evalsCacheEntry struct {
	configs []EvalConfig
	at      time.Time
}

type Client struct {
	url   string
	token string
	http  *http.Client

	mu         sync.Mutex
	rulesCache map[string]rulesCacheEntry // key: source+":"+stage
	evalsCache map[string]evalsCacheEntry // key: source
}

func New(url, token string) *Client {
	return &Client{
		url:        url,
		token:      token,
		http:       &http.Client{Timeout: 5 * time.Second},
		rulesCache: make(map[string]rulesCacheEntry),
		evalsCache: make(map[string]evalsCacheEntry),
	}
}

func (c *Client) Enabled() bool {
	return c != nil && c.url != "" && c.token != ""
}

type RunInput struct {
	Source    string
	Name      string
	Question  string
	Answer    string
	Status    string
	StartedAt time.Time
	EndedAt   time.Time
	UserID    string
	Model     string
	Provider  string
	TokensIn  int64
	TokensOut int64
	CostUSD   float64
}

// PushRun cria a run no guard e devolve o run_id — string vazia se
// desabilitado ou se a chamada falhar (best-effort, nunca retorna erro).
func (c *Client) PushRun(in RunInput) string {
	if !c.Enabled() {
		return ""
	}
	body := map[string]any{
		"source":     in.Source,
		"name":       in.Name,
		"status":     in.Status,
		"started_at": in.StartedAt.UTC().Format(time.RFC3339),
		"ended_at":   in.EndedAt.UTC().Format(time.RFC3339),
		"input":      jsonString(in.Question),
		"output":     jsonString(in.Answer),
		"tokens_in":  in.TokensIn,
		"tokens_out": in.TokensOut,
		"cost_usd":   in.CostUSD,
	}
	if in.UserID != "" {
		body["user_id"] = in.UserID
	}
	if in.Model != "" {
		body["model"] = in.Model
	}
	if in.Provider != "" {
		body["provider"] = in.Provider
	}
	return c.post("/v1/runs", body)
}

// PushScore reporta um score de eval (ex: faithfulness) atrelado a uma run.
// evalConfigID é opcional (string vazia = score sem config associada, sem
// verdict pass/warn/fail calculado do lado do guard).
func (c *Client) PushScore(runID, evalConfigID, evalName string, value float64) {
	if !c.Enabled() || runID == "" {
		return
	}
	body := map[string]any{"run_id": runID, "eval_name": evalName, "value": value}
	if evalConfigID != "" {
		body["eval_config_id"] = evalConfigID
	}
	c.post("/v1/scores", body)
}

// EvalConfig é a config de um evaluator cadastrado na tela do guard —
// ver uRag-guard-go/SPEC.md §4.8. Metric "custom" com um "prompt_template"
// no Config vira um LLM-as-judge rodado aqui no agente (ver internal/eval).
// SampleRate (0-1) controla a fração de runs em que o eval roda de fato —
// ver FetchEvalConfigs, que já filtra só configs habilitadas.
type EvalConfig struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Metric     string          `json:"metric"`
	SampleRate float64         `json:"sample_rate"`
	Config     json.RawMessage `json:"config"`
	Enabled    bool            `json:"enabled"`
}

// FetchEvalConfigs busca os evaluators ativos pra este source.
// Resultado cacheado por 30s.
func (c *Client) FetchEvalConfigs(source string) []EvalConfig {
	if !c.Enabled() {
		return nil
	}
	c.mu.Lock()
	if e, ok := c.evalsCache[source]; ok && time.Since(e.at) < rulesCacheTTL {
		c.mu.Unlock()
		return e.configs
	}
	c.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, c.url+"/v1/eval-configs?source="+source+"&enabled=true", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-Guard-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out []EvalConfig
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck

	c.mu.Lock()
	c.evalsCache[source] = evalsCacheEntry{configs: out, at: time.Now()}
	c.mu.Unlock()
	return out
}

// GuardrailRule espelha a regra cadastrada no guard (subset dos campos necessários).
type GuardrailRule struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Type    string          `json:"type"`   // regex | keyword | external_webhook
	Config  json.RawMessage `json:"config"` // struct dependente do Type
	Stage   string          `json:"stage"`  // input | output | both
	Action  string          `json:"action"` // flag | block
	Enabled bool            `json:"enabled"`
}

// FetchRules busca as regras ativas para a source, filtradas por stage (client-side).
// Resultado cacheado por 30s — regras raramente mudam entre requests.
func (c *Client) FetchRules(source, stage string) []GuardrailRule {
	if !c.Enabled() {
		return nil
	}
	key := source + ":" + stage
	c.mu.Lock()
	if e, ok := c.rulesCache[key]; ok && time.Since(e.at) < rulesCacheTTL {
		c.mu.Unlock()
		return e.rules
	}
	c.mu.Unlock()

	url := c.url + "/v1/rules?enabled=true"
	if source != "" {
		url += "&source=" + source
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-Guard-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var all []GuardrailRule
	json.NewDecoder(resp.Body).Decode(&all) //nolint:errcheck

	var out []GuardrailRule
	if stage == "" {
		out = all
	} else {
		for _, r := range all {
			if r.Stage == stage || r.Stage == "both" {
				out = append(out, r)
			}
		}
	}
	c.mu.Lock()
	c.rulesCache[key] = rulesCacheEntry{rules: out, at: time.Now()}
	c.mu.Unlock()
	return out
}

// SpanInput descreve uma tool call capturada pelo hook do ADK.
type SpanInput struct {
	RunID     string
	ToolName  string
	Args      map[string]any
	Result    map[string]any
	StartedAt time.Time
	EndedAt   time.Time
	Error     string
}

// PushSpan reporta um span de tool call filho da run. Best-effort, sem erro.
func (c *Client) PushSpan(in SpanInput) {
	if !c.Enabled() || in.RunID == "" {
		return
	}
	body := map[string]any{
		"run_id":      in.RunID,
		"tool_name":   in.ToolName,
		"args":        in.Args,
		"result":      in.Result,
		"started_at":  in.StartedAt.UTC().Format(time.RFC3339Nano),
		"ended_at":    in.EndedAt.UTC().Format(time.RFC3339Nano),
		"duration_ms": in.EndedAt.Sub(in.StartedAt).Milliseconds(),
	}
	if in.Error != "" {
		body["error"] = in.Error
	}
	c.post("/v1/spans", body)
}

// PushGuardrailEvent registra uma violação de regra atrelada a uma run.
func (c *Client) PushGuardrailEvent(runID, ruleID, ruleName, stage, verdict, snippet string) {
	if !c.Enabled() || runID == "" {
		return
	}
	body := map[string]any{
		"run_id":    runID,
		"rule_id":   ruleID,
		"rule_name": ruleName,
		"stage":     stage,
		"verdict":   verdict,
		"snippet":   snippet,
	}
	c.post("/v1/guardrail-events", body)
}

func (c *Client) post(path string, body map[string]any) string {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.url+path, bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Guard-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck
	return out["id"]
}

func jsonString(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

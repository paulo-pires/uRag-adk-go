// Package guard é um cliente fino do uRag-guard-go: reporta a run do agente
// (pergunta/resposta/timing) e o score de faithfulness calculado sobre ela.
// Desligado (todo método vira no-op) se URL ou Token estiverem vazios —
// nunca bloqueia nem falha o loop do agente.
package guard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

type Client struct {
	url   string
	token string
	http  *http.Client
}

func New(url, token string) *Client {
	return &Client{url: url, token: token, http: &http.Client{Timeout: 5 * time.Second}}
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
}

// PushRun cria a run no guard e devolve o run_id — string vazia se
// desabilitado ou se a chamada falhar (best-effort, nunca retorna erro).
func (c *Client) PushRun(in RunInput) string {
	if !c.Enabled() {
		return ""
	}
	body := map[string]any{
		"source": in.Source, "name": in.Name, "status": in.Status,
		"started_at": in.StartedAt.UTC().Format(time.RFC3339), "ended_at": in.EndedAt.UTC().Format(time.RFC3339),
		"input": jsonString(in.Question), "output": jsonString(in.Answer),
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

// FetchEvalConfigs busca os evaluators ativos pra este source — nil (sem
// erro) se desabilitado ou se a chamada falhar, nunca bloqueia o agente.
func (c *Client) FetchEvalConfigs(source string) []EvalConfig {
	if !c.Enabled() {
		return nil
	}
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
	return out
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

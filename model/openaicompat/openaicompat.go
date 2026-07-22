// Package openaicompat implements model.LLM for any OpenAI-compatible endpoint
// (LM Studio, Ollama /v1, OpenRouter, etc.).
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"sync"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

// UsageAccumulator soma tokens de todas as chamadas LLM dentro de um run.
type UsageAccumulator struct {
	mu  sync.Mutex
	In  int64
	Out int64
}

func (a *UsageAccumulator) add(in, out int) {
	a.mu.Lock()
	a.In += int64(in)
	a.Out += int64(out)
	a.mu.Unlock()
}

type usageKey struct{}

func WithUsageAccumulator(ctx context.Context, acc *UsageAccumulator) context.Context {
	return context.WithValue(ctx, usageKey{}, acc)
}

func accFromCtx(ctx context.Context) *UsageAccumulator {
	acc, _ := ctx.Value(usageKey{}).(*UsageAccumulator)
	return acc
}

type Model struct {
	baseURL   string // e.g. "http://localhost:1234/v1"
	modelName string // passed as "model" field; LM Studio accepts whatever is loaded
	client    *http.Client
}

func New(baseURL, modelName string) *Model {
	return &Model{baseURL: baseURL, modelName: modelName, client: http.DefaultClient}
}

func (m *Model) Name() string { return m.modelName }

func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		msgs, err := contentsToOAI(req.Contents)
		if err != nil {
			yield(nil, err)
			return
		}

		if req.Config != nil && req.Config.SystemInstruction != nil {
			if text := partsText(req.Config.SystemInstruction.Parts); text != "" {
				msgs = append([]oaiMessage{{Role: "system", Content: text}}, msgs...)
			}
		}

		payload := oaiRequest{Model: m.modelName, Messages: msgs, Stream: false}
		if req.Config != nil {
			payload.Tools = toolsToOAI(req.Config.Tools)
			if req.Config.Temperature != nil {
				t := float64(*req.Config.Temperature)
				payload.Temperature = &t
			}
		}

		body, _ := json.Marshal(payload)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			m.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield(nil, err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := m.client.Do(httpReq)
		if err != nil {
			yield(nil, fmt.Errorf("openaicompat: %w", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			var buf bytes.Buffer
			buf.ReadFrom(resp.Body)
			yield(nil, fmt.Errorf("openaicompat %d: %s", resp.StatusCode, buf.String()))
			return
		}

		var result oaiResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			yield(nil, fmt.Errorf("openaicompat decode: %w", err))
			return
		}
		if len(result.Choices) == 0 {
			yield(nil, fmt.Errorf("openaicompat: empty choices"))
			return
		}

		if acc := accFromCtx(ctx); acc != nil {
			acc.add(result.Usage.PromptTokens, result.Usage.CompletionTokens)
		}
		yield(oaiToLLMResponse(result.Choices[0]), nil)
	}
}

// Generate faz uma chamada LLM single-turn — usado pelo runner de eval.
func (m *Model) Generate(ctx context.Context, prompt string) (string, error) {
	payload := oaiRequest{
		Model:    m.modelName,
		Messages: []oaiMessage{{Role: "user", Content: prompt}},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openaicompat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return "", fmt.Errorf("openaicompat %d: %s", resp.StatusCode, buf.String())
	}
	var result oaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openaicompat: empty choices")
	}
	return result.Choices[0].Message.Content, nil
}

// ── wire types ───────────────────────────────────────────────────────────────

type oaiMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []oaiToolUse `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
}

type oaiToolUse struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON string, not parsed
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
	} `json:"function"`
}

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	Stream      bool         `json:"stream"`
	Temperature *float64     `json:"temperature,omitempty"`
}

type oaiChoice struct {
	Message      oaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaiResponse struct {
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

// ── translators ──────────────────────────────────────────────────────────────

func contentsToOAI(contents []*genai.Content) ([]oaiMessage, error) {
	var msgs []oaiMessage
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := c.Role
		if role == genai.RoleModel {
			role = "assistant"
		}

		var toolCalls []oaiToolUse
		var text string
		var funcResponses []*genai.FunctionResponse

		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				args, _ := json.Marshal(p.FunctionCall.Args)
				tc := oaiToolUse{ID: p.FunctionCall.ID, Type: "function"}
				tc.Function.Name = p.FunctionCall.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)
			case p.FunctionResponse != nil:
				funcResponses = append(funcResponses, p.FunctionResponse)
			case p.Text != "":
				text += p.Text
			}
		}

		// genai bundles function results as role:"user" with FunctionResponse parts.
		// OpenAI wants role:"tool" per call.
		if len(funcResponses) > 0 {
			for _, fr := range funcResponses {
				out, _ := json.Marshal(fr.Response)
				msgs = append(msgs, oaiMessage{
					Role:       "tool",
					Content:    string(out),
					ToolCallID: fr.ID,
					Name:       fr.Name,
				})
			}
			continue
		}

		msgs = append(msgs, oaiMessage{Role: role, Content: text, ToolCalls: toolCalls})
	}
	return msgs, nil
}

func toolsToOAI(tools []*genai.Tool) []oaiTool {
	var out []oaiTool
	for _, t := range tools {
		for _, fd := range t.FunctionDeclarations {
			ot := oaiTool{Type: "function"}
			ot.Function.Name = fd.Name
			ot.Function.Description = fd.Description
			ot.Function.Parameters = schemaToJSON(fd.Parameters)
			out = append(out, ot)
		}
	}
	return out
}

func oaiToLLMResponse(choice oaiChoice) *model.LLMResponse {
	msg := choice.Message
	var parts []*genai.Part

	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   tc.ID,
					Name: tc.Function.Name,
					Args: args,
				},
			})
		}
	} else if msg.Content != "" {
		parts = append(parts, genai.NewPartFromText(msg.Content))
	}

	finishReason := genai.FinishReasonStop
	if choice.FinishReason == "length" {
		finishReason = genai.FinishReasonMaxTokens
	}

	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
		TurnComplete: true,
		FinishReason: finishReason,
	}
}

func schemaToJSON(s *genai.Schema) map[string]any {
	if s == nil {
		// LM Studio rejeita null e exige properties presente
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	m := map[string]any{"type": string(s.Type)}
	if s.Description != "" {
		m["description"] = s.Description
	}
	// LM Studio exige properties mesmo vazio quando type=object
	if string(s.Type) == "object" || len(s.Properties) > 0 {
		props := map[string]any{}
		for k, v := range s.Properties {
			props[k] = schemaToJSON(v)
		}
		m["properties"] = props
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if s.Items != nil {
		m["items"] = schemaToJSON(s.Items)
	}
	return m
}

func partsText(parts []*genai.Part) string {
	var s string
	for _, p := range parts {
		s += p.Text
	}
	return s
}

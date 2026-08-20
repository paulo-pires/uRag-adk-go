package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"urag-adk-go/pkg/copilots"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// ── DB ────────────────────────────────────────────────────────────────────────

var copilotDB *gorm.DB

// runAgentFunc é injetado por main() para permitir que o handler /copilots/:id/chat
// chame o loop ReAct ADK sem depender de closures internas do main.
var runAgentFunc func(ctx context.Context, question, systemPrompt, sessionID, userID string) (answer string, sessID string, err error)

func initCopilotDB() {
	dsn := os.Getenv("COPILOT_DSN")
	if dsn == "" {
		dsn = os.Getenv("SESSION_DSN")
	}
	if dsn == "" {
		log.Printf("copilots: COPILOT_DSN / SESSION_DSN não definido — copilots desabilitados")
		return
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		log.Printf("copilots: não foi possível conectar ao DB: %v — copilots desabilitados", err)
		return
	}

	if err := db.Exec(`
		CREATE TABLE IF NOT EXISTS copilots (
			id          TEXT PRIMARY KEY,
			project_id  TEXT NOT NULL,
			name        TEXT NOT NULL,
			function    TEXT NOT NULL,
			description TEXT DEFAULT '',
			system_prompt TEXT NOT NULL,
			model       TEXT DEFAULT '',
			tools       JSONB DEFAULT '[]'::jsonb,
			config      JSONB DEFAULT '{}'::jsonb,
			access      JSONB DEFAULT '{"type":"all"}'::jsonb,
			status      TEXT DEFAULT 'active',
			created_at  TIMESTAMPTZ DEFAULT NOW(),
			updated_at  TIMESTAMPTZ DEFAULT NOW()
		)
	`).Error; err != nil {
		log.Printf("copilots: criar tabela: %v", err)
		return
	}

	copilotDB = db
	log.Printf("copilots: DB inicializado")
}

// ── Register routes ───────────────────────────────────────────────────────────

func registerCopilotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /copilots", handleCreateCopilot)
	mux.HandleFunc("GET /copilots", handleListCopilots)
	mux.HandleFunc("GET /copilots/{id}", handleGetCopilot)
	mux.HandleFunc("PUT /copilots/{id}", handleUpdateCopilot)
	mux.HandleFunc("DELETE /copilots/{id}", handleDeleteCopilot)
	mux.HandleFunc("POST /copilots/{id}/chat", handleCopilotChat)
	mux.HandleFunc("PUT /copilots/{id}/status", handleCopilotStatus)
	mux.HandleFunc("GET /copilots/{id}/stats", handleCopilotStats)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func copilotJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func copilotErrResp(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func copilotDBRequired(w http.ResponseWriter) bool {
	if copilotDB == nil {
		copilotErrResp(w, "copilots: banco de dados não configurado (defina COPILOT_DSN ou SESSION_DSN)", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func resolveProjectID(r *http.Request) string {
	if pid := r.Header.Get("X-Project-ID"); pid != "" {
		return pid
	}
	if key := r.Header.Get("X-Api-Key"); key != "" {
		if len(key) > 16 {
			return key[:16]
		}
		return key
	}
	return "default"
}

func newCopilotID() string {
	b := make([]byte, 8)
	if _, err := cryptorand.Read(b); err != nil {
		return fmt.Sprintf("cop-%d", time.Now().UnixNano())
	}
	return "cop-" + hex.EncodeToString(b)
}

// ── CRUD Handlers ─────────────────────────────────────────────────────────────

func handleCreateCopilot(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}

	var body struct {
		Name         string          `json:"name"`
		Function     string          `json:"function"`
		Description  string          `json:"description"`
		SystemPrompt string          `json:"system_prompt"`
		Model        string          `json:"model"`
		Tools        json.RawMessage `json:"tools"`
		Config       json.RawMessage `json:"config"`
		Access       json.RawMessage `json:"access"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		copilotErrResp(w, "body JSON inválido", http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.Function == "" {
		copilotErrResp(w, "name e function são obrigatórios", http.StatusBadRequest)
		return
	}

	systemPrompt := body.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = copilots.DefaultSystemPrompt(body.Function)
	}

	toolsStr := "[]"
	if len(body.Tools) > 0 {
		toolsStr = string(body.Tools)
	}
	cfgStr := "{}"
	if len(body.Config) > 0 {
		cfgStr = string(body.Config)
	}
	accessStr := `{"type":"all"}`
	if len(body.Access) > 0 {
		accessStr = string(body.Access)
	}

	id := newCopilotID()
	projectID := resolveProjectID(r)
	now := time.Now().UTC()

	err := copilotDB.Exec(`
		INSERT INTO copilots (id, project_id, name, function, description, system_prompt, model, tools, config, access, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, ?::jsonb, 'active', ?, ?)
	`, id, projectID, body.Name, body.Function, body.Description, systemPrompt,
		body.Model, toolsStr, cfgStr, accessStr, now, now).Error
	if err != nil {
		log.Printf("copilots create: %v", err)
		copilotErrResp(w, "erro ao criar copiloto: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	copilotJSON(w, map[string]interface{}{
		"id":            id,
		"name":          body.Name,
		"function":      body.Function,
		"description":   body.Description,
		"system_prompt": systemPrompt,
		"model":         body.Model,
		"status":        "active",
		"project_id":    projectID,
		"created_at":    now,
		"updated_at":    now,
	})
}

func handleListCopilots(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}
	projectID := resolveProjectID(r)

	rows, err := copilotDB.Raw(`
		SELECT id, project_id, name, function, description, system_prompt, model,
		       tools::text, config::text, access::text, status, created_at, updated_at
		FROM copilots WHERE project_id = ? ORDER BY created_at DESC
	`, projectID).Rows()
	if err != nil {
		copilotErrResp(w, "listar copilotos: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var (
			cid, pid, name, fn, desc, sp, model string
			toolsStr, cfgStr, accessStr, status  string
			createdAt, updatedAt                 time.Time
		)
		if err := rows.Scan(&cid, &pid, &name, &fn, &desc, &sp, &model,
			&toolsStr, &cfgStr, &accessStr, &status, &createdAt, &updatedAt); err != nil {
			continue
		}
		result = append(result, map[string]interface{}{
			"id": cid, "project_id": pid, "name": name, "function": fn,
			"description": desc, "system_prompt": sp, "model": model,
			"tools":      json.RawMessage(toolsStr),
			"config":     json.RawMessage(cfgStr),
			"access":     json.RawMessage(accessStr),
			"status":     status,
			"created_at": createdAt, "updated_at": updatedAt,
		})
	}
	if result == nil {
		result = []map[string]interface{}{}
	}
	copilotJSON(w, map[string]interface{}{"copilots": result, "total": len(result)})
}

func handleGetCopilot(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}
	id := r.PathValue("id")
	row := copilotDB.Raw(`
		SELECT id, project_id, name, function, description, system_prompt, model,
		       tools::text, config::text, access::text, status, created_at, updated_at
		FROM copilots WHERE id = ?
	`, id).Row()

	var (
		cid, pid, name, fn, desc, sp, model string
		toolsStr, cfgStr, accessStr, status  string
		createdAt, updatedAt                 time.Time
	)
	if err := row.Scan(&cid, &pid, &name, &fn, &desc, &sp, &model,
		&toolsStr, &cfgStr, &accessStr, &status, &createdAt, &updatedAt); err != nil {
		copilotErrResp(w, "copiloto não encontrado", http.StatusNotFound)
		return
	}

	copilotJSON(w, map[string]interface{}{
		"id": cid, "project_id": pid, "name": name, "function": fn,
		"description": desc, "system_prompt": sp, "model": model,
		"tools":      json.RawMessage(toolsStr),
		"config":     json.RawMessage(cfgStr),
		"access":     json.RawMessage(accessStr),
		"status":     status,
		"created_at": createdAt, "updated_at": updatedAt,
	})
}

func handleUpdateCopilot(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}
	id := r.PathValue("id")

	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		copilotErrResp(w, "body JSON inválido", http.StatusBadRequest)
		return
	}

	setClauses := []string{"updated_at = ?"}
	args := []interface{}{time.Now().UTC()}

	if v, ok := body["name"].(string); ok && v != "" {
		setClauses = append(setClauses, "name = ?")
		args = append(args, v)
	}
	if v, ok := body["description"].(string); ok {
		setClauses = append(setClauses, "description = ?")
		args = append(args, v)
	}
	if v, ok := body["system_prompt"].(string); ok && v != "" {
		setClauses = append(setClauses, "system_prompt = ?")
		args = append(args, v)
	}
	if v, ok := body["model"].(string); ok {
		setClauses = append(setClauses, "model = ?")
		args = append(args, v)
	}
	if v, ok := body["status"].(string); ok && v != "" {
		setClauses = append(setClauses, "status = ?")
		args = append(args, v)
	}

	args = append(args, id)
	query := "UPDATE copilots SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"

	if err := copilotDB.Exec(query, args...).Error; err != nil {
		copilotErrResp(w, "atualizar copiloto: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copilotJSON(w, map[string]string{"status": "updated", "id": id})
}

func handleDeleteCopilot(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}
	id := r.PathValue("id")
	if err := copilotDB.Exec("DELETE FROM copilots WHERE id = ?", id).Error; err != nil {
		copilotErrResp(w, "remover copiloto: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleCopilotStatus(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}
	id := r.PathValue("id")

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Status == "" {
		copilotErrResp(w, "status é obrigatório (active|paused)", http.StatusBadRequest)
		return
	}
	if body.Status != "active" && body.Status != "paused" {
		copilotErrResp(w, "status deve ser 'active' ou 'paused'", http.StatusBadRequest)
		return
	}

	if err := copilotDB.Exec("UPDATE copilots SET status = ?, updated_at = ? WHERE id = ?",
		body.Status, time.Now().UTC(), id).Error; err != nil {
		copilotErrResp(w, "atualizar status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copilotJSON(w, map[string]string{"id": id, "status": body.Status})
}

func handleCopilotChat(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}
	id := r.PathValue("id")

	// Carregar copiloto
	row := copilotDB.Raw(`
		SELECT id, system_prompt, model, status FROM copilots WHERE id = ?
	`, id).Row()
	var cid, systemPrompt, model, status string
	if err := row.Scan(&cid, &systemPrompt, &model, &status); err != nil {
		copilotErrResp(w, "copiloto não encontrado", http.StatusNotFound)
		return
	}
	if status == "paused" {
		copilotErrResp(w, "copiloto está pausado", http.StatusForbidden)
		return
	}

	var body struct {
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
		UserID    string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		copilotErrResp(w, "message é obrigatório", http.StatusBadRequest)
		return
	}

	if runAgentFunc == nil {
		copilotErrResp(w, "agente não disponível", http.StatusServiceUnavailable)
		return
	}

	answer, sessID, err := runAgentFunc(r.Context(), body.Message, systemPrompt, body.SessionID, body.UserID)
	if err != nil {
		copilotErrResp(w, "agente: "+err.Error(), http.StatusInternalServerError)
		return
	}

	copilotJSON(w, map[string]string{
		"response":   answer,
		"session_id": sessID,
		"copilot_id": cid,
		"model":      model,
	})
}

func handleCopilotStats(w http.ResponseWriter, r *http.Request) {
	if !copilotDBRequired(w) {
		return
	}
	id := r.PathValue("id")

	var count int64
	if err := copilotDB.Raw("SELECT COUNT(*) FROM copilots WHERE id = ?", id).Scan(&count).Error; err != nil || count == 0 {
		copilotErrResp(w, "copiloto não encontrado", http.StatusNotFound)
		return
	}

	copilotJSON(w, map[string]interface{}{
		"copilot_id":          id,
		"total_conversations": 0,
		"conversations_today": 0,
		"avg_satisfaction":    nil,
	})
}

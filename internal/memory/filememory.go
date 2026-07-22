// Package memory implementa memory.Service com persistência em arquivo JSONL.
// Cada usuário tem um arquivo {dir}/{appName}/{userID}.jsonl.
// Usa o mesmo keyword-matching do InMemoryService do ADK.
package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
)

// FileService é um memory.Service persistido em JSONL por usuário.
type FileService struct {
	dir string

	mu    sync.RWMutex
	cache map[cacheKey][]entry // carregado sob demanda, nunca invalidado
}

type cacheKey struct{ appName, userID string }

type entry struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Timestamp time.Time `json:"timestamp"`
	Text      string    `json:"text"` // texto plano extraído do Content
	words     map[string]struct{}
}

// New cria um FileService que armazena memórias em dir.
func New(dir string) *FileService {
	return &FileService{
		dir:   dir,
		cache: make(map[cacheKey][]entry),
	}
}

// AddSessionToMemory extrai respostas LLM da sessão e persiste em JSONL.
func (s *FileService) AddSessionToMemory(ctx context.Context, sess session.Session) error {
	var newEntries []entry
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
		newEntries = append(newEntries, entry{
			ID:        ev.ID,
			Author:    ev.Author,
			Timestamp: ev.Timestamp,
			Text:      t,
			words:     extractWords(t),
		})
	}
	if len(newEntries) == 0 {
		return nil
	}

	path := s.filePath(sess.AppName(), sess.UserID())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range newEntries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}

	k := cacheKey{sess.AppName(), sess.UserID()}
	s.mu.Lock()
	s.cache[k] = append(s.cache[k], newEntries...)
	s.mu.Unlock()
	return nil
}

// SearchMemory faz keyword matching sobre as memórias do usuário.
func (s *FileService) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	k := cacheKey{req.AppName, req.UserID}

	s.mu.RLock()
	entries, loaded := s.cache[k]
	s.mu.RUnlock()

	if !loaded {
		loaded2, err := s.load(req.AppName, req.UserID)
		if err != nil {
			return &adkmemory.SearchResponse{}, nil //nolint:nilerr — best-effort
		}
		s.mu.Lock()
		s.cache[k] = loaded2
		s.mu.Unlock()
		entries = loaded2
	}

	queryWords := extractWords(req.Query)
	resp := &adkmemory.SearchResponse{}
	for _, e := range entries {
		if intersects(e.words, queryWords) {
			resp.Memories = append(resp.Memories, adkmemory.Entry{
				ID:        e.ID,
				Author:    e.Author,
				Timestamp: e.Timestamp,
				Content:   &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(e.Text)}},
			})
		}
	}
	return resp, nil
}

func (s *FileService) load(appName, userID string) ([]entry, error) {
	f, err := os.Open(s.filePath(appName, userID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			e.words = extractWords(e.Text)
			entries = append(entries, e)
		}
	}
	return entries, sc.Err()
}

func (s *FileService) filePath(appName, userID string) string {
	return filepath.Join(s.dir, appName, userID+".jsonl")
}

func extractWords(text string) map[string]struct{} {
	words := make(map[string]struct{})
	for _, w := range strings.Fields(strings.ToLower(text)) {
		words[w] = struct{}{}
	}
	return words
}

func intersects(a, b map[string]struct{}) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

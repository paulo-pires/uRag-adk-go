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
	dir    string
	gate   *Gate
	policy Policy
	stats  statsSlot

	mu    sync.RWMutex
	cache map[cacheKey][]entry
}

type cacheKey struct{ appName, userID string }

type entry struct {
	ID         string    `json:"id"`
	Author     string    `json:"author"`
	Timestamp  time.Time `json:"timestamp"`
	LastAccess time.Time `json:"last_access"`
	Hits       int       `json:"hits"`
	Text       string    `json:"text"`
	words      map[string]struct{}
}

func New(dir string) *FileService {
	return &FileService{
		dir:    dir,
		cache:  make(map[cacheKey][]entry),
		policy: DefaultPolicy(),
	}
}

func (s *FileService) WithGate(g *Gate) *FileService {
	s.gate = g
	return s
}

func (s *FileService) WithPolicy(p Policy) *FileService {
	s.policy = p
	return s
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
		if s.gate != nil {
			filtered, allow, _ := s.gate.FilterForStore(ctx, t)
			if !allow {
				continue
			}
			t = filtered
		}
		newEntries = append(newEntries, entry{
			ID:         ev.ID,
			Author:     ev.Author,
			Timestamp:  ev.Timestamp,
			LastAccess: time.Now().UTC(),
			Hits:       0,
			Text:       t,
			words:      extractWords(t),
		})
	}
	if len(newEntries) == 0 {
		return nil
	}

	k := cacheKey{sess.AppName(), sess.UserID()}
	s.mu.Lock()
	if _, ok := s.cache[k]; !ok {
		loaded, _ := s.load(sess.AppName(), sess.UserID())
		s.cache[k] = loaded
	}
	s.cache[k] = append(s.cache[k], newEntries...)
	pruned := s.pruneLocked(k)
	s.mu.Unlock()
	return s.rewrite(sess.AppName(), sess.UserID(), pruned)
}

// SearchMemory faz keyword matching sobre as memórias do usuário.
func (s *FileService) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	k := cacheKey{req.AppName, req.UserID}
	started := time.Now()
	queryWords := extractWords(req.Query)
	now := time.Now().UTC()

	s.mu.Lock()
	if _, ok := s.cache[k]; !ok {
		loaded, err := s.load(req.AppName, req.UserID)
		if err != nil {
			s.mu.Unlock()
			return &adkmemory.SearchResponse{}, nil
		}
		s.cache[k] = loaded
	}
	entries := s.cache[k]
	dropped := 0
	var surviving []entry
	for _, e := range entries {
		la := e.LastAccess
		if la.IsZero() {
			la = e.Timestamp
		}
		age := now.Sub(la)
		if s.policy.TTL > 0 && age > s.policy.TTL {
			dropped++
			continue
		}
		ds := DecayScore(age, e.Hits, s.policy.HalfLife)
		if e.Hits == 0 && s.policy.MinScore > 0 && ds < s.policy.MinScore {
			dropped++
			continue
		}
		surviving = append(surviving, e)
	}

	resp := &adkmemory.SearchResponse{}
	var ids []string
	resultTokens := 0
	for i := range surviving {
		e := surviving[i]
		if s.gate != nil && !s.gate.FilterForRecall(ctx, e.Text) {
			dropped++
			continue
		}
		if !intersects(e.words, queryWords) {
			continue
		}
		surviving[i].LastAccess = now
		ids = append(ids, e.ID)
		resultTokens += EstimateTokens(e.Text)
		resp.Memories = append(resp.Memories, adkmemory.Entry{
			ID:        e.ID,
			Author:    e.Author,
			Timestamp: e.Timestamp,
			Content:   &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(e.Text)}},
		})
	}
	s.cache[k] = surviving
	s.mu.Unlock()

	s.stats.store(SearchStats{
		QueryTokens:  EstimateTokens(req.Query),
		ResultTokens: resultTokens,
		Hits:         len(ids),
		Dropped:      dropped,
		MemoryIDs:    ids,
		Latency:      time.Since(started),
	})
	return resp, nil
}

func (s *FileService) TakeLastSearch() SearchStats { return s.stats.take() }

func (s *FileService) Credit(ids []string, success bool) {
	if !success || len(ids) == 0 {
		return
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	now := time.Now().UTC()
	for k, entries := range s.cache {
		for i := range entries {
			if want[entries[i].ID] {
				entries[i].Hits++
				entries[i].LastAccess = now
			}
		}
		s.cache[k] = entries
	}
	s.mu.Unlock()
}

func (s *FileService) pruneLocked(k cacheKey) []entry {
	entries := s.cache[k]
	now := time.Now().UTC()
	items := make([]Item, 0, len(entries))
	byID := map[string]entry{}
	for _, e := range entries {
		la := e.LastAccess
		if la.IsZero() {
			la = e.Timestamp
		}
		byID[e.ID] = e
		items = append(items, Item{ID: e.ID, LastAccess: la, Hits: e.Hits})
	}
	kept := SelectKept(items, now, s.policy)
	out := make([]entry, 0, len(kept))
	for _, it := range kept {
		out = append(out, byID[it.ID])
	}
	s.cache[k] = out
	return out
}

func (s *FileService) rewrite(appName, userID string, entries []entry) error {
	path := s.filePath(appName, userID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

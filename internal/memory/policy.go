package memory

import (
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// Policy é o esquecimento (GAP-05): TTL por último acesso, cap e decaimento.
type Policy struct {
	TTL        time.Duration
	HalfLife   time.Duration
	MaxEntries int
	MinScore   float64
}

func DefaultPolicy() Policy {
	return Policy{
		TTL:        30 * 24 * time.Hour,
		HalfLife:   7 * 24 * time.Hour,
		MaxEntries: 200,
		MinScore:   0.05,
	}
}

func PolicyFromEnv() Policy {
	p := DefaultPolicy()
	if d, err := time.ParseDuration(os.Getenv("MEMORY_TTL")); err == nil && d > 0 {
		p.TTL = d
	}
	if d, err := time.ParseDuration(os.Getenv("MEMORY_HALF_LIFE")); err == nil && d > 0 {
		p.HalfLife = d
	}
	if n, err := strconv.Atoi(os.Getenv("MEMORY_MAX_ENTRIES")); err == nil && n > 0 {
		p.MaxEntries = n
	}
	if f, err := strconv.ParseFloat(os.Getenv("MEMORY_MIN_SCORE"), 64); err == nil && f >= 0 {
		p.MinScore = f
	}
	return p
}

// Item is a memory row for eviction ranking.
type Item struct {
	ID         string
	LastAccess time.Time
	Hits       int
}

// DecayScore cai pela metade a cada HalfLife; hits aumentam a chance de sobreviver.
func DecayScore(age time.Duration, hits int, halfLife time.Duration) float64 {
	if halfLife <= 0 {
		return 1 + math.Log1p(float64(hits))
	}
	if age < 0 {
		age = 0
	}
	recency := math.Exp(-float64(age) / float64(halfLife) * math.Ln2)
	return recency * (1 + math.Log1p(float64(hits)))
}

// SelectKept aplica TTL + min score + cap. Ordem: maior score primeiro.
func SelectKept(items []Item, now time.Time, p Policy) []Item {
	if p.MaxEntries <= 0 {
		p.MaxEntries = DefaultPolicy().MaxEntries
	}
	type scored struct {
		Item
		score float64
	}
	var keep []scored
	for _, it := range items {
		age := now.Sub(it.LastAccess)
		if p.TTL > 0 && age > p.TTL {
			continue
		}
		score := DecayScore(age, it.Hits, p.HalfLife)
		if it.Hits == 0 && p.MinScore > 0 && score < p.MinScore {
			continue
		}
		keep = append(keep, scored{Item: it, score: score})
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].score > keep[j].score })
	if len(keep) > p.MaxEntries {
		keep = keep[:p.MaxEntries]
	}
	out := make([]Item, len(keep))
	for i, s := range keep {
		out[i] = s.Item
	}
	return out
}

// EstimateTokens is a cheap tokenizer (runes/4) for GAP-06 memory token/query.
func EstimateTokens(text string) int {
	n := utf8.RuneCountInString(text)
	if n == 0 {
		return 0
	}
	t := (n + 3) / 4
	if t < 1 {
		return 1
	}
	return t
}

// SearchStats is the last SearchMemory instrumentation (GAP-06).
type SearchStats struct {
	QueryTokens  int
	ResultTokens int
	Hits         int
	Dropped      int
	MemoryIDs    []string
	Latency      time.Duration
}

func (s SearchStats) TotalTokens() int { return s.QueryTokens + s.ResultTokens }

// Instrumented is implemented by File/Vector memory backends.
type Instrumented interface {
	TakeLastSearch() SearchStats
	Credit(ids []string, success bool)
}

type statsSlot struct {
	mu   sync.Mutex
	last SearchStats
}

func (s *statsSlot) store(st SearchStats) {
	s.mu.Lock()
	s.last = st
	s.mu.Unlock()
}

func (s *statsSlot) take() SearchStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.last
	s.last = SearchStats{}
	return out
}

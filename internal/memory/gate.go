package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Gate decide o que pode entrar (e sair) da memória persistente.
// GAP-08: sessão→memória não passava por controle; o guard só via I/O do LLM.
type Gate struct {
	mode      string // enforce | warn | off
	redactPII bool
	mlURL     string
	http      *http.Client
}

func NewGate(mode, mlURL string, redactPII bool) *Gate {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "enforce"
	}
	return &Gate{
		mode:      mode,
		redactPII: redactPII,
		mlURL:     strings.TrimRight(mlURL, "/"),
		http:      &http.Client{Timeout: 800 * time.Millisecond},
	}
}

func NewGateFromEnv() *Gate {
	redact := os.Getenv("MEMORY_REDACT_PII") != "false"
	g := NewGate(os.Getenv("MEMORY_GATE_MODE"), os.Getenv("ML_GUARD_URL"), redact)
	if g.Enabled() {
		log.Printf("  Memory gate: mode=%s ml_guard=%s redact_pii=%v", g.mode, nonempty(g.mlURL, "local-only"), g.redactPII)
	}
	return g
}

func (g *Gate) Enabled() bool {
	return g != nil && g.mode != "off"
}

// FilterForStore devolve o texto a persistir (possivelmente redigido) e se deve gravar.
func (g *Gate) FilterForStore(ctx context.Context, text string) (out string, allow bool, reason string) {
	return g.filter(ctx, text, true)
}

// FilterForRecall descarta memória já persistida se parecer injeção (defesa na leitura).
func (g *Gate) FilterForRecall(ctx context.Context, text string) bool {
	if g == nil || !g.Enabled() {
		return true
	}
	_, allow, _ := g.filter(ctx, text, false)
	return allow
}

func (g *Gate) filter(ctx context.Context, text string, mutate bool) (string, bool, string) {
	if g == nil || g.mode == "off" {
		return text, true, ""
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false, "empty"
	}

	if reason := localInjectionReason(t); reason != "" {
		if g.mode == "warn" {
			log.Printf("[MemoryGate] warn store: %s", reason)
			return maybeRedact(t, g.redactPII && mutate), true, reason
		}
		log.Printf("[MemoryGate] block: %s", reason)
		return "", false, reason
	}

	if g.mlURL != "" {
		if reason := g.mlScan(ctx, t); reason != "" {
			if g.mode == "warn" {
				log.Printf("[MemoryGate] ml-guard warn: %s", reason)
			} else {
				log.Printf("[MemoryGate] ml-guard block: %s", reason)
				return "", false, reason
			}
		}
	}

	if mutate && g.redactPII {
		t = redactPII(t)
	}
	return t, true, ""
}

func (g *Gate) mlScan(ctx context.Context, text string) string {
	for _, path := range []string{"/injection", "/toxicity"} {
		verdict, detail := g.mlPost(ctx, path, text)
		if verdict == "block" {
			return path + ": " + detail
		}
	}
	return ""
}

func (g *Gate) mlPost(ctx context.Context, path, text string) (verdict, detail string) {
	body, _ := json.Marshal(map[string]string{"text": text, "stage": "memory"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.mlURL+path, bytes.NewReader(body))
	if err != nil {
		return "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", "" // fail-open no remoto; heurística local já rodou
	}
	defer resp.Body.Close()
	var out struct {
		Verdict string `json:"verdict"`
		Detail  string `json:"detail"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Verdict, out.Detail
}

func maybeRedact(t string, on bool) string {
	if on {
		return redactPII(t)
	}
	return t
}

func nonempty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

var injectionREs = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+(?:your\s+)?(?:previous|above|prior|all)\s+instructions?`),
	regexp.MustCompile(`(?i)disregard\s+(?:your\s+)?(?:previous|prior)`),
	regexp.MustCompile(`(?i)forget\s+(?:your\s+)?(?:previous|prior)\s+instructions?`),
	regexp.MustCompile(`(?i)you\s+are\s+now\s+(?:a|an)\s+`),
	regexp.MustCompile(`(?i)pretend\s+(?:you\s+are|to\s+be)`),
	regexp.MustCompile(`(?i)\bjailbreak\b`),
	regexp.MustCompile(`(?i)\bDAN\s+mode\b`),
	regexp.MustCompile(`(?i)bypass\s+(?:your\s+)?(?:filters?|restrictions?|guidelines?)`),
	regexp.MustCompile(`(?i)system\s+prompt\s*[:=]`),
	regexp.MustCompile(`\[\[?INST\]\]?`),
	regexp.MustCompile(`<\|?(?:system|user|assistant)\|?>`),
	regexp.MustCompile(`(?i)always\s+remember\s+(?:that|to)\b`),
	regexp.MustCompile(`(?i)from\s+now\s+on.{0,80}(?:you\s+must|always|ignore)`),
	regexp.MustCompile(`(?i)when\s+(?:you\s+are\s+)?(?:asked|queried)`),
	regexp.MustCompile(`(?i)hidden\s+instruction`),
	regexp.MustCompile(`(?i)ignore\s+as\s+instru[cç][oõ]es`),
	regexp.MustCompile(`(?i)esque[cç]a\s+(?:suas\s+|as\s+)?instru[cç][oõ]es`),
	regexp.MustCompile(`(?i)a\s+partir\s+de\s+agora\s+voc[eê]`),
	regexp.MustCompile(`(?i)grava(?:r)?\s+(?:isso|isto)\s+na\s+mem[oó]ria`),
	regexp.MustCompile(`(?i)lembre-se\s+(?:para\s+sempre|sempre)\b`),
}

func localInjectionReason(text string) string {
	for _, re := range injectionREs {
		if re.MatchString(text) {
			return "injection:" + re.String()
		}
	}
	return ""
}

var (
	reEmail = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	reCPF   = regexp.MustCompile(`\b\d{3}\.?\d{3}\.?\d{3}-?\d{2}\b`)
	reCard  = regexp.MustCompile(`\b(?:\d{4}[-\s]?){3}\d{1,4}\b`)
)

func redactPII(text string) string {
	text = reEmail.ReplaceAllString(text, "[REDACTED_EMAIL]")
	text = reCPF.ReplaceAllString(text, "[REDACTED_CPF]")
	text = reCard.ReplaceAllString(text, "[REDACTED_CARD]")
	return text
}

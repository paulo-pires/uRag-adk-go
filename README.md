# urag-adk-go

Agente cognitivo em Go baseado em **Google ADK v2** (`google.golang.org/adk/v2`),
**substituto *drop-in* do [`uRag-agent-go`](../uRag-agent-go)**. Expõe um
servidor **MCP** real (Streamable HTTP) no `:8081` com a tool `agent_ask`,
consumindo as *stores* do [`uRag-go`](../uRag-go) como ferramentas do loop
ReAct. Mesma porta e mesmo nome de tool do agente anterior — troca transparente
para `uRag-gateway-go`, `uRag-workflow-go` e qualquer cliente MCP.

## Arquitetura

```
urag-adk-go  --MCP (Streamable HTTP)--▶  uRag-go (:8080 / URAG_MCP_URL)
  loop ReAct (ADK v2)                    Vector/Graph/Tree/SQL/Router
  tools = MCP tools do uRag-go (+ Science MCP opcional)
  LLM = OpenAI-compatible (LM Studio / Ollama / OpenRouter)
```

Disponível como servidor MCP HTTP (`agent_ask`). Quem chama: o
[`uRag-gateway-go`](../uRag-gateway-go) (rota `/ask` com estratégia "agent"),
o [`uRag-workflow-go`](../uRag-workflow-go) (nó `agentNode`) e o
[`ignus-code-landing-page`](../ignus-code-landing-page) via proxy.

## Pré-requisitos

- **Go 1.25+**
- **uRag-go rodando** (MCP, default `http://localhost:8080`) — o agente não
  tem *stores* próprias, só consome as do uRag-go via MCP.
- **LLM OpenAI-compatível** (default **LM Studio** `http://localhost:1234/v1`,
  mas funciona com Ollama `/v1`, OpenRouter etc — ver `model/openaicompat`).

## Build

```bash
go build -o agent.exe ./cmd/agent
```

## Rodando

```bash
# usa .env local (ver exemplo abaixo)
./agent.exe

# ou sobrescrevendo via env
URAG_MCP_URL=http://localhost:8080 LM_STUDIO_URL=http://localhost:1234/v1 \
  LM_STUDIO_MODEL=gemma-4-e2b-it AGENT_HTTP_ADDR=:8081 ./agent.exe
```

O servidor sobe em `AGENT_HTTP_ADDR` (default `:8081`) expondo:

- `/` — handler MCP (`mcp.NewStreamableHTTPHandler`), tool `agent_ask`.
- `/health` — health check (`{"status":"ok"}`).

### Tool `agent_ask`

| Argumento | Tipo | Descrição |
|---|---|---|
| `question` | string | Pergunta do usuário (obrigatório) |
| `system_prompt` | string | Instrução de sistema injetada como prefixo da mensagem |
| `session_id` | string | ID de sessão externo (multi-turn; reutiliza sessão ADK existente) |
| `user_id` | string | ID do usuário (namespacing de memória/sessão) |
| `temperature` | float | `nil` = default do modelo; `0.0` = determinístico (busca) |

Retorna `{ answer, session_id }`.

## Configuração (variáveis de ambiente)

Copie `.env` e ajuste. Todas opcionais (têm default).

| Var | Default | Descrição |
|---|---|---|
| `LM_STUDIO_URL` | `http://localhost:1234/v1` | Endpoint OpenAI-compatível do LLM |
| `LM_STUDIO_MODEL` | `gemma-4-e2b-it` | Modelo usado nas chamadas |
| `URAG_MCP_URL` | `http://localhost:8080` | URL do MCP do uRag-go |
| `URAG_RAG_TOKEN` | (vazio) | Token `X-RAG-Token` do uRag-go (se exigido) |
| `URAG_VECTOR_MEMORY` | (vazio) | `true` → memória semântica via uRag-go (ver abaixo) |
| `MEMORY_DIR` | (vazio) | Diretório de fallback (memória por keyword em JSONL) |
| `SESSION_DSN` | (vazio) | Postgres p/ sessões persistentes; vazio = InMemory |
| `GUARD_URL` | (vazio) | uRag-guard-go; vazio = telemetria desligada |
| `GUARD_INGEST_TOKEN` | (vazio) | Token de ingestão do guard |
| `SCIENCE_MCP_URL` | (vazio) | MCP opcional de ciência (RDKit/DeepChem) |
| `AGENT_HTTP_ADDR` | `:8081` | Endereço do servidor HTTP do agente |

## Funcionalidades

### LLM (OpenAI-compatible)
`model/openaicompat` implementa `model.LLM` do ADK para **qualquer** endpoint
OpenAI-compatível (LM Studio, Ollama `/v1`, OpenRouter). Traduz `genai` ↔
OpenAI nos dois sentidos (texto, tool-calling e function responses). Acumula
`tokens_in`/`tokens_out` por run para reporte de custo.

### Memória
`internal/memory` escolhe o backend conforme as env vars:

- **`URAG_VECTOR_MEMORY=true`** → `VectorMemoryService`: usa o uRag-go como
  backend semântico (`vector_add`/`vector_query`), com isolamento lógico por
  `{mem_app, mem_user}`. Fallback silencioso se o uRag-go estiver offline.
- **`MEMORY_DIR` definido** → `FileService`: persistência em JSONL por
  usuário (`{dir}/{app}/{user}.jsonl`), keyword matching (legado).
- **nenhuma das duas** → `InMemoryService` do ADK (sem persistência).

### Sessões
`SESSION_DSN` definido → `session/database` do ADK v2 com **GORM + Postgres**
(auto-migrate na subida). Vazio → `InMemoryService` (Tier 1 fallback).

### Guardrails (input/output)
Consulta regras do [`uRag-guard-go`](../uRag-guard-go) (`GET /v1/rules`,
cache 30s) e avalia antes (input) e depois (output) da geração:

- **regex** — padrão no `config.pattern`.
- **keyword** — lista em `config.keywords`.
- **external_webhook** — `POST` para `config.endpoint` (ex.: RDKit), usa o
  campo `verdict` (`pass`/`flag`/`block`).

`action: "block"` aborta a resposta (e a registra como `blocked` no guard);
`action: "flag"` apenas reporta o evento.

### Evals (background)
Após a resposta, roda em goroutine assíncrona (não atrasa o usuário):
LLM-as-judge de **faithfulness** + evaluators **custom** cadastrados no guard
(`GET /v1/eval-configs?source=urag-adk-agent`). Respeita `sample_rate` (0–1)
por config. Scores reportados via `POST /v1/scores`.

### Telemetria (uRag-guard-go)
Client fino (`internal/guard`, fail-open): reporta a **run** (`/v1/runs`),
**spans** de tool calls (`/v1/spans`, uma goroutine por run), **scores** de
eval (`/v1/scores`) e **eventos de guardrail** (`/v1/guardrail-events`).
`GUARD_URL`/`GUARD_INGEST_TOKEN` vazios → tudo vira no-op. `source` usado:
`urag-adk-agent`.

## Estrutura do projeto

```
urag-adk-go/
├── cmd/agent/            # entrypoint (servidor MCP + loop ReAct)
├── internal/
│   ├── guard/            # cliente fino do uRag-guard-go (runs/spans/scores/eventos)
│   └── memory/           # VectorMemoryService (semântica) + FileService (JSONL)
├── model/openaicompat/   # adapter model.LLM para qualquer endpoint OpenAI-compatível
└── .env                  # exemplo de configuração
```

## Ecossistema uRag

| Projeto | Papel |
|---|---|
| [uRag-go](../uRag-go) | Stores (Vector/Graph/Tree/SQL) consumidas por este agente via MCP |
| **urag-adk-go** (este) | Loop ReAct sobre as stores do uRag-go — substituiu o `uRag-agent-go` |
| [uRag-workflow-go](../uRag-workflow-go) | Orquestra pipelines, chama este agente via `agentNode` |
| [uRag-gateway-go](../uRag-gateway-go) | Fachada REST única, decide entre chamar uRag-go direto ou este agente |
| [urag-front](../urag-front) | UI web (hoje fala direto com uRag-go, não com o agente) |

## Testes

```bash
go build ./... && go vet ./... && go test ./...
```

## Licença

MIT.

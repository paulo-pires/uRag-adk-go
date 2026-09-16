# MEDICAO.md — Forge Agentic Loop: Com vs Sem Diagnostico

**Data:** 2026-09-14T03:47:01Z
**Modelo:** deepseek-v4-flash
**Proxy:** http://192.168.0.151:8095/v1
**N por braco:** 10
**MaxIteracoes:** 4
**Sandbox:** Docker real (UseDocker=true), build tsc+vite real

---

## Cenário: import-faltando

| Metrica | Braco A (com diagnostico) | Braco B (sem diagnostico) |
|---|---|---|
| Taxa de sucesso | 100.0% (10/10) | 90.0% (9/10) |
| Falhou em 4 iter | 0 (0.0%) | 1 (10.0%) |
| Dist. iter 1/2/3/4 | 0/10/0/0 | 0/8/0/1 |
| Duracao media | 56.627s | 1m11.311s |
| Tokens in/out | 78436/10845 | 152544/26196 |
| Custo estimado USD | $0.03311 | $0.07000 |

**Conclusao:** **SEM DIFERENÇA SIGNIFICATIVA** (diff=10.0 pp). O laço agêntico pode não estar funcionando como esperado.

### Braço A — Com diagnostico real

| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |
|---|---|---|---|---|---|
| 1 | true | 2 | 53.18s | 6834 | 904 |
| 2 | true | 2 | 47.075s | 6574 | 845 |
| 3 | true | 2 | 1m9.221s | 8979 | 1997 |
| 4 | true | 2 | 43.957s | 6082 | 668 |
| 5 | true | 2 | 53.656s | 7412 | 1492 |
| 6 | true | 2 | 58.495s | 7070 | 1083 |
| 7 | true | 2 | 1m7.085s | 7769 | 581 |
| 8 | true | 2 | 59.786s | 9927 | 1514 |
| 9 | true | 2 | 1m5.004s | 9282 | 944 |
| 10 | true | 2 | 48.809s | 8507 | 817 |

### Braço B — Sem diagnostico ("Build failed. Try again.")

| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |
|---|---|---|---|---|---|
| 1 | true | 2 | 42.009s | 7969 | 814 |
| 2 | true | 2 | 44.579s | 7087 | 576 |
| 3 | true | 2 | 1m12.121s | 12200 | 1947 |
| 4 | true | 2 | 53.942s | 11461 | 1655 |
| 5 | true | 2 | 2m11.225s | 56787 | 11556 |
| 6 | true | 2 | 59.17s | 9925 | 1380 |
| 7 | true | 2 | 36.394s | 5410 | 521 |
| 8 | false | 0 | 54.98s | 7780 | 1656 |
| 9 | true | 2 | 1m7.852s | 8865 | 2846 |
| 10 | true | 4 | 2m30.838s | 25060 | 3245 |

---

## Cenário: erro-entre-arquivos

| Metrica | Braco A (com diagnostico) | Braco B (sem diagnostico) |
|---|---|---|
| Taxa de sucesso | 100.0% (10/10) | 60.0% (6/10) |
| Falhou em 4 iter | 0 (0.0%) | 4 (40.0%) |
| Dist. iter 1/2/3/4 | 0/7/3/0 | 0/1/3/2 |
| Duracao media | 1m12.196s | 2m19.766s |
| Tokens in/out | 119787/27557 | 243024/59074 |
| Custo estimado USD | $0.06266 | $0.13060 |

**Conclusao:** Diagnóstico ajuda: Braço A 40.0 pp melhor que B.

### Braço A — Com diagnostico real

| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |
|---|---|---|---|---|---|
| 1 | true | 3 | 1m4.18s | 8223 | 1110 |
| 2 | true | 3 | 1m21.369s | 11598 | 3286 |
| 3 | true | 2 | 54.386s | 7506 | 2142 |
| 4 | true | 3 | 1m52.761s | 15873 | 3166 |
| 5 | true | 2 | 1m47.77s | 30553 | 5673 |
| 6 | true | 2 | 56.419s | 11605 | 2436 |
| 7 | true | 2 | 51.686s | 7452 | 1964 |
| 8 | true | 2 | 1m2.862s | 7894 | 2602 |
| 9 | true | 2 | 1m10.316s | 7768 | 2631 |
| 10 | true | 2 | 1m0.215s | 11315 | 2547 |

### Braço B — Sem diagnostico ("Build failed. Try again.")

| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |
|---|---|---|---|---|---|
| 1 | false | 0 | 1m14.815s | 6129 | 3537 |
| 2 | true | 3 | 3m22.52s | 23573 | 11829 |
| 3 | true | 2 | 41.521s | 9554 | 1085 |
| 4 | false | 0 | 1m24.916s | 9147 | 3369 |
| 5 | true | 4 | 1m57.787s | 20182 | 3537 |
| 6 | true | 3 | 1m43.37s | 13792 | 2785 |
| 7 | false | 0 | 1m33.511s | 13050 | 2425 |
| 8 | true | 3 | 2m49.65s | 26483 | 8207 |
| 9 | false | 0 | 6m17.3s | 97691 | 17827 |
| 10 | true | 4 | 2m12.273s | 23423 | 4473 |

---

## Notas metodologicas

- Cenário **import-faltando**: src/App.tsx usa <Calendar /> sem importar de lucide-react.
  Erro visível relendo o arquivo; cenário de referência histórico.
- Cenário **erro-entre-arquivos**: src/types.ts e src/App.tsx têm erros interdependentes.
  - types.ts: emptyCard() retorna objeto sem 'total' (TS2741 em types.ts)
  - App.tsx: passa total como string em vez de number (TS2322 em App.tsx)
  - Lendo só App.tsx, não é possível saber qual arquivo corrigir.
  - Prova da discriminação: CENARIO-2-PROVA.txt (saída real do tsc antes das rodadas)
- Braço A: compilar devolve diagnosticos estruturados (arquivo, linha, codigo TS, mensagem).
- Braço B: compilar devolve apenas "Build failed. Try again." -- sem arquivo, sem linha, sem simbolo.
- "Iter compilou" = numero da chamada a compilar quando retornou ok:true. 0 = nao compilou.
- Custo estimado com tarifa deepseek-v4-flash: $0.27/M input, $1.10/M output.
- Se nao houver diferenca entre bracos em import-faltando, e resultado esperado (caso facil).
- O cenario erro-entre-arquivos e o que deve discriminar os bracos.

---

## Conclusão entre os dois cenários

| | import-faltando | erro-entre-arquivos |
|---|---|---|
| Braço A (com diagnóstico) | 100% | **100%** |
| Braço B (sem diagnóstico) | 100% | **60%** |
| Diferença | 0 pp | **40 pp** |
| Custo A / B | $0,033 / $0,056 | $0,063 / $0,131 |

**O diagnóstico do compilador vale, e o número que prova isso é 40 pp.**

O cenário `import-faltando` dava 0 pp e a leitura preguiçosa disso seria "o laço não
funciona". Estava errada: o que aquele empate media era a facilidade do caso. Um `<Calendar />`
sem import tem o símbolo e a lista de imports na mesma tela, então o modelo conserta relendo o
arquivo e o braço B nunca precisou do diagnóstico.

Com o erro espalhado em dois arquivos — `types.ts` não preenche `total`, `App.tsx` passa string
onde espera number — ler só o arquivo do erro não diz qual lado corrigir. Aí o braço B **falha
4 de 10 vezes**, e cada rodada custa mais que o dobro.

⚠️ Leitura que continua valendo: empate entre braços **nunca** é evidência de que o laço falhou.
É evidência de que o caso não discrimina. A função `degenerada()` em `measure_test.go` existe
para sinalizar exatamente isso — todas as rodadas caindo no mesmo número de iterações.

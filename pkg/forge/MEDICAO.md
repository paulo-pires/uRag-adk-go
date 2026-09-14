# MEDICAO.md — Forge Agentic Loop: Com vs Sem Diagnostico

**Data:** 2026-09-13T23:57:32Z
**Modelo:** deepseek-v4-flash
**Proxy:** http://192.168.0.151:8095/v1
**N por braco:** 10
**MaxIteracoes:** 4
**Sandbox:** Docker real (UseDocker=true), build tsc+vite real

---

## Resumo

| Metrica | Braco A (com diagnostico) | Braco B (sem diagnostico) |
|---|---|---|
| Taxa de sucesso | 100.0% (10/10) | 100.0% (10/10) |
| Falhou em 4 iter | 0 (0.0%) | 0 (0.0%) |
| Dist. iter 1/2/3/4 | 0/10/0/0 | 0/10/0/0 |
| Duracao media | 47.041s | 51.737s |
| Tokens in/out | 79878/10770 | 124714/20213 |
| Custo estimado USD | $0.03341 | $0.05591 |

**Conclusao (corrigida na revisao):** nao houve diferenca na TAXA DE SUCESSO (ambos 100%), mas
houve diferenca grande no CUSTO. Ver a secao "Leitura" abaixo -- a conclusao original dizia que o
laco "pode nao estar funcionando", e os dados nao sustentam isso.

---

## Braco A -- Com diagnostico real (arquivo, linha, codigo TS)

| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |
|---|---|---|---|---|---|
| 1 | true | 2 | 53.657s | 13146 | 2079 |
| 2 | true | 2 | 47.145s | 8439 | 1515 |
| 3 | true | 2 | 44.882s | 6113 | 587 |
| 4 | true | 2 | 48.666s | 7835 | 1455 |
| 5 | true | 2 | 48.468s | 11822 | 1520 |
| 6 | true | 2 | 44.44s | 6607 | 794 |
| 7 | true | 2 | 47.97s | 6629 | 782 |
| 8 | true | 2 | 44.393s | 5893 | 433 |
| 9 | true | 2 | 46.757s | 6985 | 957 |
| 10 | true | 2 | 44.028s | 6409 | 648 |

## Braco B -- Sem diagnostico ("Build failed. Try again.")

| Rodada | Sucesso | Iter compilou | Duracao | Tokens In | Tokens Out |
|---|---|---|---|---|---|
| 1 | true | 2 | 54.203s | 15755 | 2550 |
| 2 | true | 2 | 44.445s | 6587 | 1072 |
| 3 | true | 2 | 1m12.051s | 27350 | 3877 |
| 4 | true | 2 | 32.803s | 5395 | 436 |
| 5 | true | 2 | 59.307s | 13205 | 2888 |
| 6 | true | 2 | 52.405s | 7283 | 1572 |
| 7 | true | 2 | 57.043s | 19862 | 3495 |
| 8 | true | 2 | 47.52s | 11179 | 1531 |
| 9 | true | 2 | 53.033s | 11701 | 1775 |
| 10 | true | 2 | 44.563s | 6397 | 1017 |

---

## Notas metodologicas

- Ambos os bracos partem do mesmo app quebrado: src/App.tsx com Calendar nao importado.
- O prompt inicial e identico; so o retorno da ferramenta compilar difere entre os bracos.
- Braco A: compilar devolve diagnosticos estruturados (arquivo, linha, codigo TS2304, mensagem).
- Braco B: compilar devolve apenas "Build failed. Try again." -- sem arquivo, sem linha, sem simbolo.
- "Iter compilou" = numero da chamada a compilar quando retornou ok:true. 0 = nao compilou.
- Custo estimado com tarifa deepseek-v4-flash: $0.27/M input, $1.10/M output.
- Se nao houver diferenca entre bracos, esse e o resultado principal.

---

## Leitura (revisao, 2026-09-14)

### O laco FUNCIONA. O caso de teste e que nao discrimina.

**As 20 rodadas dos dois bracos convergiram em exatamente 2 iteracoes.** Zero variancia, em nenhuma
direcao. Uma medicao em que todas as tentativas dao o mesmo resultado nao consegue distinguir as
condicoes -- o `diff=0.0 pp` mede a facilidade do caso, nao a inutilidade do diagnostico.

O caso de partida e `src/App.tsx` com `Calendar` nao importado. O modelo **le o arquivo inteiro** a
cada tentativa: o simbolo usado e a lista de imports estao os dois na tela, e o erro e obvio sem
ninguem apontar. O bracco B nao precisou do diagnostico porque a informacao ja estava no contexto
por outro caminho.

Conclusao correta dos dados: o laco converge 100% com build real em Docker e modelo real. O que
este experimento **nao** respondeu e se o diagnostico estruturado ajuda em erro dificil.

### Onde o diagnostico pagou: custo, nao acerto

| | Braco A (com diag) | Braco B (sem diag) | Diferenca |
|---|---|---|---|
| Tokens de entrada | 79.878 | 124.714 | **B gasta 56% a mais** |
| Tokens de saida | 10.770 | 20.213 | **B gasta 88% a mais** |
| Custo | $0.03341 | $0.05591 | **B custa 67% a mais** |
| Duracao media | 47,0s | 51,7s | B 10% mais lento |

Convergir as cegas sai mais caro. Em producao, com muitas geracoes, 67% e a diferenca que importa --
e ela nao aparece se a metrica olhada for so taxa de sucesso.

### O que falta medir

Este experimento precisa de um caso **que o modelo nao resolva so relendo o arquivo**:

- erro de tipo que so aparece na composicao de dois arquivos (o modelo teria que abrir os dois);
- incompatibilidade de versao de dependencia (a mensagem do `tsc` e a unica pista);
- erro em arquivo que o modelo nao escreveu nesta rodada;
- erro de config (`tsconfig`, `vite.config`) que nao aparece no componente.

Com um desses, `diff=0.0 pp` passaria a significar alguma coisa. Hoje nao significa.

### Ressalva sobre a coluna "Iter compilou"

Nao e iteracao do `LoopAgent`: e quantas vezes o modelo chamou a ferramenta `compilar()`. Por isso a
rodada anterior chegou a registrar `iter=5` com `maxIter=4`. Para o que se quer saber aqui
("quantos builds ate compilar") a metrica serve, mas nao confunda com o teto do laco.

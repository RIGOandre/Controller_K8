# preview-operator

Operator que sobe **um ambiente efêmero por pull request** no Kubernetes e o
derruba no merge ou no fim do TTL.

O CI do repositório da aplicação não cria namespace, deployment nem ingress.
Ele escreve um objeto dizendo *"o PR 42 quer a imagem X no ar por 24h"* — e o
cluster converge sozinho.

```yaml
apiVersion: preview.rigo.dev/v1alpha1
kind: PreviewEnvironment
metadata:
  name: pr-42
  namespace: previews
spec:
  repository: acme/loja
  pullRequest: 42
  commit: 9f1c2ab
  image: ghcr.io/acme/loja:9f1c2ab
  port: 3000
  ttl: 8h
```

```
$ kubectl get previews -n previews
NAME    PR          #    PHASE   URL                                    EXPIRA   IDADE
pr-42   acme/loja   42   Ready   https://pr-42-acme-loja.preview.dev    7h58m    2m
```

---

## O problema

Revisar um PR de front-end lendo diff é chute. A alternativa de costume é um
ambiente de staging só, disputado por todo mundo: quem sobe por último ganha,
e a revisão do PR anterior vira ficção.

Ambiente por PR resolve, e é fácil de montar errado. Os dois jeitos que já vi
falhar:

- **Script no CI que roda `kubectl apply`.** Funciona até o job morrer no meio.
  Aí ninguém apaga nada, e o cluster acumula namespace órfão até estourar.
- **Um `kubectl delete` no fechamento do PR.** Só que PR fechado sem merge, PR
  abandonado e job de limpeza que falhou não avisam ninguém.

A diferença de um operator é que a limpeza não depende de o CI ter chegado ao
fim. O estado desejado está no cluster; se algo diverge, o laço corrige na
próxima passada.

---

## Como usar

**1. Instale o operator** (CRD, RBAC e manager):

```bash
make deploy IMG=ghcr.io/rigoandre/preview-operator:v0.1.0
```

**2. Copie o workflow** de [`examples/github-actions/preview.yml`](examples/github-actions/preview.yml)
para o repositório da aplicação. Ele constrói a imagem do PR, aplica o
`PreviewEnvironment`, espera a condition `Ready` e comenta a URL no pull
request — editando o comentário que já existe, em vez de empilhar um por push.

**3. No fechamento do PR**, o mesmo workflow apaga o CR. O finalizer leva o
namespace junto, e o namespace leva o resto.

### O que sobe por ambiente

Um namespace só dele, com `ResourceQuota`, rótulos de Pod Security Admission,
Deployment, Service e Ingress. O host sai de `--base-domain`:
`pr-<número>-<owner>-<repo>.<domínio>`.

### Flags do manager

| Flag | Default | Para quê |
|---|---|---|
| `--base-domain` | vazio | Domínio dos previews. Vazio, o ambiente sobe sem Ingress |
| `--ingress-class` | vazio | `ingressClassName` dos Ingress criados |
| `--quota-cpu` / `--quota-memory` | `2` / `2Gi` | Teto do namespace do preview |
| `--quota-pods` | `10` | Teto de pods do namespace |
| `--default-cpu` / `--default-memory` | `500m` / `512Mi` | Limite do container quando o spec não pede |
| `--max-concurrent-reconciles` | `4` | Reconciles simultâneos |
| `--leader-elect` | `false` | Eleição entre réplicas do manager |

As quantidades são validadas no boot. Um `2Gii` digitado errado derruba o
processo na hora, em vez de virar `ResourceQuota` inválida no primeiro pull
request que aparecer.

---

## As decisões

O detalhe está em [`docs/arquitetura.md`](docs/arquitetura.md). O resumo:

**A limpeza é um finalizer, não `ownerReferences`.** Um dono *namespaced* não
pode ser dono de um `Namespace`, que é *cluster-scoped* — o garbage collector
recusa a relação e apaga o filho na hora. Deployment, Service e Ingress também
ficam de fora, por morarem em outro namespace que o do dono. Sobra apagar o
namespace e deixar o cascade nativo levar o resto.

**O finalizer só se solta quando o namespace some de verdade**, não quando o
`DELETE` retorna. Namespace fica em `Terminating` por um tempo e às vezes trava
ali; soltar antes deixaria um namespace zumbi sem nada apontando para a origem.

**O TTL conta da criação do objeto, não do último reconcile.** Se contasse do
reconcile, bastaria editar o spec de hora em hora para o ambiente viver para
sempre — o oposto do que o TTL existe para fazer.

**O despertar é `RequeueAfter` no instante do vencimento**, não varredura
periódica. Cada ambiente acorda uma vez, na hora dele.

**O selector do Deployment só é escrito na criação.** Ele é imutável: antes de
o reconcile parar de reescrevê-lo, o primeiro commit novo em qualquer PR
travava o ambiente com erro de campo imutável.

**Vencido, o ambiente cai mas o CR fica.** O objeto vira o registro de que
aquele PR teve ambiente e de quando ele caiu.

**Nome e rótulo de métrica são interface pública.** O alerta do cluster e o
painel do Grafana moram em outro repositório e casam por string: trocar o
valor `error` por `erro` não quebra compilação, quebra o alerta — que
simplesmente para de disparar. Por isso o contrato está travado em teste.

**O operator lê secret no cluster inteiro.** É a regra mais larga do
`ClusterRole` e existe por um motivo só: copiar o pull secret do registry
privado para o namespace do preview, já que `imagePullSecrets` é uma referência
local ao namespace do pod. Quem publica imagem pública pode apagar a regra e o
resto continua funcionando. Secret fica fora do cache do manager — com watch em
tudo, o processo guardaria na memória todo secret de todo namespace, e ele é o
alvo mais valioso do cluster.

**Copiar um secret exige consentimento de quem é dono dele.** O nome em
`spec.imagePullSecrets` é texto livre escolhido por quem abre o pull request.
Sem trava, bastava citar qualquer secret do namespace do CR para o operator
entregá-lo a um pod que roda código de PR — o operator emprestando o acesso
dele a uma requisição que não carrega autorização nenhuma. Hoje são três
travas: só tipo `dockerconfigjson`, só a chave do registry, e só com o rótulo
`preview.rigo.dev/copiavel: "true"` no secret de origem.

**`spec.env` aceita valor literal e nada mais.** `valueFrom` resolveria no
namespace do preview — que é onde o pull secret acabou de pousar. O CRD recusa
e o controller descarta: duas trancas, porque um cluster com o CRD antigo
reabriria o caminho sozinho.

**O domínio é decisão do cluster, não do pull request.** Por isso `spec.subdomain`
é um rótulo DNS, não um hostname inteiro: aceitando hostname completo, quem abre
o PR reivindicaria qualquer nome no ingress controller compartilhado — inclusive
um nome interno que ainda não tem Ingress — e passaria a servi-lo a partir do
container dele.

**`spec.ttl` é `string`, não `metav1.Duration`.** A diferença é entre um objeto
ruim e um cluster parado: `metav1.Duration` decodifica dentro do informer, e um
`ttl: "24 horas"` digitado em qualquer namespace faria a LIST inteira falhar em
laço — o controller pararia de reconciliar **todos** os ambientes do cluster,
sem nada no status de ninguém. Com string, o pior caso é um ambiente cair no TTL
padrão. O schema recusa o formato errado; o parse no controller é tolerante de
propósito.

**`repository` e `pullRequest` são imutáveis.** O nome do namespace deriva
deles: editar trocaria o destino e abandonaria o namespace antigo com o workload
dentro, consumindo quota sem nenhum objeto que aponte para ele.

---

## Testes

```
$ go test ./... -race -cover
ok  github.com/RIGOandre/preview-operator/api/v1alpha1        coverage: 37.3%
ok  github.com/RIGOandre/preview-operator/internal/controller  coverage: 88.4%
```

60 casos, em duas camadas. Os 37% do pacote da API são cobertura diluída pelo
`zz_generated.deepcopy.go`, que é gerado e não tem decisão dentro.

**Com `fake client`**, sem etcd e sem apiserver, para a decisão do reconcile —
que é onde mora a lógica. Rodam em 50ms.

**Com `envtest`**, contra um kube-apiserver e um etcd de verdade, para o que o
client falso não alcança: o schema do CRD, os defaults que vêm dele, a
validação que o apiserver aplica e o status como subresource real.

A segunda camada se pagou no primeiro dia. `spec.ttl` era `metav1.Duration`,
que é struct — e `omitempty` não omite struct. O campo ia na requisição como
`"0s"` mesmo sem ninguém ter pedido, o apiserver via valor presente e o
default de 24h do CRD nunca era aplicado. Nenhum teste com client falso
pegaria isso: lá o default do CRD não existe. Hoje `ttl` é ponteiro.

O que os testes seguram, em ordem de importância:

| Caso | O que quebraria sem ele |
|---|---|
| Finalizer antes de qualquer objeto | Namespace órfão consumindo quota para sempre |
| Segunda passada não reescreve nada | Enxurrada de update idêntico no apiserver |
| Commit novo não mexe no selector | Ambiente travado no primeiro push depois de aberto |
| Finalizer segura o CR até o namespace sumir | Namespace zumbi sem dono |
| Namespace de terceiro não é apagado | Um preview virar incidente |
| Requeue no instante do vencimento | Ambiente vivo além do TTL, ou varredura cara |
| O apiserver recusa spec inválido | Marcação de validação virar comentário decorativo |
| Os defaults do CRD chegam ao objeto | Ambiente sem TTL, vivo para sempre |
| Nome e rótulo de cada métrica | Alerta que para de disparar sem quebrar build nenhum |
| Namespace que já existe não é adotado | A guarda do teardown conferir o rótulo que o próprio apply escreveu |
| Dois ambientes com o mesmo nome derivado não se derrubam | Um preview apagar o preview de outro time |
| Ready exige a revisão atual no ar | Revisor aprovar o PR olhando o commit anterior |
| Erro no apply não carimba observedGeneration | Status afirmando convergência que não houve |
| Vencimento no meio da passada derruba o ambiente | Requeue negativo, descartado em silêncio, ambiente vivo por horas |
| O apiserver recusa `ttl` que o Go não decodifica | Um CR digitado errado parar o operator inteiro |
| `env` com `valueFrom` não chega ao container | PR lendo qualquer secret que o operator alcance |

Três deles nasceram falhando e apontaram erro meu: status de Deployment é
subresource até no client falso, a derrubada leva uma passada a mais porque o
namespace fica em `Terminating`, e o `ttl` que nunca recebia default.

O CI ainda valida que o CRD e o RBAC gerados das marcações estão em dia com o
código, e passa `kubeconform` em todo manifest — inclusive no sample do
`PreviewEnvironment`, contra o schema extraído do próprio CRD. Sem esse passo o
sample passava *pulado*, e um campo digitado errado chegaria ao cluster.

---

## A revisão

Depois de o repositório estar de pé e o CI verde, rodei uma revisão adversarial
em cima dele: seis leitores independentes, um por dimensão de risco
(reconcile, finalizer, uso da API do Kubernetes, métricas, segurança, e "os
testes provam o que dizem provar"), e cada achado passou por dois céticos
encarregados de refutá-lo — um relendo o código, outro obrigado a escrever um
teste que reproduzisse o problema.

Saíram 24 achados; 23 sobreviveram. Os piores não eram bugs de digitação:

- O ambiente anunciava **Ready** logo depois de um commit novo, lendo o status
  de uma revisão do Deployment que ainda não tinha rolado. Com `replicas: 1` o
  `maxUnavailable` padrão é zero, então o pod antigo continua pronto para
  sempre se a imagem nova não sobe — o revisor aprovaria o PR olhando o commit
  anterior, e o gate do workflow liberaria o merge.
- Um `ttl: "24 horas"` em **qualquer** namespace parava o operator inteiro.
- O `CreateOrUpdate` do namespace **adotava** um namespace que já existisse e
  carimbava nele o rótulo `managed-by` — a guarda do teardown, que existe para
  não apagar namespace de terceiro, passou a conferir o rótulo que o próprio
  apply tinha acabado de escrever.
- `RequeueAfter` podia sair negativo quando o TTL vencia no meio da passada, e
  o controller-runtime só agenda com valor positivo: o despertar sumia sem erro
  nenhum, e o ambiente ficava de pé até o resync do informer, que é de horas.

Cada correção tem um teste. E cada teste foi conferido quebrando a correção de
propósito, para provar que ele falha sem ela — dois deles passavam pelo motivo
errado e precisaram ser reescritos, e um só era possível contra um apiserver de
verdade, porque o `fake client` não faz a contabilidade de `metadata.generation`.

## Estado

`v1alpha1`, rodando em cluster próprio. O que falta, na ordem em que pretendo
resolver:

- `NetworkPolicy` de egress padrão-nega no namespace do preview. Hoje o pod
  alcança a rede interna do cluster, o que é insuficiente para PR de fork.
- Banco efêmero por ambiente. Hoje o preview aponta para um banco que já
  existe, e migração destrutiva num PR afeta os outros.
- Webhook de validação. O CRD já barra o que dá para expressar em schema, mas
  não uma imagem de registry não permitido.

O grupo da API é `preview.rigo.dev`. Antes de instalar em cluster
compartilhado, troque pelo domínio que você controla.

## Licença

MIT.

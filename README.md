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
make deploy IMG=ghcr.io/rigoandre/controller_k8:v0.1.0
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

**O operator lê secret no cluster inteiro.** É a regra mais larga do
`ClusterRole` e existe por um motivo só: copiar o pull secret do registry
privado para o namespace do preview, já que `imagePullSecrets` é uma referência
local ao namespace do pod. Quem publica imagem pública pode apagar a regra e o
resto continua funcionando.

---

## Testes

```
$ go test ./... -race -cover
ok  github.com/RIGOandre/preview-operator/api/v1alpha1        coverage: 35.0%
ok  github.com/RIGOandre/preview-operator/internal/controller  coverage: 82.4%
```

24 casos, sem etcd e sem apiserver: o `fake client` do controller-runtime
basta para o que está sob teste, que é a decisão do reconcile. Os 35% do
pacote da API são cobertura diluída pelo `zz_generated.deepcopy.go`, que é
gerado e não tem decisão dentro.

O que os testes seguram, em ordem de importância:

| Caso | O que quebraria sem ele |
|---|---|
| Finalizer antes de qualquer objeto | Namespace órfão consumindo quota para sempre |
| Segunda passada não reescreve nada | Enxurrada de update idêntico no apiserver |
| Commit novo não mexe no selector | Ambiente travado no primeiro push depois de aberto |
| Finalizer segura o CR até o namespace sumir | Namespace zumbi sem dono |
| Namespace de terceiro não é apagado | Um preview virar incidente |
| Requeue no instante do vencimento | Ambiente vivo além do TTL, ou varredura cara |

Dois deles nasceram falhando e apontaram erro meu: status de Deployment é
subresource até no client falso, e a derrubada leva uma passada a mais porque
o namespace fica em `Terminating`.

O CI ainda valida que o CRD e o RBAC gerados das marcações estão em dia com o
código, e passa `kubeconform` em todo manifest — inclusive no sample do
`PreviewEnvironment`, contra o schema extraído do próprio CRD. Sem esse passo o
sample passava *pulado*, e um campo digitado errado chegaria ao cluster.

---

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

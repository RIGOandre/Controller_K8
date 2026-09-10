# Como o operator funciona por dentro

## O laço

`Reconcile` roda inteiro a cada evento e não guarda nada entre chamadas. Ele
não confia no que o status diz: lê o cluster, compara com o spec e escreve a
diferença. Rodar duas vezes seguidas não muda nada na segunda — o teste
`TestSegundaPassadaNaoEscreveDeNovo` compara o `resourceVersion` do Deployment
antes e depois justamente para provar isso.

A ordem importa:

1. **Objeto sumiu** → limpa as métricas com rótulo por objeto e encerra.
2. **Objeto em remoção** → vai para o finalizer.
3. **Sem finalizer** → grava o finalizer e sai. Nada é criado nesta passada.
4. **TTL vencido** → derruba o ambiente e mantém o CR.
5. **Caso normal** → aplica os objetos, lê o Deployment, publica o status e
   pede requeue no instante do vencimento.

O passo 3 sai cedo de propósito. Criar o namespace antes de gravar o finalizer
abre uma janela: se o CR for removido nesse intervalo, o namespace fica órfão
consumindo quota sem ninguém que saiba de onde veio.

## Por que finalizer, e não ownerReference

O caminho normal de limpeza no Kubernetes é `ownerReferences`: o garbage
collector apaga o filho quando o dono some. Aqui ele não serve, por duas
razões independentes:

- O dono (`PreviewEnvironment`) é *namespaced* e o `Namespace` é
  *cluster-scoped*. O GC recusa essa relação — pior, ele trata o dono como
  inexistente e apaga o filho na hora.
- Deployment, Service e Ingress vivem em **outro namespace** que o do dono, e
  ownerReference entre namespaces também não vale.

Sobra uma limpeza só: apagar o namespace e deixar o cascade nativo levar o
resto. É o que o finalizer faz — e ele só se solta quando o namespace some de
verdade, não quando o `DELETE` retorna. Namespace fica em `Terminating` por um
tempo, e às vezes trava ali; soltar o finalizer antes deixaria um namespace
zumbi sem nenhum objeto que apontasse para a origem dele.

## O TTL

O vencimento conta a partir de `metadata.creationTimestamp`, não do último
reconcile. Se contasse do reconcile, bastaria alguém editar o spec de hora em
hora para o ambiente viver para sempre — exatamente o que o TTL existe para
impedir.

O despertar é `RequeueAfter: expiry - now`, calculado na saída de cada
passada. Não há varredura periódica: uma varredura custaria uma passada em
todos os ambientes a cada tique para acertar o vencimento de um.

Vencido, o operator derruba o namespace mas **mantém o CR**. O objeto vira o
registro de que aquele PR teve ambiente e de quando ele caiu. Quem apaga o CR
é o workflow do repositório, no merge ou no fechamento do PR.

## O caminho de volta

`Owns()` depende de ownerReference, que aqui não existe. Então o controller
observa Deployments e Ingresses e volta ao dono pelos rótulos que ele mesmo
carimba:

```
preview.rigo.dev/owner-namespace
preview.rigo.dev/owner-name
```

São dois rótulos e não um `ns/nome` porque valor de rótulo não aceita barra e
não passa de 63 caracteres.

## O que segura o código do PR

Um preview roda o build de um pull request, às vezes de fork. O namespace
nasce com três defesas:

| Defesa | O que impede |
|---|---|
| `ResourceQuota` | Um loop infinito no PR consumir o cluster inteiro |
| Rótulos de Pod Security Admission (`baseline`) | Pod privilegiado, host network, montagem do host |
| `securityContext` no container | Escalada de privilégio e capabilities herdadas |

O que **não** está aqui: `NetworkPolicy`. Hoje o pod do preview alcança a rede
interna do cluster. Para preview de PR de fork isso é insuficiente, e a
correção é uma policy de egress padrão-nega no namespace.

## Nomes

Namespace e host saem de uma função pura do spec — nunca do status. Se o
processo morrer entre criar o namespace e gravar o status, o próximo reconcile
ainda sabe exatamente onde procurar o que já existe.

O corte em 63 caracteres não é cego: nomes longos perdem o meio e ganham oito
caracteres de hash do nome inteiro. Sem isso, dois repositórios com prefixo
igual e nome longo virariam o mesmo namespace, e um preview apagaria o outro.

## Métricas

Quatro séries, em `/metrics`, no registry do controller-runtime:

| Série | Para quê |
|---|---|
| `preview_environments_active` | Quantos ambientes existem agora |
| `preview_environment_transitions_total{phase}` | Quantos subiram, venceram, falharam |
| `preview_environment_reconcile_duration_seconds{result}` | Se o laço está lento |
| `preview_environment_expires_at_seconds{namespace,name}` | Alertar antes de o preview sumir debaixo de quem revisa |

A última sai da série quando o ambiente cai. Sem isso o Prometheus continuaria
mostrando o vencimento de um preview apagado meses antes, e todo alerta em
cima dela ficaria preso no passado.

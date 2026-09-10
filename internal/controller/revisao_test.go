package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus/client_golang/prometheus/testutil"

	previewv1alpha1 "github.com/RIGOandre/preview-operator/api/v1alpha1"
)

// Este arquivo guarda um teste por achado da revisão adversarial. Cada um
// falha se a correção correspondente for desfeita.

// O nome do namespace deriva de repositório e PR, e nada impede que já exista
// um namespace com esse nome. Adotá-lo carimbava nele o rótulo managed-by e o
// guard do teardown passava a conferir o rótulo que o próprio apply tinha
// acabado de escrever — a guarda protegia contra si mesma.
func TestNaoAdotaNamespaceQueJaExiste(t *testing.T) {
	pe := novoAmbiente()
	alheio := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   pe.NamespaceName(),
			Labels: map[string]string{"dono": "outro-time"},
		},
	}
	s := monta(t, criacao, pe, alheio)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	var ns corev1.Namespace
	if err := s.c.Get(ctx, types.NamespacedName{Name: pe.NamespaceName()}, &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels[previewv1alpha1.LabelManagedBy] != "" {
		t.Fatalf("o operator carimbou managed-by num namespace que não é dele: %v", ns.Labels)
	}
	if ns.Labels["dono"] != "outro-time" {
		t.Fatalf("os rótulos do dono original foram mexidos: %v", ns.Labels)
	}

	var deploy appsv1.Deployment
	err := s.c.Get(ctx, types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}, &deploy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("subiu workload dentro do namespace de terceiro: %v", err)
	}

	atual := s.lerAmbiente(t, pe)
	if atual.Status.Phase != previewv1alpha1.PhaseFailed {
		t.Fatalf("queria Failed, veio %q", atual.Status.Phase)
	}
}

// Dois PreviewEnvironment em namespaces diferentes, mesmo repositório e mesmo
// PR, derivam o mesmo nome de namespace. Com o guard conferindo só managed-by,
// o segundo adotava o ambiente do primeiro — e apagá-lo derrubava o preview
// alheio.
func TestDoisAmbientesComOMesmoNomeDerivadoNaoSeDerrubam(t *testing.T) {
	a := novoAmbiente(func(p *previewv1alpha1.PreviewEnvironment) {
		p.Name, p.Namespace = "a", "time-a"
	})
	b := novoAmbiente(func(p *previewv1alpha1.PreviewEnvironment) {
		p.Name, p.Namespace = "b", "time-b"
	})
	if a.NamespaceName() != b.NamespaceName() {
		t.Fatalf("o teste só faz sentido com nomes iguais: %s vs %s", a.NamespaceName(), b.NamespaceName())
	}

	s := monta(t, criacao, a, b)
	s.reconcile(t, a)
	s.reconcile(t, a)
	s.reconcile(t, b)
	s.reconcile(t, b)

	if fase := s.lerAmbiente(t, b).Status.Phase; fase != previewv1alpha1.PhaseFailed {
		t.Fatalf("o segundo ambiente devia recusar o namespace ocupado, veio %q", fase)
	}

	ctx := context.Background()
	atualB := s.lerAmbiente(t, b)
	if err := s.c.Delete(ctx, atualB); err != nil {
		t.Fatal(err)
	}
	s.reconcile(t, b)

	var ns corev1.Namespace
	if err := s.c.Get(ctx, types.NamespacedName{Name: a.NamespaceName()}, &ns); err != nil {
		t.Fatalf("apagar o ambiente B derrubou o namespace do ambiente A: %v", err)
	}
	var deploy appsv1.Deployment
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: a.NamespaceName(), Name: deploymentName}, &deploy); err != nil {
		t.Fatalf("o workload do ambiente A não sobreviveu: %v", err)
	}
}

// Entre a checagem do TTL no topo do reconcile e o cálculo do requeue no fim
// roda a passada inteira. Se o vencimento cair nessa janela, a subtração sai
// negativa — e o controller-runtime só agenda com RequeueAfter > 0: negativo
// vira Forget(req) e o despertar some sem erro nenhum.
func TestVencimentoDuranteAPassadaNaoPerdeODespertar(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)

	// Relógio que atravessa o vencimento no meio do reconcile: as duas
	// primeiras leituras ainda estão dentro do TTL, as seguintes já passaram.
	chamadas := 0
	s.r.Now = func() time.Time {
		chamadas++
		if chamadas <= 2 {
			return criacao.Add(59 * time.Minute)
		}
		return criacao.Add(61 * time.Minute)
	}

	res := s.reconcile(t, pe)
	if res.RequeueAfter < 0 {
		t.Fatalf("requeue negativo é descartado em silêncio pelo controller-runtime: %s", res.RequeueAfter)
	}

	atual := s.lerAmbiente(t, pe)
	if atual.Status.Phase != previewv1alpha1.PhaseExpired {
		t.Fatalf("o ambiente atravessou o vencimento e continuou %q", atual.Status.Phase)
	}
}

// Com replicas=1 o maxUnavailable padrão é zero: o pod antigo só cai quando o
// novo fica pronto. Se a imagem nova não sobe, ReadyReplicas fica travado em 1
// e o ambiente anunciaria Ready servindo o commit anterior, para sempre.
func TestNaoDizReadyComRolloutTravadoNaRevisaoAnterior(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	chave := types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}
	var deploy appsv1.Deployment
	if err := s.c.Get(ctx, chave, &deploy); err != nil {
		t.Fatal(err)
	}
	// O pod da revisão ANTERIOR está pronto; o da nova não subiu.
	// Números reais de um rollout travado com replicas=1: o pod antigo continua
	// pronto e disponível, então UnavailableReplicas é zero — o único sinal de
	// que a revisão nova não subiu é UpdatedReplicas.
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.ReadyReplicas = 1
	deploy.Status.UpdatedReplicas = 0
	deploy.Status.UnavailableReplicas = 0
	if err := s.c.Status().Update(ctx, &deploy); err != nil {
		t.Fatal(err)
	}

	s.reconcile(t, pe)

	atual := s.lerAmbiente(t, pe)
	if atual.Status.Phase == previewv1alpha1.PhaseReady {
		t.Fatal("anunciou Ready com a revisão nova sem nenhuma réplica no ar")
	}
	if condicaoVerdadeira(atual, previewv1alpha1.ConditionReady) {
		t.Fatalf("condition Ready ficou True durante rollout travado: %+v", atual.Status.Conditions)
	}
}

func TestRolloutQueEstouraOPrazoViraFailed(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	chave := types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}
	var deploy appsv1.Deployment
	if err := s.c.Get(ctx, chave, &deploy); err != nil {
		t.Fatal(err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:    appsv1.DeploymentProgressing,
		Status:  corev1.ConditionFalse,
		Reason:  "ProgressDeadlineExceeded",
		Message: `ReplicaSet "preview-abc" has timed out progressing.`,
	}}
	if err := s.c.Status().Update(ctx, &deploy); err != nil {
		t.Fatal(err)
	}

	s.reconcile(t, pe)

	atual := s.lerAmbiente(t, pe)
	if atual.Status.Phase != previewv1alpha1.PhaseFailed {
		t.Fatalf("queria Failed com o rollout estourado, veio %q", atual.Status.Phase)
	}
	if atual.Status.URL != "" {
		t.Fatalf("continuou publicando URL de um ambiente que não subiu: %q", atual.Status.URL)
	}
}

// observedGeneration é o campo que um consumidor lê para decidir se pode
// confiar no resto do status. Carimbá-lo no caminho de erro fazia o par
// (phase: Ready, observedGeneration: N) sobreviver a um apply quebrado.
func TestErroNoApplyNaoCarimbaConvergencia(t *testing.T) {
	origem := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ghcr", Namespace: "previews",
			Labels: map[string]string{previewv1alpha1.LabelCopiavel: "true"},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)},
	}
	pe := novoAmbiente(func(p *previewv1alpha1.PreviewEnvironment) {
		p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "ghcr"}}
	})
	s := monta(t, criacao, pe, origem)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	if err := s.c.Delete(ctx, origem); err != nil {
		t.Fatal(err)
	}

	// Nova geração do spec, que agora não tem como convergir.
	atual := s.lerAmbiente(t, pe)
	atual.Spec.Image = "ghcr.io/acme/loja:def5678"
	atual.Generation = 2
	if err := s.c.Update(ctx, atual); err != nil {
		t.Fatal(err)
	}

	s.reconcile(t, pe)

	depois := s.lerAmbiente(t, pe)
	if depois.Status.ObservedGeneration == 2 {
		t.Fatal("carimbou observedGeneration=2 sem ter atendido a geração 2")
	}
	if condicaoVerdadeira(depois, previewv1alpha1.ConditionReady) {
		t.Fatalf("continuou dizendo Ready com o apply falhando: %+v", depois.Status.Conditions)
	}
}

// O CR sobrevive ao vencimento de propósito. Contar esses registros fazia o
// gauge de "ativos" nunca descer: num cluster com duzentos PRs velhos, o painel
// mostraria duzentos ambientes ativos e nenhum namespace existindo.
func TestGaugeDeAtivosNaoContaVencido(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	if got := testutil.ToFloat64(activeEnvironments.WithLabelValues("acme/loja")); got != 1 {
		t.Fatalf("ambiente no ar devia contar 1, veio %v", got)
	}

	s.relogio(criacao.Add(2 * time.Hour))
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	if fase := s.lerAmbiente(t, pe).Status.Phase; fase != previewv1alpha1.PhaseExpired {
		t.Fatalf("o ambiente devia estar Expired, veio %q", fase)
	}
	if got := testutil.ToFloat64(activeEnvironments.WithLabelValues("acme/loja")); got != 0 {
		t.Fatalf("ambiente vencido continuou contando como ativo: %v", got)
	}
}

// ownerFromLabels é o substituto declarado do Owns(), que não funciona aqui
// porque não há ownerReference entre namespaces. Sem teste, desligá-lo por
// completo não quebraria nada — e o status pararia de acompanhar o Deployment.
func TestOwnerFromLabels(t *testing.T) {
	casos := []struct {
		nome    string
		labels  map[string]string
		querido int
	}{
		{"objeto nosso volta ao dono", map[string]string{
			previewv1alpha1.LabelManagedBy:      previewv1alpha1.ManagedByValue,
			previewv1alpha1.LabelOwnerNamespace: "previews",
			previewv1alpha1.LabelOwnerName:      "loja-pr-7",
		}, 1},
		{"objeto de outro operator é ignorado", map[string]string{
			previewv1alpha1.LabelManagedBy:      "outro",
			previewv1alpha1.LabelOwnerNamespace: "previews",
			previewv1alpha1.LabelOwnerName:      "loja-pr-7",
		}, 0},
		{"sem os rótulos do dono não há caminho de volta", map[string]string{
			previewv1alpha1.LabelManagedBy: previewv1alpha1.ManagedByValue,
		}, 0},
		{"sem rótulo nenhum", nil, 0},
	}

	for _, caso := range casos {
		t.Run(caso.nome, func(t *testing.T) {
			obj := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "preview", Labels: caso.labels}}
			pedidos := ownerFromLabels(context.Background(), obj)
			if len(pedidos) != caso.querido {
				t.Fatalf("queria %d pedidos, veio %d (%v)", caso.querido, len(pedidos), pedidos)
			}
			if caso.querido == 1 {
				querido := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "previews", Name: "loja-pr-7"}}
				if pedidos[0] != querido {
					t.Fatalf("voltou para o dono errado: %v", pedidos[0])
				}
			}
		})
	}
}

// A idempotência era conferida só no Deployment. Namespace, quota, Service e
// Ingress podiam estar sendo reescritos a cada passada sem ninguém notar.
func TestSegundaPassadaNaoEscreveEmNenhumObjeto(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	ns := pe.NamespaceName()
	objetos := map[string]client.Object{
		"namespace":     &corev1.Namespace{},
		"resourcequota": &corev1.ResourceQuota{},
		"deployment":    &appsv1.Deployment{},
		"service":       &corev1.Service{},
		"ingress":       &networkingv1.Ingress{},
	}
	chaves := map[string]types.NamespacedName{
		"namespace":     {Name: ns},
		"resourcequota": {Namespace: ns, Name: quotaName},
		"deployment":    {Namespace: ns, Name: deploymentName},
		"service":       {Namespace: ns, Name: serviceName},
		"ingress":       {Namespace: ns, Name: ingressName},
	}

	antes := map[string]string{}
	for nome, obj := range objetos {
		if err := s.c.Get(ctx, chaves[nome], obj); err != nil {
			t.Fatalf("%s: %v", nome, err)
		}
		antes[nome] = obj.GetResourceVersion()
	}

	s.reconcile(t, pe)
	s.reconcile(t, pe)

	for nome, obj := range objetos {
		if err := s.c.Get(ctx, chaves[nome], obj); err != nil {
			t.Fatalf("%s: %v", nome, err)
		}
		if obj.GetResourceVersion() != antes[nome] {
			t.Errorf("%s foi reescrito sem mudança: %s -> %s", nome, antes[nome], obj.GetResourceVersion())
		}
	}
}

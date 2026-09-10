package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	previewv1alpha1 "github.com/RIGOandre/Controller_K8/api/v1alpha1"
)

var criacao = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		BaseDomain:       "preview.exemplo.dev",
		IngressClassName: "nginx",
		QuotaCPU:         resource.MustParse("2"),
		QuotaMemory:      resource.MustParse("2Gi"),
		QuotaPods:        10,
		DefaultResources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
		},
	}
}

func novoAmbiente(mods ...func(*previewv1alpha1.PreviewEnvironment)) *previewv1alpha1.PreviewEnvironment {
	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "loja-pr-7",
			Namespace:         "previews",
			CreationTimestamp: metav1.NewTime(criacao),
			Generation:        1,
		},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository:  "acme/loja",
			PullRequest: 7,
			Commit:      "abc1234",
			Image:       "ghcr.io/acme/loja:abc1234",
			Port:        3000,
			TTL:         &metav1.Duration{Duration: time.Hour},
		},
	}
	for _, m := range mods {
		m(pe)
	}
	return pe
}

type cenario struct {
	r  *PreviewEnvironmentReconciler
	c  client.Client
	pe *previewv1alpha1.PreviewEnvironment
}

// monta um reconciler com client falso. Sem etcd e sem apiserver: o que está
// sob teste é a decisão do reconcile, não o servidor da API.
func monta(t *testing.T, agora time.Time, objs ...client.Object) *cenario {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := previewv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&previewv1alpha1.PreviewEnvironment{}).
		Build()

	return &cenario{
		r: &PreviewEnvironmentReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(256),
			Config:   testConfig(),
			Now:      func() time.Time { return agora },
		},
		c: c,
	}
}

func (s *cenario) reconcile(t *testing.T, pe *previewv1alpha1.PreviewEnvironment) ctrl.Result {
	t.Helper()
	res, err := s.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pe.Namespace, Name: pe.Name},
	})
	if err != nil {
		t.Fatalf("reconcile devolveu erro: %v", err)
	}
	return res
}

func (s *cenario) lerAmbiente(t *testing.T, pe *previewv1alpha1.PreviewEnvironment) *previewv1alpha1.PreviewEnvironment {
	t.Helper()
	var atual previewv1alpha1.PreviewEnvironment
	if err := s.c.Get(context.Background(), client.ObjectKeyFromObject(pe), &atual); err != nil {
		t.Fatalf("não consegui reler o ambiente: %v", err)
	}
	return &atual
}

func (s *cenario) relogio(t time.Time) { s.r.Now = func() time.Time { return t } }

// O finalizer precisa entrar antes de qualquer objeto. Na ordem inversa
// existe uma janela em que o namespace já subiu e uma remoção do CR o
// deixaria órfão, consumindo quota para sempre.
func TestFinalizerEntraAntesDeCriarQualquerCoisa(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)

	s.reconcile(t, pe)

	atual := s.lerAmbiente(t, pe)
	if !controllerutil.ContainsFinalizer(atual, previewv1alpha1.Finalizer) {
		t.Fatal("o finalizer não foi gravado na primeira passada")
	}
	var ns corev1.Namespace
	err := s.c.Get(context.Background(), types.NamespacedName{Name: pe.NamespaceName()}, &ns)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("o namespace subiu antes do finalizer: %v", err)
	}
}

func TestReconcileMontaOAmbienteCompleto(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe) // grava o finalizer
	s.reconcile(t, pe) // aplica

	ctx := context.Background()
	ns := pe.NamespaceName()

	var namespace corev1.Namespace
	if err := s.c.Get(ctx, types.NamespacedName{Name: ns}, &namespace); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if got := namespace.Labels["pod-security.kubernetes.io/enforce"]; got != "baseline" {
		t.Fatalf("PSA não aplicado, veio %q", got)
	}

	var quota corev1.ResourceQuota
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: quotaName}, &quota); err != nil {
		t.Fatalf("resourcequota: %v", err)
	}
	if quota.Spec.Hard.Pods().Value() != 10 {
		t.Fatalf("teto de pods errado: %v", quota.Spec.Hard.Pods())
	}

	var deploy appsv1.Deployment
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: deploymentName}, &deploy); err != nil {
		t.Fatalf("deployment: %v", err)
	}
	container := deploy.Spec.Template.Spec.Containers[0]
	if container.Image != pe.Spec.Image {
		t.Fatalf("imagem errada: %q", container.Image)
	}
	if container.Ports[0].ContainerPort != 3000 {
		t.Fatalf("porta errada: %d", container.Ports[0].ContainerPort)
	}
	if *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("o container subiu podendo escalar privilégio")
	}

	var svc corev1.Service
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: serviceName}, &svc); err != nil {
		t.Fatalf("service: %v", err)
	}
	if svc.Spec.Ports[0].TargetPort.IntValue() != 3000 {
		t.Fatalf("targetPort errado: %v", svc.Spec.Ports[0].TargetPort)
	}

	var ing networkingv1.Ingress
	if err := s.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: ingressName}, &ing); err != nil {
		t.Fatalf("ingress: %v", err)
	}
	if ing.Spec.Rules[0].Host != "pr-7-acme-loja.preview.exemplo.dev" {
		t.Fatalf("host errado: %q", ing.Spec.Rules[0].Host)
	}
	if *ing.Spec.IngressClassName != "nginx" {
		t.Fatalf("ingress class errada: %q", *ing.Spec.IngressClassName)
	}

	atual := s.lerAmbiente(t, pe)
	if atual.Status.Namespace != ns {
		t.Fatalf("status.namespace errado: %q", atual.Status.Namespace)
	}
	if atual.Status.URL != "https://pr-7-acme-loja.preview.exemplo.dev" {
		t.Fatalf("status.url errado: %q", atual.Status.URL)
	}
	if atual.Status.Phase != previewv1alpha1.PhasePending && atual.Status.Phase != previewv1alpha1.PhaseProvisioning {
		t.Fatalf("phase errada com o deployment ainda vazio: %q", atual.Status.Phase)
	}
	if atual.Status.ObservedGeneration != pe.Generation {
		t.Fatalf("observedGeneration não acompanhou o spec: %d", atual.Status.ObservedGeneration)
	}
}

func TestViraReadyQuandoODeploymentFicaPronto(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	var deploy appsv1.Deployment
	chave := types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}
	if err := s.c.Get(ctx, chave, &deploy); err != nil {
		t.Fatal(err)
	}
	deploy.Status.ReadyReplicas = 1
	// Status de Deployment é subresource até no client falso: Update comum
	// descartaria a mudança em silêncio e o teste passaria por acidente.
	if err := s.c.Status().Update(ctx, &deploy); err != nil {
		t.Fatal(err)
	}

	s.reconcile(t, pe)

	atual := s.lerAmbiente(t, pe)
	if atual.Status.Phase != previewv1alpha1.PhaseReady {
		t.Fatalf("queria Ready, veio %q", atual.Status.Phase)
	}
	if atual.Status.ReadyReplicas != 1 {
		t.Fatalf("readyReplicas errado: %d", atual.Status.ReadyReplicas)
	}
	if !condicaoVerdadeira(atual, previewv1alpha1.ConditionReady) {
		t.Fatalf("condition Ready não ficou True: %+v", atual.Status.Conditions)
	}
	if condicaoVerdadeira(atual, previewv1alpha1.ConditionProgressing) {
		t.Fatal("Progressing continuou True depois de pronto")
	}
}

// O requeue tem que cair no instante do vencimento. Uma varredura periódica
// custaria uma passada em todos os ambientes a cada tique para acertar o
// vencimento de um.
func TestRequeueCaiNoInstanteDoVencimento(t *testing.T) {
	pe := novoAmbiente()
	agora := criacao.Add(10 * time.Minute)
	s := monta(t, agora, pe)
	s.reconcile(t, pe)
	res := s.reconcile(t, pe)

	if querido := 50 * time.Minute; res.RequeueAfter != querido {
		t.Fatalf("queria requeue em %s, veio %s", querido, res.RequeueAfter)
	}
}

func TestTTLVencidoDerrubaOAmbienteEGuardaORegistro(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	s.relogio(criacao.Add(2 * time.Hour))

	// A primeira passada pede a remoção e volta para conferir: no cluster de
	// verdade o namespace fica em Terminating por um tempo.
	if res := s.reconcile(t, pe); res.RequeueAfter != terminatingRequeue {
		t.Fatalf("queria conferência em %s, veio %s", terminatingRequeue, res.RequeueAfter)
	}

	ctx := context.Background()
	var ns corev1.Namespace
	if err := s.c.Get(ctx, types.NamespacedName{Name: pe.NamespaceName()}, &ns); !apierrors.IsNotFound(err) {
		t.Fatalf("o namespace sobreviveu ao vencimento: %v", err)
	}

	// Com o namespace fora, o ambiente para de acordar.
	if res := s.reconcile(t, pe); res.RequeueAfter != 0 {
		t.Fatalf("continuou acordando depois de derrubado: %s", res.RequeueAfter)
	}

	// O CR fica: é o registro de que o PR teve ambiente e de quando caiu.
	// Quem apaga o CR é o workflow do repositório, no merge ou no fechamento.
	atual := s.lerAmbiente(t, pe)
	if atual.Status.Phase != previewv1alpha1.PhaseExpired {
		t.Fatalf("queria Expired, veio %q", atual.Status.Phase)
	}
	if !condicaoVerdadeira(atual, previewv1alpha1.ConditionExpired) {
		t.Fatalf("condition Expired não ficou True: %+v", atual.Status.Conditions)
	}
	if atual.Status.URL != "" {
		t.Fatalf("a URL continuou publicada depois da derrubada: %q", atual.Status.URL)
	}
}

// Reconcile é chamado a cada evento do cluster, muitos deles sem nada novo.
// Se cada passada reescrevesse os objetos, o apiserver levaria uma enxurrada
// de updates idênticos e todo watch do cluster acordaria junto.
func TestSegundaPassadaNaoEscreveDeNovo(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	chave := types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}
	var antes appsv1.Deployment
	if err := s.c.Get(ctx, chave, &antes); err != nil {
		t.Fatal(err)
	}

	s.reconcile(t, pe)

	var depois appsv1.Deployment
	if err := s.c.Get(ctx, chave, &depois); err != nil {
		t.Fatal(err)
	}
	if antes.ResourceVersion != depois.ResourceVersion {
		t.Fatalf("o deployment foi reescrito sem mudança: %s -> %s", antes.ResourceVersion, depois.ResourceVersion)
	}
}

// Selector é imutável no Deployment. Antes de o reconcile parar de reescrevê-lo,
// o primeiro commit novo no PR travava o ambiente com erro de campo imutável.
func TestCommitNovoTrocaAImagemSemMexerNoSelector(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	chave := types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}
	var antes appsv1.Deployment
	if err := s.c.Get(ctx, chave, &antes); err != nil {
		t.Fatal(err)
	}

	atual := s.lerAmbiente(t, pe)
	atual.Spec.Image = "ghcr.io/acme/loja:def5678"
	atual.Spec.Commit = "def5678"
	atual.Generation = 2
	if err := s.c.Update(ctx, atual); err != nil {
		t.Fatal(err)
	}

	s.reconcile(t, pe)

	var depois appsv1.Deployment
	if err := s.c.Get(ctx, chave, &depois); err != nil {
		t.Fatal(err)
	}
	if depois.Spec.Template.Spec.Containers[0].Image != "ghcr.io/acme/loja:def5678" {
		t.Fatalf("a imagem não trocou: %q", depois.Spec.Template.Spec.Containers[0].Image)
	}
	if antes.Spec.Selector.String() != depois.Spec.Selector.String() {
		t.Fatalf("o selector mudou: %v -> %v", antes.Spec.Selector, depois.Spec.Selector)
	}
	if depois.Spec.Template.Labels[previewv1alpha1.LabelCommit] != "def5678" {
		t.Fatalf("o rótulo de commit não acompanhou: %q", depois.Spec.Template.Labels[previewv1alpha1.LabelCommit])
	}
}

func TestSemDominioNaoCriaIngress(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.r.Config.BaseDomain = ""
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	var ing networkingv1.Ingress
	err := s.c.Get(context.Background(), types.NamespacedName{Namespace: pe.NamespaceName(), Name: ingressName}, &ing)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("criou Ingress sem domínio configurado: %v", err)
	}

	atual := s.lerAmbiente(t, pe)
	if atual.Status.URL != "" {
		t.Fatalf("publicou URL sem Ingress: %q", atual.Status.URL)
	}
	// Cluster mal configurado não é PR quebrado: o ambiente sobe assim mesmo.
	var deploy appsv1.Deployment
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}, &deploy); err != nil {
		t.Fatalf("o deployment devia ter subido mesmo sem Ingress: %v", err)
	}
}

func TestCopiaOPullSecretParaONamespaceDoPreview(t *testing.T) {
	origem := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ghcr", Namespace: "previews"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)},
	}
	pe := novoAmbiente(func(p *previewv1alpha1.PreviewEnvironment) {
		p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "ghcr"}}
	})
	s := monta(t, criacao, pe, origem)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	var copia corev1.Secret
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: pe.NamespaceName(), Name: "ghcr"}, &copia); err != nil {
		t.Fatalf("o pull secret não foi copiado: %v", err)
	}
	if copia.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("o tipo do secret se perdeu na cópia: %q", copia.Type)
	}
	if string(copia.Data[".dockerconfigjson"]) != `{"auths":{}}` {
		t.Fatal("o conteúdo do secret se perdeu na cópia")
	}
}

func TestPullSecretAusenteDevolveErro(t *testing.T) {
	pe := novoAmbiente(func(p *previewv1alpha1.PreviewEnvironment) {
		p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "nao-existe"}}
	})
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)

	_, err := s.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pe.Namespace, Name: pe.Name},
	})
	if err == nil {
		t.Fatal("secret ausente devia devolver erro para o controller tentar de novo")
	}
}

func condicaoVerdadeira(pe *previewv1alpha1.PreviewEnvironment, tipo string) bool {
	for _, c := range pe.Status.Conditions {
		if c.Type == tipo {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

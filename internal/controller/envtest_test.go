package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	previewv1alpha1 "github.com/RIGOandre/Controller_K8/api/v1alpha1"
)

// Os testes com client falso cobrem a decisão do reconcile. O que eles não
// conseguem tocar é o apiserver: o schema do CRD, os defaults que vêm dele, a
// validação que ele aplica e o subresource de status. Isso só aparece contra
// um apiserver de verdade, que é o que o envtest sobe.
//
// Sem os binários instalados o teste é pulado em vez de falhar — o CI instala
// e roda; a máquina de quem só quer `go test ./...` não precisa.
func acharAssets(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir
	}
	candidatos, _ := filepath.Glob("/tmp/envtest/k8s/*")
	for _, c := range candidatos {
		if _, err := os.Stat(filepath.Join(c, "kube-apiserver")); err == nil {
			return c
		}
	}
	t.Skip("binários do envtest ausentes; defina KUBEBUILDER_ASSETS para rodar este teste")
	return ""
}

type ambienteDeTeste struct {
	c      client.Client
	ctx    context.Context
	testes *testing.T
}

// Um apiserver para todos os testes, e não um por teste. Não é só custo dos
// seis segundos de boot: o controller-runtime mantém registro global de nome
// de controller, então um segundo manager no mesmo processo recusa registrar
// "previewenvironment" de novo.
var (
	umaVez        sync.Once
	compartilhado *envtest.Environment
	clienteMgr    client.Client
	ctxMgr        context.Context
	pararMgr      context.CancelFunc
	erroSetup     error
)

func TestMain(m *testing.M) {
	codigo := m.Run()
	if pararMgr != nil {
		pararMgr()
	}
	if compartilhado != nil {
		_ = compartilhado.Stop()
	}
	os.Exit(codigo)
}

func apiserver(t *testing.T) *ambienteDeTeste {
	t.Helper()
	assets := acharAssets(t)

	umaVez.Do(func() { erroSetup = subir(assets) })
	if erroSetup != nil {
		t.Fatalf("apiserver de teste: %v", erroSetup)
	}
	return &ambienteDeTeste{c: clienteMgr, ctx: ctxMgr, testes: t}
}

func subir(assets string) error {
	compartilhado = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
	}
	cfg, err := compartilhado.Start()
	if err != nil {
		return fmt.Errorf("subir o apiserver: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := previewv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// Métricas desligadas: aqui elas só disputariam porta.
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}

	reconciler := &PreviewEnvironmentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("preview-operator-teste"),
		Config:   testConfig(),
	}
	if err := reconciler.SetupWithManager(mgr, 1); err != nil {
		return fmt.Errorf("registrar o controller: %w", err)
	}

	ctxMgr, pararMgr = context.WithCancel(context.Background())
	go func() { _ = mgr.Start(ctxMgr) }()
	if !mgr.GetCache().WaitForCacheSync(ctxMgr) {
		return errors.New("o cache do manager não sincronizou")
	}
	clienteMgr = mgr.GetClient()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "previews"}}
	if err := clienteMgr.Create(ctxMgr, ns); err != nil {
		return fmt.Errorf("criar o namespace dos CRs: %w", err)
	}
	return nil
}

// ateQue repete a checagem até ela passar. O client do manager lê de cache:
// escrever e ler na linha seguinte devolve o estado antigo.
func (a *ambienteDeTeste) ateQue(descricao string, checar func() error) {
	a.testes.Helper()
	err := wait.PollUntilContextTimeout(a.ctx, 100*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) {
			return checar() == nil, nil
		})
	if err != nil {
		a.testes.Fatalf("%s: não aconteceu a tempo (último erro: %v)", descricao, checar())
	}
}

func TestApiserverAplicaOsDefaultsDoCRD(t *testing.T) {
	amb := apiserver(t)

	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: "previews"},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository:  "acme/loja",
			PullRequest: 1,
			Image:       "ghcr.io/acme/loja:sha",
		},
	}
	if err := amb.c.Create(amb.ctx, pe); err != nil {
		t.Fatalf("criar: %v", err)
	}

	var lido previewv1alpha1.PreviewEnvironment
	amb.ateQue("o objeto aparecer no cache", func() error {
		return amb.c.Get(amb.ctx, client.ObjectKeyFromObject(pe), &lido)
	})

	if lido.Spec.TTL != "24h" {
		t.Fatalf("default de ttl não veio do CRD: %q", lido.Spec.TTL)
	}
	if lido.TTLDuration() != 24*time.Hour {
		t.Fatalf("o default do CRD não vira duração: %s", lido.TTLDuration())
	}
	if lido.Spec.Port != 8080 {
		t.Fatalf("default de port não veio do CRD: %d", lido.Spec.Port)
	}
	if lido.Spec.Replicas == nil || *lido.Spec.Replicas != 1 {
		t.Fatalf("default de replicas não veio do CRD: %v", lido.Spec.Replicas)
	}
}

// As marcações de validação viram schema no CRD, e o schema só vale se o
// apiserver de fato recusar. Este teste é o que prova que a marcação não é
// comentário decorativo.
func TestApiserverRecusaSpecInvalido(t *testing.T) {
	amb := apiserver(t)

	casos := []struct {
		nome string
		spec previewv1alpha1.PreviewEnvironmentSpec
	}{
		{"repositório sem barra", previewv1alpha1.PreviewEnvironmentSpec{Repository: "sembarra", PullRequest: 1, Image: "img"}},
		{"pull request zero", previewv1alpha1.PreviewEnvironmentSpec{Repository: "acme/loja", PullRequest: 0, Image: "img"}},
		{"imagem vazia", previewv1alpha1.PreviewEnvironmentSpec{Repository: "acme/loja", PullRequest: 1, Image: ""}},
		{"porta fora da faixa", previewv1alpha1.PreviewEnvironmentSpec{Repository: "acme/loja", PullRequest: 1, Image: "img", Port: 70000}},
	}

	for i, caso := range casos {
		t.Run(caso.nome, func(t *testing.T) {
			pe := &previewv1alpha1.PreviewEnvironment{
				ObjectMeta: metav1.ObjectMeta{Name: "invalido-" + string(rune('a'+i)), Namespace: "previews"},
				Spec:       caso.spec,
			}
			if err := amb.c.Create(amb.ctx, pe); err == nil {
				t.Fatal("o apiserver aceitou um spec que o schema devia recusar")
			}
		})
	}
}

// O buraco que este teste fecha: metav1.Duration decodifica com
// time.ParseDuration, e um schema de string pura aceitaria "24 horas". O
// informer decodifica a LIST inteira de uma vez — um único objeto ruim, em
// qualquer namespace, faria a LIST falhar em laço e o controller pararia de
// reconciliar TODOS os ambientes do cluster, sem nada no status de ninguém.
//
// Passa por unstructured porque o tipo Go não representa esse valor: só dá
// para escrevê-lo falando direto com o apiserver, que é exatamente o que um
// `kubectl apply` faz.
func TestApiserverRecusaTtlQueOGoNaoDecodifica(t *testing.T) {
	amb := apiserver(t)

	venenos := []string{"24 horas", "1 dia", "24", "abc", "-5h"}
	for i, veneno := range venenos {
		t.Run(veneno, func(t *testing.T) {
			cr := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": previewv1alpha1.GroupVersion.String(),
				"kind":       "PreviewEnvironment",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("veneno-%d", i),
					"namespace": "previews",
				},
				"spec": map[string]any{
					"repository":  "acme/loja",
					"pullRequest": int64(1),
					"image":       "ghcr.io/acme/loja:sha",
					"ttl":         veneno,
				},
			}}
			if err := amb.c.Create(amb.ctx, cr); err == nil {
				t.Fatalf("o apiserver aceitou ttl=%q; um CR desses derruba o informer de todo o cluster", veneno)
			}
		})
	}

	// E o que o Go decodifica continua passando.
	valido := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": previewv1alpha1.GroupVersion.String(),
		"kind":       "PreviewEnvironment",
		"metadata":   map[string]any{"name": "ttl-valido", "namespace": "previews"},
		"spec": map[string]any{
			"repository": "acme/loja", "pullRequest": int64(2),
			"image": "ghcr.io/acme/loja:sha", "ttl": "1h30m",
		},
	}}
	if err := amb.c.Create(amb.ctx, valido); err != nil {
		t.Fatalf("o schema recusou um ttl válido: %v", err)
	}
}

// repository e pullRequest decidem o nome do namespace. Editá-los trocaria o
// destino e abandonaria o namespace antigo, com o workload dentro, consumindo
// quota sem nenhum objeto que aponte para ele.
func TestApiserverRecusaMudarRepositoryEPullRequest(t *testing.T) {
	amb := apiserver(t)

	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "imutavel", Namespace: "previews"},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository: "acme/loja", PullRequest: 3, Image: "ghcr.io/acme/loja:sha",
		},
	}
	if err := amb.c.Create(amb.ctx, pe); err != nil {
		t.Fatalf("criar: %v", err)
	}

	var lido previewv1alpha1.PreviewEnvironment
	amb.ateQue("o objeto aparecer no cache", func() error {
		return amb.c.Get(amb.ctx, client.ObjectKeyFromObject(pe), &lido)
	})

	trocado := lido.DeepCopy()
	trocado.Spec.Repository = "outro/repo"
	if err := amb.c.Update(amb.ctx, trocado); err == nil {
		t.Fatal("o apiserver aceitou trocar repository; o namespace antigo ficaria órfão")
	}

	trocado = lido.DeepCopy()
	trocado.Spec.PullRequest = 4
	if err := amb.c.Update(amb.ctx, trocado); err == nil {
		t.Fatal("o apiserver aceitou trocar pullRequest; o namespace antigo ficaria órfão")
	}

	// Imagem continua editável: é o que um push novo no PR muda.
	trocado = lido.DeepCopy()
	trocado.Spec.Image = "ghcr.io/acme/loja:def"
	if err := amb.c.Update(amb.ctx, trocado); err != nil {
		t.Fatalf("a imagem devia continuar editável: %v", err)
	}
}

// env com valueFrom resolveria no namespace do preview, que é onde o pull
// secret acaba de pousar — quem escreve o CR passaria a ler, dentro de uma
// imagem que ele mesmo escolheu, qualquer Secret que o operator alcance.
func TestApiserverRecusaEnvComValueFrom(t *testing.T) {
	amb := apiserver(t)

	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-roubado", Namespace: "previews"},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository: "acme/loja", PullRequest: 5, Image: "ghcr.io/acme/loja:sha",
			Env: []corev1.EnvVar{{
				Name: "ROUBADO",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "prod-db"},
					Key:                  "password",
				}},
			}},
		},
	}
	if err := amb.c.Create(amb.ctx, pe); err == nil {
		t.Fatal("o apiserver aceitou env com valueFrom")
	}
}

func TestControllerMontaOAmbienteContraOApiserverDeVerdade(t *testing.T) {
	amb := apiserver(t)

	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-9", Namespace: "previews"},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository:  "acme/loja",
			PullRequest: 9,
			Commit:      "abc1234",
			Image:       "ghcr.io/acme/loja:abc1234",
			Port:        3000,
		},
	}
	if err := amb.c.Create(amb.ctx, pe); err != nil {
		t.Fatalf("criar: %v", err)
	}
	nsPreview := pe.NamespaceName()

	amb.ateQue("o namespace do preview nascer", func() error {
		var ns corev1.Namespace
		return amb.c.Get(amb.ctx, types.NamespacedName{Name: nsPreview}, &ns)
	})

	amb.ateQue("o deployment nascer com a imagem do PR", func() error {
		var d appsv1.Deployment
		if err := amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsPreview, Name: deploymentName}, &d); err != nil {
			return err
		}
		if got := d.Spec.Template.Spec.Containers[0].Image; got != pe.Spec.Image {
			return &erroDeEspera{"imagem ainda é " + got}
		}
		return nil
	})

	amb.ateQue("o service nascer", func() error {
		var s corev1.Service
		return amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsPreview, Name: serviceName}, &s)
	})

	amb.ateQue("o ingress nascer com o host calculado", func() error {
		var i networkingv1.Ingress
		if err := amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsPreview, Name: ingressName}, &i); err != nil {
			return err
		}
		if got := i.Spec.Rules[0].Host; got != "pr-9-acme-loja.preview.exemplo.dev" {
			return &erroDeEspera{"host ainda é " + got}
		}
		return nil
	})

	// Status é subresource de verdade aqui: quem escreve o spec não escreve o
	// status junto, e vice-versa. Com o client falso isso é configuração; com
	// o apiserver é o comportamento real.
	amb.ateQue("o status ser publicado", func() error {
		var lido previewv1alpha1.PreviewEnvironment
		if err := amb.c.Get(amb.ctx, client.ObjectKeyFromObject(pe), &lido); err != nil {
			return err
		}
		if lido.Status.URL != "https://pr-9-acme-loja.preview.exemplo.dev" {
			return &erroDeEspera{"url ainda é " + lido.Status.URL}
		}
		if lido.Status.Namespace != nsPreview {
			return &erroDeEspera{"namespace do status ainda é " + lido.Status.Namespace}
		}
		if lido.Status.ExpiresAt == nil {
			return &erroDeEspera{"expiresAt ainda vazio"}
		}
		if len(lido.Status.Conditions) == 0 {
			return &erroDeEspera{"sem conditions"}
		}
		return nil
	})

	// O finalizer precisa ter chegado ao objeto no apiserver, não só à cópia
	// em memória do reconcile.
	var lido previewv1alpha1.PreviewEnvironment
	if err := amb.c.Get(amb.ctx, client.ObjectKeyFromObject(pe), &lido); err != nil {
		t.Fatal(err)
	}
	if len(lido.Finalizers) == 0 || lido.Finalizers[0] != previewv1alpha1.Finalizer {
		t.Fatalf("finalizer não persistiu: %v", lido.Finalizers)
	}
}

// O client falso não mantém metadata.generation: só um apiserver de verdade
// faz essa contabilidade, e é ela que separa "o Deployment respondeu à
// alteração" de "o status ainda é o da revisão anterior". Sem esta checagem, o
// ambiente anuncia Ready para uma geração cujos pods não existem.
func TestNaoDizReadyComStatusDefasadoDoDeployment(t *testing.T) {
	amb := apiserver(t)

	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-11", Namespace: "previews"},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository:  "acme/defasado",
			PullRequest: 11,
			Image:       "ghcr.io/acme/defasado:abc",
		},
	}
	if err := amb.c.Create(amb.ctx, pe); err != nil {
		t.Fatalf("criar: %v", err)
	}

	chave := types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}
	amb.ateQue("o deployment nascer", func() error {
		var d appsv1.Deployment
		return amb.c.Get(amb.ctx, chave, &d)
	})

	var deploy appsv1.Deployment
	if err := amb.c.Get(amb.ctx, chave, &deploy); err != nil {
		t.Fatal(err)
	}
	if deploy.Generation == 0 {
		t.Fatal("o apiserver não atribuiu generation; o teste perderia o sentido")
	}
	// Números de uma revisão pronta, com o status ainda uma geração atrás.
	deploy.Status.ObservedGeneration = deploy.Generation - 1
	deploy.Status.Replicas = 1
	deploy.Status.ReadyReplicas = 1
	deploy.Status.UpdatedReplicas = 1
	deploy.Status.AvailableReplicas = 1
	deploy.Status.UnavailableReplicas = 0
	if err := amb.c.Status().Update(amb.ctx, &deploy); err != nil {
		t.Fatalf("gravar status do deployment: %v", err)
	}

	amb.ateQue("o ambiente ficar em Provisioning aguardando o rollout", func() error {
		var lido previewv1alpha1.PreviewEnvironment
		if err := amb.c.Get(amb.ctx, client.ObjectKeyFromObject(pe), &lido); err != nil {
			return err
		}
		if lido.Status.Phase == previewv1alpha1.PhaseReady {
			return &erroDeEspera{"anunciou Ready lendo status de uma revisão anterior"}
		}
		for _, c := range lido.Status.Conditions {
			if c.Type == previewv1alpha1.ConditionReady && c.Reason == "AguardandoRollout" {
				return nil
			}
		}
		return &erroDeEspera{"ainda não reportou AguardandoRollout"}
	})
}

type erroDeEspera struct{ msg string }

func (e *erroDeEspera) Error() string { return e.msg }

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	previewv1alpha1 "github.com/RIGOandre/Controller_K8/api/v1alpha1"
)

// erroPermanente marca a falha que nova tentativa não resolve: spec apontando
// para o que não existe, namespace de outro dono, permissão negada. Ela vira
// PhaseFailed em vez de retry infinito com o status mentindo que está tudo no
// ar. O controller-runtime não distingue os dois casos sozinho — para ele todo
// erro devolvido é motivo de reenfileirar.
type erroPermanente struct{ motivo, mensagem string }

func (e *erroPermanente) Error() string { return e.mensagem }

func permanente(motivo, formato string, args ...any) *erroPermanente {
	return &erroPermanente{motivo: motivo, mensagem: fmt.Sprintf(formato, args...)}
}

// terminatingRequeue é o intervalo de espera enquanto um namespace ainda
// está em Terminating. Não é polling de estado normal: só acontece durante a
// derrubada, e para assim que o namespace some.
const terminatingRequeue = 5 * time.Second

// PreviewEnvironmentReconciler leva o cluster ao estado que o spec descreve.
type PreviewEnvironmentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Config   Config

	// Now é injetável para o teste conseguir vencer um TTL sem esperar o TTL.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=preview.rigo.dev,resources=previewenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=preview.rigo.dev,resources=previewenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=preview.rigo.dev,resources=previewenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete

// Reconcile é chamado a cada evento no PreviewEnvironment e nos Deployments
// que ele gerou. Roda inteiro toda vez: não guarda estado entre chamadas e
// não confia no que o status diz — lê o cluster.
func (r *PreviewEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	started := r.now()
	defer func() {
		outcome := resultadoOK
		if err != nil {
			outcome = resultadoErro
		}
		reconcileDuration.WithLabelValues(outcome).Observe(r.now().Sub(started).Seconds())
	}()

	var pe previewv1alpha1.PreviewEnvironment
	if err := r.Get(ctx, req.NamespacedName, &pe); err != nil {
		if apierrors.IsNotFound(err) {
			// Objeto já saiu do cluster: só resta limpar as séries que
			// carregam o nome dele.
			forgetEnvironment(req.Namespace, req.Name)
			r.refreshActiveGauge(ctx)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !pe.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &pe)
	}

	// O finalizer entra antes de qualquer objeto ser criado. Na ordem
	// inversa existe uma janela em que o namespace já subiu e uma remoção do
	// CR o deixaria órfão, consumindo quota para sempre.
	if !controllerutil.ContainsFinalizer(&pe, previewv1alpha1.Finalizer) {
		controllerutil.AddFinalizer(&pe, previewv1alpha1.Finalizer)
		return ctrl.Result{}, r.Update(ctx, &pe)
	}

	expiry := pe.ExpiryTime()
	if !r.now().Before(expiry) {
		return r.expire(ctx, &pe)
	}

	// A série do vencimento só existe enquanto o ambiente existe. Publicá-la
	// antes da checagem acima faria um ambiente já vencido recriar a própria
	// série a cada evento, logo depois de a derrubada tê-la removido.
	expiryTimestamp.WithLabelValues(
		pe.Namespace, pe.Name, pe.Spec.Repository, strconv.FormatInt(int64(pe.Spec.PullRequest), 10),
	).Set(float64(expiry.Unix()))

	if err := r.apply(ctx, &pe); err != nil {
		return r.falhar(ctx, &pe, err)
	}

	if err := r.observe(ctx, &pe); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.publishStatus(ctx, &pe, true); err != nil {
		return ctrl.Result{}, err
	}
	r.refreshActiveGauge(ctx)

	// O vencimento é reconferido aqui, e não subtraído do que foi lido lá em
	// cima. Entre a checagem do topo e este ponto rodou o reconcile inteiro —
	// seis chamadas à API, uma gravação de status e uma listagem. Se o TTL
	// venceu nesse intervalo, a subtração sai negativa, e o controller-runtime
	// só agenda com RequeueAfter > 0: valor negativo cai no Forget(req) e o
	// despertar some sem erro nenhum. O ambiente ficaria de pé até um evento
	// alheio ou o resync do informer, que é de horas.
	espera := expiry.Sub(r.now())
	if espera <= 0 {
		return r.expire(ctx, &pe)
	}

	// Requeue no instante exato do vencimento. Uma varredura periódica
	// custaria uma passada em todos os ambientes a cada tique para acertar o
	// vencimento de um; aqui cada ambiente acorda uma vez, na hora dele.
	return ctrl.Result{RequeueAfter: espera}, nil
}

// falhar publica o erro no status sem afirmar convergência.
//
// A versão anterior mexia só na condition Progressing e ainda assim carimbava
// observedGeneration. O par (phase: Ready, observedGeneration: N) sobrevivia a
// um apply quebrado: quem lesse o objeto — ou o gate do workflow — concluía
// que a geração N estava no ar enquanto todas as passadas falhavam.
func (r *PreviewEnvironmentReconciler) falhar(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment, err error) (ctrl.Result, error) {
	var permanente *erroPermanente
	definitivo := errors.As(err, &permanente)

	motivo := "ErroAoAplicar"
	if definitivo {
		motivo = permanente.motivo
	}

	// Unknown e não False: o operator não sabe mais em que estado o ambiente
	// está, e dizer "não está pronto" seria uma afirmação que ele não pode
	// sustentar.
	r.setReady(pe, metav1.ConditionUnknown, motivo, err.Error())
	r.setProgressing(pe, boolParaCondition(!definitivo), motivo, err.Error())
	if definitivo {
		r.transition(pe, previewv1alpha1.PhaseFailed)
		pe.Status.URL = ""
		r.event(pe, corev1.EventTypeWarning, motivo, err.Error())
	}

	if statusErr := r.publishStatus(ctx, pe, false); statusErr != nil {
		log.FromContext(ctx).Error(statusErr, "falha ao gravar status depois do erro de aplicação")
	}

	// Erro definitivo não volta para a fila: reenfileirar com backoff um spec
	// que aponta para o que não existe só gasta o apiserver. Uma edição do
	// spec dispara novo evento e nova tentativa.
	if definitivo {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, err
}

func boolParaCondition(v bool) metav1.ConditionStatus {
	if v {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// apply garante namespace, quota, pull secrets, deployment, service e
// ingress. Idempotente: rodar duas vezes seguidas não muda nada na segunda.
func (r *PreviewEnvironmentReconciler) apply(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) error {
	ns, err := r.garantirNamespace(ctx, pe)
	if err != nil {
		return err
	}
	pe.Status.Namespace = ns

	quota := quotaFor(pe, r.Config)
	desiredQuota := quota.Spec
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Labels = mergeLabels(quota.Labels, pe.CommonLabels())
		quota.Spec = desiredQuota
		return nil
	}); err != nil {
		return fmt.Errorf("resourcequota: %w", err)
	}

	if err := r.copyPullSecrets(ctx, pe); err != nil {
		return err
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: pe.NamespaceName()}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		aplicarNoDeployment(deploy, pe, r.Config)
		return nil
	}); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	svc := serviceFor(pe)
	desiredSvc := serviceSpec(pe)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = mergeLabels(svc.Labels, pe.CommonLabels())
		svc.Spec.Selector = desiredSvc.Selector
		svc.Spec.Ports = desiredSvc.Ports
		return nil
	}); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	host := pe.HostFor(r.Config.BaseDomain)
	if host == "" {
		// Sem domínio configurado o ambiente ainda funciona por
		// port-forward. É um cluster mal configurado, não um erro do PR:
		// vale um aviso no status, não um Failed.
		pe.Status.URL = ""
		r.setProgressing(pe, metav1.ConditionFalse, "SemDominio",
			"nem spec.host nem --base-domain definidos; o ambiente sobe sem Ingress")
		return nil
	}

	ing := ingressFor(pe, host, r.Config)
	desiredIng := ingressSpec(host, r.Config)
	annotations := ing.Annotations
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		ing.Labels = mergeLabels(ing.Labels, pe.CommonLabels())
		ing.Annotations = mergeLabels(ing.Annotations, annotations)
		ing.Spec = desiredIng
		return nil
	}); err != nil {
		return fmt.Errorf("ingress: %w", err)
	}
	pe.Status.URL = "https://" + host
	return nil
}

// garantirNamespace cria o namespace do ambiente, e recusa adotar um que já
// exista sem ser deste PreviewEnvironment.
//
// A versão anterior usava CreateOrUpdate, que adota: se um namespace com o
// nome derivado já existisse — de outro ambiente, ou de qualquer time do
// cluster — o reconcile carimbava nele o rótulo managed-by e passava a
// tratá-lo como seu. O guard do teardown, que existe justamente para não
// apagar namespace de terceiro, virava letra morta: ele conferia o rótulo que
// o próprio apply tinha acabado de escrever.
func (r *PreviewEnvironmentReconciler) garantirNamespace(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) (string, error) {
	desejado := namespaceFor(pe)

	var atual corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: desejado.Name}, &atual)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desejado); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("criar namespace %s: %w", desejado.Name, err)
		}
		return desejado.Name, nil
	case err != nil:
		return "", fmt.Errorf("ler namespace %s: %w", desejado.Name, err)
	}

	if !nosso(&atual, pe) {
		return "", permanente("NamespaceOcupado",
			"o namespace %s já existe e não pertence a este ambiente; nada foi alterado nele", desejado.Name)
	}

	// Namespace nosso: só reforça os rótulos, sem tocar em nada mais.
	if !contemTodos(atual.Labels, desejado.Labels) {
		atual.Labels = mergeLabels(atual.Labels, desejado.Labels)
		if err := r.Update(ctx, &atual); err != nil {
			return "", fmt.Errorf("atualizar rótulos do namespace %s: %w", desejado.Name, err)
		}
	}
	return desejado.Name, nil
}

// nosso responde se o objeto foi criado por este operator PARA este
// PreviewEnvironment. Conferir só managed-by não basta: o nome do namespace
// deriva de repositório e PR, e dois CRs em namespaces diferentes derivam o
// mesmo nome — um derrubaria o ambiente do outro achando que era seu.
func nosso(obj client.Object, pe *previewv1alpha1.PreviewEnvironment) bool {
	l := obj.GetLabels()
	return l[previewv1alpha1.LabelManagedBy] == previewv1alpha1.ManagedByValue &&
		l[previewv1alpha1.LabelOwnerNamespace] == pe.Namespace &&
		l[previewv1alpha1.LabelOwnerName] == pe.Name
}

func contemTodos(atual, querido map[string]string) bool {
	for chave, valor := range querido {
		if atual[chave] != valor {
			return false
		}
	}
	return true
}

// copyPullSecrets replica no namespace do preview os secrets de registry
// citados no spec. ImagePullSecrets é uma referência local ao namespace do
// pod, e o namespace do pod acabou de nascer vazio — sem a cópia, imagem de
// registry privado nunca sobe.
func (r *PreviewEnvironmentReconciler) copyPullSecrets(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) error {
	for _, ref := range pe.Spec.ImagePullSecrets {
		var src corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: pe.Namespace, Name: ref.Name}, &src); err != nil {
			if apierrors.IsNotFound(err) {
				return permanente("PullSecretAusente",
					"o secret %q não existe em %s", ref.Name, pe.Namespace)
			}
			return fmt.Errorf("pull secret %s: %w", ref.Name, err)
		}

		// Duas travas antes de copiar. Sem elas, bastava citar o nome de
		// qualquer Secret do namespace do CR para o operator entregá-lo a um
		// pod que roda código de pull request — o operator emprestaria o
		// acesso de leitura dele a uma requisição que não carrega autorização
		// nenhuma.
		if src.Type != corev1.SecretTypeDockerConfigJson {
			return permanente("PullSecretDeTipoErrado",
				"o secret %s é do tipo %s; só %s pode ser copiado para o preview",
				ref.Name, src.Type, corev1.SecretTypeDockerConfigJson)
		}
		if src.Labels[previewv1alpha1.LabelCopiavel] != "true" {
			return permanente("PullSecretSemConsentimento",
				"o secret %s não tem o rótulo %s=true; quem é dono dele precisa autorizar a cópia",
				ref.Name, previewv1alpha1.LabelCopiavel)
		}

		dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: pe.NamespaceName()}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dst, func() error {
			dst.Labels = mergeLabels(dst.Labels, pe.CommonLabels())
			dst.Type = src.Type
			// Só a chave do registry. Copiar o Data inteiro levaria junto
			// qualquer outra chave que o Secret carregue.
			dst.Data = map[string][]byte{
				corev1.DockerConfigJsonKey: src.Data[corev1.DockerConfigJsonKey],
			}
			return nil
		}); err != nil {
			return fmt.Errorf("copiar pull secret %s: %w", ref.Name, err)
		}
	}
	return nil
}

// observe lê o Deployment e traduz para phase e conditions.
func (r *PreviewEnvironmentReconciler) observe(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) error {
	var deploy appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: pe.NamespaceName(), Name: deploymentName}, &deploy)
	switch {
	case apierrors.IsNotFound(err):
		pe.Status.ReadyReplicas = 0
		r.transition(pe, previewv1alpha1.PhasePending)
		r.setReady(pe, metav1.ConditionFalse, "DeploymentAusente", "deployment ainda não visível no cache")
		return nil
	case err != nil:
		return fmt.Errorf("ler deployment: %w", err)
	}

	pe.Status.ReadyReplicas = deploy.Status.ReadyReplicas
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	// Status defasado não vale como resposta. Logo depois de o apply reescrever
	// o pod template, o status do Deployment ainda é o da revisão anterior: o
	// controller do Deployment nem reagiu. Ler ReadyReplicas ali diria Ready
	// para uma geração cujos pods não existem.
	if deploy.Status.ObservedGeneration < deploy.Generation {
		r.transition(pe, previewv1alpha1.PhaseProvisioning)
		r.setReady(pe, metav1.ConditionFalse, "AguardandoRollout",
			"o Deployment ainda não reagiu à última alteração")
		r.setProgressing(pe, metav1.ConditionTrue, "AguardandoRollout", "rollout ainda não começou")
		return nil
	}

	// Rollout travado é falha, não espera. Com replicas=1 o maxUnavailable
	// padrão é zero: o pod antigo só cai quando o novo fica pronto. Se a
	// imagem nova não sobe, o pod velho segue Ready para sempre e
	// ReadyReplicas fica em 1 — o ambiente anunciaria Ready servindo o commit
	// anterior, indefinidamente.
	if travado := condicaoDoDeployment(&deploy, appsv1.DeploymentProgressing); travado != nil &&
		travado.Status == corev1.ConditionFalse && travado.Reason == "ProgressDeadlineExceeded" {
		r.transition(pe, previewv1alpha1.PhaseFailed)
		pe.Status.URL = ""
		r.setReady(pe, metav1.ConditionFalse, "RolloutTravado", travado.Message)
		r.setProgressing(pe, metav1.ConditionFalse, "RolloutTravado", travado.Message)
		return nil
	}

	pronto := desired > 0 &&
		deploy.Status.UpdatedReplicas >= desired &&
		deploy.Status.ReadyReplicas >= desired &&
		deploy.Status.UnavailableReplicas == 0

	if pronto {
		r.transition(pe, previewv1alpha1.PhaseReady)
		r.setReady(pe, metav1.ConditionTrue, "Disponivel",
			fmt.Sprintf("%d de %d réplicas prontas na revisão atual", deploy.Status.ReadyReplicas, desired))
		r.setProgressing(pe, metav1.ConditionFalse, "Concluido", "ambiente no ar")
		return nil
	}

	r.transition(pe, previewv1alpha1.PhaseProvisioning)
	detalhe := fmt.Sprintf("%d de %d réplicas prontas, %d atualizadas, %d indisponíveis",
		deploy.Status.ReadyReplicas, desired, deploy.Status.UpdatedReplicas, deploy.Status.UnavailableReplicas)
	r.setReady(pe, metav1.ConditionFalse, "Subindo", detalhe)
	r.setProgressing(pe, metav1.ConditionTrue, "Subindo", detalhe)
	return nil
}

func condicaoDoDeployment(deploy *appsv1.Deployment, tipo appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range deploy.Status.Conditions {
		if deploy.Status.Conditions[i].Type == tipo {
			return &deploy.Status.Conditions[i]
		}
	}
	return nil
}

// expire derruba o ambiente vencido mas mantém o CR. O objeto vira o
// registro de que aquele PR teve ambiente e de quando ele caiu; quem apaga o
// CR é o workflow do repositório, no merge ou no fechamento do PR.
func (r *PreviewEnvironmentReconciler) expire(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) (ctrl.Result, error) {
	gone, err := r.teardown(ctx, pe)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.transition(pe, previewv1alpha1.PhaseExpired)
	pe.Status.URL = ""
	pe.Status.ReadyReplicas = 0
	r.setReady(pe, metav1.ConditionFalse, "TTLVencido", "ambiente derrubado por vencimento do TTL")
	r.setProgressing(pe, metav1.ConditionFalse, "TTLVencido", "nada a fazer: o TTL venceu")
	meta := metav1.Condition{
		Type:    previewv1alpha1.ConditionExpired,
		Status:  metav1.ConditionTrue,
		Reason:  "TTLVencido",
		Message: fmt.Sprintf("TTL de %s contado a partir da criação", pe.TTLDuration()),
	}
	setCondition(pe, meta)

	if err := r.publishStatus(ctx, pe, true); err != nil {
		return ctrl.Result{}, err
	}
	if !gone {
		return ctrl.Result{RequeueAfter: terminatingRequeue}, nil
	}
	forgetEnvironment(pe.Namespace, pe.Name)
	r.refreshActiveGauge(ctx)
	return ctrl.Result{}, nil
}

// finalize segura a remoção do CR até o namespace do preview sumir.
//
// Não dá para resolver isso com ownerReference: o dono é namespaced e o
// namespace é cluster-scoped, e o garbage collector recusa essa relação — ele
// apagaria o namespace na hora, achando que o dono já não existe. Deployment,
// Service e Ingress também ficam de fora por morarem em outro namespace que
// o do dono. Então a limpeza é uma só: apagar o namespace e deixar o cascade
// nativo levar o resto.
func (r *PreviewEnvironmentReconciler) finalize(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pe, previewv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}

	gone, err := r.teardown(ctx, pe)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !gone {
		// Enquanto o namespace não sai, o status precisa dizer isso. Sem esta
		// parte, um namespace preso em Terminating deixava o ambiente
		// aparecendo como Provisioning e com a URL publicada — quem olhasse
		// `kubectl get previews` veria um ambiente que parece estar subindo e
		// um endereço que não responde mais.
		r.transition(pe, previewv1alpha1.PhaseTerminating)
		pe.Status.URL = ""
		pe.Status.ReadyReplicas = 0
		r.setReady(pe, metav1.ConditionFalse, "EmRemocao",
			fmt.Sprintf("aguardando o namespace %s sair de Terminating", pe.NamespaceName()))
		r.setProgressing(pe, metav1.ConditionTrue, "EmRemocao", "removendo o ambiente")
		if err := r.publishStatus(ctx, pe, true); err != nil {
			// Status é diagnóstico: não vale travar a remoção por causa dele.
			// Um conflito aqui é comum — o objeto está sendo apagado.
			log.FromContext(ctx).V(1).Info("não consegui publicar o status durante a remoção", "erro", err)
		}

		// Soltar o finalizer aqui deixaria o namespace terminando sem dono.
		// Se ele travasse em Terminating, ninguém mais saberia de onde veio.
		return ctrl.Result{RequeueAfter: terminatingRequeue}, nil
	}

	forgetEnvironment(pe.Namespace, pe.Name)
	controllerutil.RemoveFinalizer(pe, previewv1alpha1.Finalizer)
	if err := r.Update(ctx, pe); err != nil {
		return ctrl.Result{}, err
	}
	r.refreshActiveGauge(ctx)
	return ctrl.Result{}, nil
}

// teardown pede a remoção do namespace e responde se ele já sumiu.
func (r *PreviewEnvironmentReconciler) teardown(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) (bool, error) {
	var ns corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: pe.NamespaceName()}, &ns)
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("ler namespace: %w", err)
	}

	// Só apaga o que é deste ambiente. Conferir apenas managed-by não bastava:
	// o nome do namespace deriva de repositório e PR, e dois PreviewEnvironment
	// em namespaces diferentes derivam o mesmo nome — um derrubaria o ambiente
	// do outro achando que era seu.
	if !nosso(&ns, pe) {
		// Solta o finalizer mesmo assim, de propósito. Segurar aqui deixaria o
		// CR impossível de apagar sem editar finalizer na mão, e o namespace
		// que não é nosso nunca vai sumir por nossa conta. O preço é um
		// namespace que pode ficar órfão se alguém tiver arrancado os rótulos
		// dele — por isso o evento nomeia o namespace, que é o que permite
		// achá-lo depois.
		r.event(pe, corev1.EventTypeWarning, "NamespaceDeOutroDono",
			fmt.Sprintf("namespace %s existe e não pertence a este ambiente; não será apagado", ns.Name))
		return true, nil
	}

	if ns.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("apagar namespace: %w", err)
		}
		r.event(pe, corev1.EventTypeNormal, "AmbienteDerrubado",
			fmt.Sprintf("namespace %s marcado para remoção", ns.Name))
	}
	return false, nil
}

// publishStatus grava o status observado.
//
// observedGeneration só avança quando `convergiu` — é o carimbo de que ESTE
// spec foi atendido, não de que houve uma tentativa. Carimbá-lo no caminho de
// erro fazia o campo afirmar convergência que não houve, e é exatamente esse
// campo que um consumidor lê para decidir se pode confiar no resto do status.
func (r *PreviewEnvironmentReconciler) publishStatus(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment, convergiu bool) error {
	expiry := metav1.NewTime(pe.ExpiryTime())
	pe.Status.ExpiresAt = &expiry
	if convergiu {
		pe.Status.ObservedGeneration = pe.Generation
	}
	if err := r.Status().Update(ctx, pe); err != nil {
		return fmt.Errorf("gravar status: %w", err)
	}
	return nil
}

// transition troca a phase e conta a troca. Só conta quando muda de fato:
// contar toda passada transformaria a métrica num contador de reconciles.
func (r *PreviewEnvironmentReconciler) transition(pe *previewv1alpha1.PreviewEnvironment, phase previewv1alpha1.Phase) {
	if pe.Status.Phase == phase {
		return
	}
	pe.Status.Phase = phase
	transitions.WithLabelValues(string(phase), pe.Spec.Repository).Inc()
	r.event(pe, corev1.EventTypeNormal, string(phase), fmt.Sprintf("ambiente em %s", phase))
}

func (r *PreviewEnvironmentReconciler) setReady(pe *previewv1alpha1.PreviewEnvironment, status metav1.ConditionStatus, reason, msg string) {
	setCondition(pe, metav1.Condition{Type: previewv1alpha1.ConditionReady, Status: status, Reason: reason, Message: msg})
}

func (r *PreviewEnvironmentReconciler) setProgressing(pe *previewv1alpha1.PreviewEnvironment, status metav1.ConditionStatus, reason, msg string) {
	setCondition(pe, metav1.Condition{Type: previewv1alpha1.ConditionProgressing, Status: status, Reason: reason, Message: msg})
}

func setCondition(pe *previewv1alpha1.PreviewEnvironment, c metav1.Condition) {
	c.ObservedGeneration = pe.Generation
	apimeta := &pe.Status.Conditions
	for i := range *apimeta {
		if (*apimeta)[i].Type != c.Type {
			continue
		}
		// LastTransitionTime só muda quando o status muda. Reescrever a cada
		// passada apagaria a informação de "há quanto tempo está assim".
		if (*apimeta)[i].Status == c.Status {
			c.LastTransitionTime = (*apimeta)[i].LastTransitionTime
		} else {
			c.LastTransitionTime = metav1.Now()
		}
		(*apimeta)[i] = c
		return
	}
	c.LastTransitionTime = metav1.Now()
	*apimeta = append(*apimeta, c)
}

func (r *PreviewEnvironmentReconciler) event(pe *previewv1alpha1.PreviewEnvironment, kind, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(pe, kind, reason, msg)
}

// refreshActiveGauge recalcula o total a partir do cache do manager. É uma
// leitura em memória, não uma chamada à API.
func (r *PreviewEnvironmentReconciler) refreshActiveGauge(ctx context.Context) {
	var list previewv1alpha1.PreviewEnvironmentList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "não foi possível recontar os ambientes ativos")
		return
	}

	// Conta só o que está de fato no ar. O CR sobrevive ao vencimento de
	// propósito — vira o registro de que aquele PR teve ambiente — e contar
	// esses registros faria o gauge de "ativos" nunca descer: num cluster com
	// duzentos PRs velhos, o painel mostraria duzentos ambientes ativos e
	// nenhum namespace de preview existindo.
	porRepositorio := map[string]int{}
	for i := range list.Items {
		item := &list.Items[i]
		if !item.DeletionTimestamp.IsZero() {
			continue
		}
		switch item.Status.Phase {
		case previewv1alpha1.PhaseExpired, previewv1alpha1.PhaseTerminating, previewv1alpha1.PhaseFailed:
			continue
		}
		porRepositorio[item.Spec.Repository]++
	}
	publicarAtivos(porRepositorio)
}

func (r *PreviewEnvironmentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registra o controller.
//
// Owns() não serve aqui: ele depende de ownerReference, que não existe entre
// o CR e objetos de outro namespace. O caminho de volta é feito pelos rótulos
// que `apply` carimba em tudo que cria.
func (r *PreviewEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrent int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&previewv1alpha1.PreviewEnvironment{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(ownerFromLabels)).
		Watches(&networkingv1.Ingress{}, handler.EnqueueRequestsFromMapFunc(ownerFromLabels)).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Named("previewenvironment").
		Complete(r)
}

// ownerFromLabels faz o caminho de volta: de um objeto criado pelo operator
// para o PreviewEnvironment que o pediu.
func ownerFromLabels(_ context.Context, obj client.Object) []ctrl.Request {
	l := obj.GetLabels()
	if l[previewv1alpha1.LabelManagedBy] != previewv1alpha1.ManagedByValue {
		return nil
	}
	ns, name := l[previewv1alpha1.LabelOwnerNamespace], l[previewv1alpha1.LabelOwnerName]
	if ns == "" || name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

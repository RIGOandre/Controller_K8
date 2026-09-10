package controller

import (
	"context"
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
		r.setProgressing(&pe, metav1.ConditionTrue, "ErroAoAplicar", err.Error())
		if statusErr := r.publishStatus(ctx, &pe); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "falha ao gravar status depois do erro de aplicação")
		}
		return ctrl.Result{}, err
	}

	if err := r.observe(ctx, &pe); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.publishStatus(ctx, &pe); err != nil {
		return ctrl.Result{}, err
	}
	r.refreshActiveGauge(ctx)

	// Requeue no instante exato do vencimento. Uma varredura periódica
	// custaria uma passada em todos os ambientes a cada tique para acertar o
	// vencimento de um; aqui cada ambiente acorda uma vez, na hora dele.
	return ctrl.Result{RequeueAfter: expiry.Sub(r.now())}, nil
}

// apply garante namespace, quota, pull secrets, deployment, service e
// ingress. Idempotente: rodar duas vezes seguidas não muda nada na segunda.
func (r *PreviewEnvironmentReconciler) apply(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) error {
	ns := namespaceFor(pe)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		ns.Labels = mergeLabels(ns.Labels, namespaceFor(pe).Labels)
		return nil
	}); err != nil {
		return fmt.Errorf("namespace %s: %w", ns.Name, err)
	}
	pe.Status.Namespace = ns.Name

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

	deploy := deploymentFor(pe, r.Config)
	desiredDeploy := deploymentSpec(pe, r.Config)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = mergeLabels(deploy.Labels, pe.CommonLabels(), selectorLabels())
		// Selector é imutável no Deployment: sobrescrever num objeto que já
		// existe devolve erro de campo imutável e trava o reconcile para
		// sempre. Só é preenchido na criação.
		if deploy.Spec.Selector == nil {
			deploy.Spec.Selector = desiredDeploy.Selector
		}
		deploy.Spec.Replicas = desiredDeploy.Replicas
		deploy.Spec.Template = desiredDeploy.Template
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

// copyPullSecrets replica no namespace do preview os secrets de registry
// citados no spec. ImagePullSecrets é uma referência local ao namespace do
// pod, e o namespace do pod acabou de nascer vazio — sem a cópia, imagem de
// registry privado nunca sobe.
func (r *PreviewEnvironmentReconciler) copyPullSecrets(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) error {
	for _, ref := range pe.Spec.ImagePullSecrets {
		var src corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: pe.Namespace, Name: ref.Name}, &src); err != nil {
			if apierrors.IsNotFound(err) {
				r.event(pe, corev1.EventTypeWarning, "PullSecretAusente",
					fmt.Sprintf("secret %q não existe em %s", ref.Name, pe.Namespace))
			}
			return fmt.Errorf("pull secret %s: %w", ref.Name, err)
		}

		dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: pe.NamespaceName()}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dst, func() error {
			dst.Labels = mergeLabels(dst.Labels, pe.CommonLabels())
			dst.Type = src.Type
			dst.Data = src.Data
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

	if deploy.Status.ReadyReplicas >= desired && desired > 0 {
		r.transition(pe, previewv1alpha1.PhaseReady)
		r.setReady(pe, metav1.ConditionTrue, "Disponivel",
			fmt.Sprintf("%d de %d réplicas prontas", deploy.Status.ReadyReplicas, desired))
		r.setProgressing(pe, metav1.ConditionFalse, "Concluido", "ambiente no ar")
		return nil
	}

	r.transition(pe, previewv1alpha1.PhaseProvisioning)
	r.setReady(pe, metav1.ConditionFalse, "Subindo",
		fmt.Sprintf("%d de %d réplicas prontas", deploy.Status.ReadyReplicas, desired))
	r.setProgressing(pe, metav1.ConditionTrue, "Subindo", "aguardando as réplicas ficarem prontas")
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

	if err := r.publishStatus(ctx, pe); err != nil {
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
		if err := r.publishStatus(ctx, pe); err != nil {
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

	// Um namespace com o mesmo nome que não seja nosso não é apagado. O nome
	// é derivado do repositório e do PR, mas nada impede alguém de ter criado
	// um namespace assim na mão antes.
	if ns.Labels[previewv1alpha1.LabelManagedBy] != previewv1alpha1.ManagedByValue {
		r.event(pe, corev1.EventTypeWarning, "NamespaceDeTerceiro",
			fmt.Sprintf("namespace %s existe e não foi criado por este operator; não será apagado", ns.Name))
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

// publishStatus grava o status observado. observedGeneration é escrito aqui e
// em nenhum outro lugar: é o carimbo de qual spec este status responde.
func (r *PreviewEnvironmentReconciler) publishStatus(ctx context.Context, pe *previewv1alpha1.PreviewEnvironment) error {
	expiry := metav1.NewTime(pe.ExpiryTime())
	pe.Status.ExpiresAt = &expiry
	pe.Status.ObservedGeneration = pe.Generation
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

	porRepositorio := map[string]int{}
	for i := range list.Items {
		porRepositorio[list.Items[i].Spec.Repository]++
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

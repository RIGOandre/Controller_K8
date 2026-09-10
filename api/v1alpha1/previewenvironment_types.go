package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase é o resumo de uma linha do estado do ambiente. Quem precisa de
// detalhe lê as conditions; a phase existe para o `kubectl get` caber na tela.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Expired;Failed
type Phase string

const (
	// PhasePending é o estado antes do primeiro reconcile completar.
	PhasePending Phase = "Pending"
	// PhaseProvisioning indica objetos aplicados e pods ainda subindo.
	PhaseProvisioning Phase = "Provisioning"
	// PhaseReady indica ao menos uma réplica pronta e a URL publicada.
	PhaseReady Phase = "Ready"
	// PhaseExpired indica que o TTL venceu e o ambiente foi derrubado.
	PhaseExpired Phase = "Expired"
	// PhaseFailed indica erro que não se resolve em nova tentativa sozinho.
	PhaseFailed Phase = "Failed"
)

// Tipos de condition publicados no status.
const (
	// ConditionReady acompanha a disponibilidade do workload.
	ConditionReady = "Ready"
	// ConditionProgressing fica verdadeiro enquanto há reconcile em andamento.
	ConditionProgressing = "Progressing"
	// ConditionExpired marca o vencimento do TTL.
	ConditionExpired = "Expired"
)

// Finalizer que segura a remoção do CR até o namespace do preview sair junto.
const Finalizer = "preview.rigo.dev/cleanup"

// PreviewEnvironmentSpec descreve o ambiente efêmero de um pull request.
type PreviewEnvironmentSpec struct {
	// Repository é o repositório de origem no formato owner/name.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repository string `json:"repository"`

	// PullRequest é o número do PR que pediu o ambiente.
	// +kubebuilder:validation:Minimum=1
	PullRequest int32 `json:"pullRequest"`

	// Commit é o SHA que gerou a imagem. Só informativo: vira label e evento.
	// +optional
	Commit string `json:"commit,omitempty"`

	// Image é a imagem já construída pelo CI do repositório de origem.
	// O operator não constrói nada — quem constrói é quem já tem o contexto.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// ImagePullSecrets são copiados para o namespace do preview antes do deploy.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Port é a porta que o container escuta.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`

	// Replicas do deployment do preview.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Env são as variáveis do container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Host sobrescreve o hostname calculado a partir de --base-domain.
	// Sem host e sem base domain o operator não cria Ingress.
	// +optional
	Host string `json:"host,omitempty"`

	// TTL conta a partir da criação do objeto. Vencido, o ambiente é derrubado.
	// +kubebuilder:default="24h"
	// +optional
	TTL metav1.Duration `json:"ttl,omitempty"`

	// Resources do container. Sem valor, herda o default do manager.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PreviewEnvironmentStatus é o que o controller publica de volta.
type PreviewEnvironmentStatus struct {
	// Phase resume o estado em uma palavra.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Namespace é onde o ambiente foi materializado.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// URL é o endereço público, quando há Ingress.
	// +optional
	URL string `json:"url,omitempty"`

	// ExpiresAt é o instante em que o TTL vence.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// ReadyReplicas é o que o Deployment do preview reporta.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// ObservedGeneration é a generation do spec que este status responde.
	// Sem isso, quem observa não sabe se está lendo status velho.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carregam o detalhe que a phase não cabe.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pe;preview
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="#",type=integer,JSONPath=`.spec.pullRequest`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Expira",type=date,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="Idade",type=date,JSONPath=`.metadata.creationTimestamp`

// PreviewEnvironment é um ambiente efêmero de pull request.
type PreviewEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PreviewEnvironmentSpec   `json:"spec,omitempty"`
	Status PreviewEnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PreviewEnvironmentList é a coleção de PreviewEnvironment.
type PreviewEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreviewEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PreviewEnvironment{}, &PreviewEnvironmentList{})
}

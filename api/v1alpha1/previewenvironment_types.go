package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase é o resumo de uma linha do estado do ambiente. Quem precisa de
// detalhe lê as conditions; a phase existe para o `kubectl get` caber na tela.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Expired;Terminating;Failed
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
	// PhaseTerminating indica remoção pedida e ainda não concluída.
	PhaseTerminating Phase = "Terminating"
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
	//
	// Imutável: o nome do namespace do preview deriva deste campo. Deixar
	// editar troca o namespace de destino e abandona o antigo, com o workload
	// dentro, consumindo quota sem nenhum objeto que aponte para ele.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="repository é imutável: o namespace do preview deriva dele"
	Repository string `json:"repository"`

	// PullRequest é o número do PR que pediu o ambiente.
	//
	// Imutável, pelo mesmo motivo de repository.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pullRequest é imutável: o namespace do preview deriva dele"
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

	// Env são as variáveis do container, e só valores literais.
	//
	// valueFrom é recusado de propósito. Ele resolve no namespace do pod, que
	// é onde o operator acabou de copiar o pull secret — quem escreve o CR
	// passaria a ler, dentro de uma imagem que ele mesmo escolheu, qualquer
	// Secret que o operator tenha alcançado. O operator não tem como saber se
	// quem pediu tem direito àquele segredo, então não empresta o acesso dele.
	// +kubebuilder:validation:XValidation:rule="self.all(e, !has(e.valueFrom))",message="env aceita só valor literal; valueFrom resolveria no namespace do preview com o acesso do operator"
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Subdomain é o rótulo que precede o domínio base do manager. Vazio, o
	// operator calcula um a partir do repositório e do número do PR.
	//
	// É rótulo e não hostname inteiro de propósito. Aceitando hostname
	// completo, quem abre o pull request escolheria qualquer nome no ingress
	// controller compartilhado — inclusive um nome interno que ainda não tem
	// Ingress — e passaria a servi-lo a partir do container do PR. O domínio
	// é decisão do cluster, e o cluster a mantém.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Subdomain string `json:"subdomain,omitempty"`

	// TTL conta a partir da criação do objeto. Vencido, o ambiente é derrubado.
	// Formato do time.ParseDuration do Go: "24h", "1h30m", "90m".
	//
	// É string e não metav1.Duration de propósito, e essa é a diferença entre
	// um objeto ruim e um cluster parado. metav1.Duration decodifica dentro do
	// informer com time.ParseDuration; um "24 horas" digitado em qualquer
	// namespace faria a LIST inteira falhar, em laço, e o controller pararia
	// de reconciliar TODOS os ambientes do cluster — sem nada no status de
	// ninguém, só no log do manager. Com string, o pior caso é um ambiente
	// cair no TTL padrão.
	//
	// O campo antes era ponteiro por causa do default: metav1.Duration é
	// struct e `omitempty` não omite struct, então o campo ia na requisição
	// como "0s" mesmo sem ninguém ter pedido e o default do CRD nunca era
	// aplicado. String vazia omite sozinha.
	// O teto e o piso vão em CEL porque o pattern não sabe comparar grandezas:
	// "-5h" e "9999h" casam com o formato e são igualmente ruins — o primeiro
	// nasce vencido, o segundo transforma ambiente efêmero em permanente.
	//
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s') && duration(self) <= duration('168h')",message="ttl precisa ser positivo e no máximo 168h"
	// +kubebuilder:default="24h"
	// +optional
	TTL string `json:"ttl,omitempty"`

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

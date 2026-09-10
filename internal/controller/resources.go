package controller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	previewv1alpha1 "github.com/RIGOandre/Controller_K8/api/v1alpha1"
)

// Nomes fixos dentro do namespace do preview. O namespace é exclusivo do
// ambiente, então não há motivo para sufixar nada aqui.
const (
	appName        = "preview"
	containerName  = "app"
	quotaName      = "preview-quota"
	deploymentName = "preview"
	serviceName    = "preview"
	ingressName    = "preview"
)

// Config são as decisões que pertencem ao cluster, não ao pull request. Vêm
// por flag do manager: quem abre o PR não deveria precisar saber o domínio,
// a ingress class nem o teto de recurso do ambiente.
type Config struct {
	BaseDomain       string
	IngressClassName string
	// QuotaCPU e QuotaMemory limitam o namespace inteiro. Preview roda código
	// de PR, inclusive de fork: sem teto, um loop infinito derruba o cluster.
	QuotaCPU    resource.Quantity
	QuotaMemory resource.Quantity
	QuotaPods   int64
	// DefaultResources vale quando o spec não pede nada.
	DefaultResources corev1.ResourceRequirements
}

// selectorLabels são os únicos rótulos que entram no selector do Deployment.
// Selector é imutável depois de criado: se `CommonLabels` entrasse aqui,
// mudar o commit no spec quebraria o update com erro de campo imutável.
func selectorLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": appName}
}

func mergeLabels(base map[string]string, extra ...map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for _, m := range extra {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// namespaceFor descreve o namespace do ambiente. Os rótulos de Pod Security
// Admission entram aqui porque é o namespace que os aplica — é o que impede
// o pod de um PR qualquer de pedir privilégio ou montar o host.
func namespaceFor(pe *previewv1alpha1.PreviewEnvironment) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: pe.NamespaceName(),
			Labels: mergeLabels(pe.CommonLabels(), map[string]string{
				"pod-security.kubernetes.io/enforce": "baseline",
				"pod-security.kubernetes.io/warn":    "restricted",
				"pod-security.kubernetes.io/audit":   "restricted",
			}),
		},
	}
}

// quotaFor limita o namespace inteiro.
func quotaFor(pe *previewv1alpha1.PreviewEnvironment, cfg Config) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      quotaName,
			Namespace: pe.NamespaceName(),
			Labels:    pe.CommonLabels(),
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceLimitsCPU:      cfg.QuotaCPU,
				corev1.ResourceLimitsMemory:   cfg.QuotaMemory,
				corev1.ResourceRequestsCPU:    cfg.QuotaCPU,
				corev1.ResourceRequestsMemory: cfg.QuotaMemory,
				corev1.ResourcePods:           *resource.NewQuantity(cfg.QuotaPods, resource.DecimalSI),
			},
		},
	}
}

// deploymentSpec é a parte do Deployment que o reconcile reescreve a cada
// passada. Fica separada do objeto para o mutate do CreateOrUpdate poder
// aplicá-la sobre o que já existe sem tocar em selector nem em metadata.
func deploymentSpec(pe *previewv1alpha1.PreviewEnvironment, cfg Config) appsv1.DeploymentSpec {
	replicas := int32(1)
	if pe.Spec.Replicas != nil {
		replicas = *pe.Spec.Replicas
	}
	port := pe.Spec.Port
	if port == 0 {
		port = 8080
	}
	resources := pe.Spec.Resources
	if resources.Limits == nil && resources.Requests == nil {
		resources = cfg.DefaultResources
	}

	return appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{MatchLabels: selectorLabels()},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: mergeLabels(pe.CommonLabels(), selectorLabels()),
			},
			Spec: corev1.PodSpec{
				ImagePullSecrets: pe.Spec.ImagePullSecrets,
				Containers: []corev1.Container{{
					Name:  containerName,
					Image: pe.Spec.Image,
					Ports: []corev1.ContainerPort{{
						Name:          "http",
						ContainerPort: port,
						Protocol:      corev1.ProtocolTCP,
					}},
					Env:       pe.Spec.Env,
					Resources: resources,
					// Sonda de TCP e não de HTTP: o operator não conhece a
					// rota de saúde da aplicação do PR, e chutar /healthz
					// deixaria o ambiente eternamente NotReady.
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
						},
						PeriodSeconds:    5,
						FailureThreshold: 6,
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				}},
			},
		},
	}
}

func deploymentFor(pe *previewv1alpha1.PreviewEnvironment, cfg Config) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: pe.NamespaceName(),
			Labels:    mergeLabels(pe.CommonLabels(), selectorLabels()),
		},
		Spec: deploymentSpec(pe, cfg),
	}
}

func serviceSpec(pe *previewv1alpha1.PreviewEnvironment) corev1.ServiceSpec {
	port := pe.Spec.Port
	if port == 0 {
		port = 8080
	}
	return corev1.ServiceSpec{
		Selector: selectorLabels(),
		Ports: []corev1.ServicePort{{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		}},
	}
}

func serviceFor(pe *previewv1alpha1.PreviewEnvironment) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: pe.NamespaceName(),
			Labels:    pe.CommonLabels(),
		},
		Spec: serviceSpec(pe),
	}
}

func ingressSpec(host string, cfg Config) networkingv1.IngressSpec {
	pathType := networkingv1.PathTypePrefix
	spec := networkingv1.IngressSpec{
		Rules: []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: serviceName,
								Port: networkingv1.ServiceBackendPort{Number: 80},
							},
						},
					}},
				},
			},
		}},
	}
	if cfg.IngressClassName != "" {
		spec.IngressClassName = &cfg.IngressClassName
	}
	return spec
}

func ingressFor(pe *previewv1alpha1.PreviewEnvironment, host string, cfg Config) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressName,
			Namespace: pe.NamespaceName(),
			Labels:    pe.CommonLabels(),
			Annotations: map[string]string{
				"preview.rigo.dev/pull-request-url": fmt.Sprintf("https://github.com/%s/pull/%d", pe.Spec.Repository, pe.Spec.PullRequest),
			},
		},
		Spec: ingressSpec(host, cfg),
	}
}

func ptr[T any](v T) *T { return &v }

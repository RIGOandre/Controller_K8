package controller

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	previewv1alpha1 "github.com/RIGOandre/Controller_K8/api/v1alpha1"
)

func TestZZProbeDiff(t *testing.T) {
	amb := apiserver(t)
	direto, err := client.New(compartilhado.Config, client.Options{Scheme: clienteMgr.Scheme()})
	if err != nil {
		t.Fatal(err)
	}

	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "probe2", Namespace: "previews",
			CreationTimestamp: metav1.Now(),
		},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository: "acme/probe2", PullRequest: 98,
			Image: "ghcr.io/acme/probe2:v1", Port: 3000,
		},
	}
	r := &PreviewEnvironmentReconciler{Client: direto, Scheme: clienteMgr.Scheme(), Config: testConfig()}
	if err := r.apply(amb.ctx, pe); err != nil {
		t.Fatalf("apply1: %v", err)
	}
	nsp := pe.NamespaceName()
	key := types.NamespacedName{Namespace: nsp, Name: deploymentName}

	var antes appsv1.Deployment
	if err := direto.Get(amb.ctx, key, &antes); err != nil {
		t.Fatal(err)
	}
	t.Logf("apos create: rv=%s gen=%d", antes.ResourceVersion, antes.Generation)

	// simula o mutate
	existente := antes.DeepCopy()
	mutado := antes.DeepCopy()
	desired := deploymentSpec(pe, testConfig())
	mutado.Labels = mergeLabels(mutado.Labels, pe.CommonLabels(), selectorLabels())
	mutado.Spec.Replicas = desired.Replicas
	mutado.Spec.Template = desired.Template
	t.Logf("DeepEqual antes/depois do mutate: %v", equality.Semantic.DeepEqual(existente, mutado))

	c0 := existente.Spec.Template.Spec.Containers[0]
	c1 := mutado.Spec.Template.Spec.Containers[0]
	t.Logf("existente: terminationMessagePath=%q terminationMessagePolicy=%q imagePullPolicy=%q",
		c0.TerminationMessagePath, c0.TerminationMessagePolicy, c0.ImagePullPolicy)
	t.Logf("mutado:    terminationMessagePath=%q terminationMessagePolicy=%q imagePullPolicy=%q",
		c1.TerminationMessagePath, c1.TerminationMessagePolicy, c1.ImagePullPolicy)
	t.Logf("existente probe: %+v", c0.ReadinessProbe)
	t.Logf("mutado    probe: %+v", c1.ReadinessProbe)
	t.Logf("existente podspec: restart=%q dns=%q sched=%q grace=%v secctx=%+v",
		existente.Spec.Template.Spec.RestartPolicy, existente.Spec.Template.Spec.DNSPolicy,
		existente.Spec.Template.Spec.SchedulerName, existente.Spec.Template.Spec.TerminationGracePeriodSeconds,
		existente.Spec.Template.Spec.SecurityContext)
	t.Logf("mutado    podspec: restart=%q dns=%q sched=%q grace=%v secctx=%+v",
		mutado.Spec.Template.Spec.RestartPolicy, mutado.Spec.Template.Spec.DNSPolicy,
		mutado.Spec.Template.Spec.SchedulerName, mutado.Spec.Template.Spec.TerminationGracePeriodSeconds,
		mutado.Spec.Template.Spec.SecurityContext)
	t.Logf("existente resources=%+v", c0.Resources)
	t.Logf("mutado    resources=%+v", c1.Resources)

	// agora 3 applies seguidos com client direto: rv muda?
	for i := 0; i < 3; i++ {
		if err := r.apply(amb.ctx, pe); err != nil {
			t.Fatalf("apply: %v", err)
		}
		var d appsv1.Deployment
		if err := direto.Get(amb.ctx, key, &d); err != nil {
			t.Fatal(err)
		}
		t.Logf("apos apply %d: rv=%s gen=%d", i+2, d.ResourceVersion, d.Generation)
		time.Sleep(200 * time.Millisecond)
	}
}

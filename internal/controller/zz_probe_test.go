package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	previewv1alpha1 "github.com/RIGOandre/Controller_K8/api/v1alpha1"
)

type spyClient struct {
	client.Client
	updates []string
	creates []string
}

func (s *spyClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	s.updates = append(s.updates, fmt.Sprintf("%T %s/%s", obj, obj.GetNamespace(), obj.GetName()))
	return s.Client.Update(ctx, obj, opts...)
}

func (s *spyClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	s.creates = append(s.creates, fmt.Sprintf("%T %s/%s", obj, obj.GetNamespace(), obj.GetName()))
	return s.Client.Create(ctx, obj, opts...)
}

func TestZZProbeIdempotencia(t *testing.T) {
	amb := apiserver(t)

	spy := &spyClient{Client: amb.c}
	r := &PreviewEnvironmentReconciler{
		Client: spy,
		Scheme: clienteMgr.Scheme(),
		Config: testConfig(),
	}

	pe := &previewv1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "probe", Namespace: "previews",
			CreationTimestamp: metav1.Now(),
		},
		Spec: previewv1alpha1.PreviewEnvironmentSpec{
			Repository:  "acme/probe",
			PullRequest: 99,
			Image:       "ghcr.io/acme/probe:v1",
			Port:        3000,
		},
	}

	if err := r.apply(amb.ctx, pe); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	t.Logf("PASSADA 1 creates=%v updates=%v", spy.creates, spy.updates)

	nsp := pe.NamespaceName()
	amb.ateQue("cache ver tudo", func() error {
		var d appsv1.Deployment
		if err := amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsp, Name: deploymentName}, &d); err != nil {
			return err
		}
		var s corev1.Service
		if err := amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsp, Name: serviceName}, &s); err != nil {
			return err
		}
		var i networkingv1.Ingress
		if err := amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsp, Name: ingressName}, &i); err != nil {
			return err
		}
		var q corev1.ResourceQuota
		return amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsp, Name: quotaName}, &q)
	})

	for passada := 2; passada <= 4; passada++ {
		spy.updates = nil
		spy.creates = nil
		if err := r.apply(amb.ctx, pe); err != nil {
			t.Fatalf("apply %d: %v", passada, err)
		}
		t.Logf("PASSADA %d creates=%v updates=%v", passada, spy.creates, spy.updates)
		time.Sleep(500 * time.Millisecond)
	}

	var d appsv1.Deployment
	if err := amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsp, Name: deploymentName}, &d); err != nil {
		t.Fatal(err)
	}
	t.Logf("deployment generation=%d resourceVersion=%s", d.Generation, d.ResourceVersion)
	var i networkingv1.Ingress
	if err := amb.c.Get(amb.ctx, types.NamespacedName{Namespace: nsp, Name: ingressName}, &i); err != nil {
		t.Fatal(err)
	}
	t.Logf("ingress annotations=%v ingressClassName=%v", i.Annotations, i.Spec.IngressClassName)
}

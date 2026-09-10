package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	previewv1alpha1 "github.com/RIGOandre/preview-operator/api/v1alpha1"
)

// Soltar o finalizer assim que o Delete do namespace volta deixaria um
// namespace em Terminating sem dono. Se ele travasse ali — e trava, sempre
// que um recurso do namespace tem finalizer próprio — ninguém mais saberia
// de onde veio nem quem devia limpar.
func TestFinalizerSeguraOCRAteONamespaceSumir(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	ctx := context.Background()
	chaveNS := types.NamespacedName{Name: pe.NamespaceName()}

	// Um finalizer no namespace reproduz o Terminating: o client falso passa
	// a marcar deletionTimestamp em vez de sumir com o objeto na hora.
	var ns corev1.Namespace
	if err := s.c.Get(ctx, chaveNS, &ns); err != nil {
		t.Fatal(err)
	}
	ns.Finalizers = append(ns.Finalizers, "teste/segura")
	if err := s.c.Update(ctx, &ns); err != nil {
		t.Fatal(err)
	}

	atual := s.lerAmbiente(t, pe)
	if err := s.c.Delete(ctx, atual); err != nil {
		t.Fatal(err)
	}

	if res := s.reconcile(t, pe); res.RequeueAfter != terminatingRequeue {
		t.Fatalf("queria nova conferência em %s, veio %s", terminatingRequeue, res.RequeueAfter)
	}
	if err := s.c.Get(ctx, client.ObjectKeyFromObject(pe), &previewv1alpha1.PreviewEnvironment{}); err != nil {
		t.Fatalf("o CR saiu antes do namespace: %v", err)
	}
	if err := s.c.Get(ctx, chaveNS, &ns); err != nil {
		t.Fatal(err)
	}
	if ns.DeletionTimestamp.IsZero() {
		t.Fatal("o namespace não foi marcado para remoção")
	}

	// Quem quer que segurasse o namespace terminou.
	ns.Finalizers = nil
	if err := s.c.Update(ctx, &ns); err != nil {
		t.Fatal(err)
	}

	s.reconcile(t, pe)

	err := s.c.Get(ctx, client.ObjectKeyFromObject(pe), &previewv1alpha1.PreviewEnvironment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("o CR devia ter saído com o namespace: %v", err)
	}
}

// O nome do namespace é derivado do repositório e do PR, mas nada impede que
// alguém já tivesse um namespace com esse nome. Apagar o que não é nosso
// seria transformar um preview num incidente.
func TestNaoApagaNamespaceQueNaoEDoOperator(t *testing.T) {
	pe := novoAmbiente(func(p *previewv1alpha1.PreviewEnvironment) {
		p.Finalizers = []string{previewv1alpha1.Finalizer}
		agora := metav1.NewTime(criacao)
		p.DeletionTimestamp = &agora
	})
	alheio := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   pe.NamespaceName(),
			Labels: map[string]string{"dono": "outro-time"},
		},
	}
	s := monta(t, criacao, pe, alheio)

	s.reconcile(t, pe)

	ctx := context.Background()
	var ns corev1.Namespace
	if err := s.c.Get(ctx, types.NamespacedName{Name: pe.NamespaceName()}, &ns); err != nil {
		t.Fatalf("o namespace de terceiro foi apagado: %v", err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		t.Fatal("o namespace de terceiro foi marcado para remoção")
	}

	// O CR sai mesmo assim: segurá-lo para sempre por um namespace que nunca
	// será apagado só criaria um objeto impossível de remover.
	err := s.c.Get(ctx, client.ObjectKeyFromObject(pe), &previewv1alpha1.PreviewEnvironment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("o CR ficou preso: %v", err)
	}
}

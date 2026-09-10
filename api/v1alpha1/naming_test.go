package v1alpha1

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func env(repo string, pr int32) *PreviewEnvironment {
	return &PreviewEnvironment{Spec: PreviewEnvironmentSpec{Repository: repo, PullRequest: pr}}
}

func TestNamespaceNameEhUmLabelValido(t *testing.T) {
	casos := []struct{ repo string }{
		{"RIGOandre/preview-operator"},
		{"acme/API_Gateway"},
		{"Org.With.Dots/repo.name"},
		{"a/b"},
		{strings.Repeat("owner", 20) + "/" + strings.Repeat("repo", 20)},
	}
	for _, c := range casos {
		ns := env(c.repo, 42).NamespaceName()
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			t.Fatalf("%q gerou namespace inválido %q: %v", c.repo, ns, errs)
		}
	}
}

// Dois repositórios longos com o mesmo prefixo são o caso que um corte cego
// erraria: viravam o mesmo namespace e um preview apagaria o outro.
func TestNamespacesLongosNaoColidem(t *testing.T) {
	base := strings.Repeat("mesmo-prefixo-", 6)
	a := env("acme/"+base+"alpha", 1).NamespaceName()
	b := env("acme/"+base+"beta", 1).NamespaceName()

	if a == b {
		t.Fatalf("nomes colidiram: %q", a)
	}
	if len(a) > 63 || len(b) > 63 {
		t.Fatalf("passou de 63: %d e %d", len(a), len(b))
	}
}

func TestNamespaceMudaComOPullRequest(t *testing.T) {
	a := env("acme/loja", 10).NamespaceName()
	b := env("acme/loja", 11).NamespaceName()
	if a == b {
		t.Fatalf("PRs diferentes geraram o mesmo namespace: %q", a)
	}
}

func TestHostFor(t *testing.T) {
	casos := []struct {
		nome       string
		pe         *PreviewEnvironment
		baseDomain string
		querido    string
	}{
		{"monta do domínio base", env("acme/loja", 7), "preview.exemplo.dev", "pr-7-acme-loja.preview.exemplo.dev"},
		{"spec.host manda", &PreviewEnvironment{Spec: PreviewEnvironmentSpec{Repository: "acme/loja", PullRequest: 7, Host: "fixo.exemplo.dev"}}, "preview.exemplo.dev", "fixo.exemplo.dev"},
		{"sem domínio não há host", env("acme/loja", 7), "", ""},
		{"ponto sobrando no domínio", env("acme/loja", 7), ".preview.exemplo.dev", "pr-7-acme-loja.preview.exemplo.dev"},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := c.pe.HostFor(c.baseDomain); got != c.querido {
				t.Fatalf("queria %q, veio %q", c.querido, got)
			}
		})
	}
}

func TestHostForCortaORotuloEm63(t *testing.T) {
	pe := env("organizacao-com-nome-enorme/repositorio-com-nome-ainda-maior-que-o-outro", 12345)
	host := pe.HostFor("preview.exemplo.dev")
	rotulo, _, _ := strings.Cut(host, ".")
	if errs := validation.IsDNS1123Label(rotulo); len(errs) > 0 {
		t.Fatalf("rótulo %q inválido: %v", rotulo, errs)
	}
}

func TestTTLCaiNoDefaultQuandoNaoInformado(t *testing.T) {
	pe := env("acme/loja", 1)
	if got := pe.TTLDuration(); got != DefaultTTL {
		t.Fatalf("queria %s, veio %s", DefaultTTL, got)
	}
	pe.Spec.TTL = &metav1.Duration{Duration: -time.Hour}
	if got := pe.TTLDuration(); got != DefaultTTL {
		t.Fatalf("TTL negativo devia cair no default, veio %s", got)
	}
}

// O vencimento conta da criação, não do último reconcile. Contar do reconcile
// deixaria um ambiente vivo para sempre se alguém editasse o spec de hora em
// hora — exatamente o que o TTL existe para impedir.
func TestExpiryContaDaCriacao(t *testing.T) {
	criado := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	pe := env("acme/loja", 1)
	pe.CreationTimestamp = metav1.NewTime(criado)
	pe.Spec.TTL = &metav1.Duration{Duration: 3 * time.Hour}

	if got := pe.ExpiryTime(); !got.Equal(criado.Add(3 * time.Hour)) {
		t.Fatalf("queria %s, veio %s", criado.Add(3*time.Hour), got)
	}
}

func TestCommonLabelsSaoValidos(t *testing.T) {
	pe := env("RIGOandre/preview-operator", 9)
	pe.Namespace = "previews"
	pe.Name = "loja-pr-9"
	pe.Spec.Commit = "A1B2C3D4E5F6"

	labels := pe.CommonLabels()
	for k, v := range labels {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			t.Fatalf("chave %q inválida: %v", k, errs)
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Fatalf("valor %q da chave %q inválido: %v", v, k, errs)
		}
	}
	if labels[LabelOwnerName] != "loja-pr-9" || labels[LabelOwnerNamespace] != "previews" {
		t.Fatalf("o caminho de volta ao dono se perdeu: %v", labels)
	}
	if labels[LabelCommit] != "a1b2c3d4e5f6" {
		t.Fatalf("commit devia virar minúsculo, veio %q", labels[LabelCommit])
	}
}

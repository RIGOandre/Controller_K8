package controller

import (
	"sort"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Nome e rótulo de métrica são interface pública: o alerta do cluster e o
// painel do Grafana moram em outro repositório e casam por string. Trocar
// "erro" por "error" ou tirar um rótulo não quebra compilação nenhuma — quebra
// o alerta, que simplesmente para de disparar. Silêncio é o pior modo de
// falha possível para um alerta, então o contrato fica travado aqui.
func TestContratoDasMetricas(t *testing.T) {
	pe := novoAmbiente()
	s := monta(t, criacao, pe)
	s.reconcile(t, pe)
	s.reconcile(t, pe)

	familias, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("coletar métricas: %v", err)
	}

	querido := map[string][]string{
		"preview_environments_active":                    {},
		"preview_environment_transitions_total":          {"phase"},
		"preview_environment_reconcile_duration_seconds": {"result"},
		"preview_environment_expires_at_seconds":         {"name", "namespace", "pull_request", "repository"},
	}

	vistas := map[string]bool{}
	for _, familia := range familias {
		nome := familia.GetName()
		rotulosQueridos, nossa := querido[nome]
		if !nossa {
			continue
		}
		vistas[nome] = true
		if len(familia.GetMetric()) == 0 {
			t.Errorf("%s existe mas não tem amostra", nome)
			continue
		}
		if got := rotulosDe(familia.GetMetric()[0]); !mesmaLista(got, rotulosQueridos) {
			t.Errorf("%s: rótulos %v, queria %v", nome, got, rotulosQueridos)
		}
	}

	for nome := range querido {
		if !vistas[nome] {
			t.Errorf("métrica %s sumiu do registry", nome)
		}
	}
}

// O alerta PreviewOperatorReconciliacaoFalhando filtra por result="error".
// Este teste é o que garante que o valor emitido é esse mesmo.
func TestValoresDoRotuloResult(t *testing.T) {
	if resultadoOK != "success" || resultadoErro != "error" {
		t.Fatalf("os valores de result mudaram (%q, %q); os alertas do cluster casam por string",
			resultadoOK, resultadoErro)
	}
}

func rotulosDe(m *dto.Metric) []string {
	var nomes []string
	for _, par := range m.GetLabel() {
		nomes = append(nomes, par.GetName())
	}
	sort.Strings(nomes)
	return nomes
}

func mesmaLista(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

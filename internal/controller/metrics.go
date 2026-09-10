package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Métricas do operator. Vão para o registry do controller-runtime, que já é
// exposto em /metrics — não há segundo servidor de métrica aqui.
//
// A pergunta que estas quatro respondem: quantos ambientes existem agora,
// quantos já subiram e caíram, quanto tempo o reconcile leva e quando cada
// ambiente vence. A última é a que permite alertar antes do preview sumir
// debaixo de quem está revisando o PR.
var (
	activeEnvironments = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "preview_environments_active",
		Help: "Ambientes de preview existentes no cluster.",
	})

	transitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "preview_environment_transitions_total",
		Help: "Transições de fase, contadas por fase de destino.",
	}, []string{"phase"})

	reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "preview_environment_reconcile_duration_seconds",
		Help:    "Duração do reconcile, por resultado.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"result"})

	expiryTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "preview_environment_expires_at_seconds",
		Help: "Instante do vencimento do TTL, em epoch de segundos.",
	}, []string{"namespace", "name"})
)

func init() {
	metrics.Registry.MustRegister(activeEnvironments, transitions, reconcileDuration, expiryTimestamp)
}

// forgetEnvironment tira o ambiente das séries com rótulo por objeto. Sem
// isso o Prometheus continuaria vendo o vencimento de um preview apagado
// meses atrás, e todo alerta em cima da série ficaria preso no passado.
func forgetEnvironment(namespace, name string) {
	expiryTimestamp.DeleteLabelValues(namespace, name)
}

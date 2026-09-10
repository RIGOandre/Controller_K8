package controller

import (
	"sync"

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
	// Quebrado por repositório e não um total só: num cluster que atende mais
	// de um projeto, "sete ambientes no ar" não diz de quem eles são nem qual
	// repositório está segurando a quota. A cardinalidade é o número de
	// repositórios, que é pequeno e não cresce com o tempo.
	activeEnvironments = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "preview_environments_active",
		Help: "Ambientes de preview existentes no cluster, por repositório.",
	}, []string{"repository"})

	transitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "preview_environment_transitions_total",
		Help: "Transições de fase, contadas por fase de destino e repositório.",
	}, []string{"phase", "repository"})

	reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "preview_environment_reconcile_duration_seconds",
		Help:    "Duração do reconcile, por resultado.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"result"})

	// repository e pull_request entram como rótulo porque quem lê o alerta
	// precisa saber de qual PR se trata. Com namespace e nome do objeto só, o
	// aviso de "vence em 30 minutos" chega dizendo `pr-42` e obriga a abrir o
	// cluster para descobrir de que repositório é.
	expiryTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "preview_environment_expires_at_seconds",
		Help: "Instante do vencimento do TTL, em epoch de segundos.",
	}, []string{"namespace", "name", "repository", "pull_request"})
)

// Valores do rótulo `result` do histograma. São consumidos por alerta e por
// dashboard, então ficam em inglês junto com o nome da métrica — um filtro
// result="error" que não casa com "erro" é um alerta que nunca dispara, e a
// falha é silenciosa dos dois lados.
const (
	resultadoOK   = "success"
	resultadoErro = "error"
)

// travaAtivos serializa o par Reset+Set do gauge de ativos. Com mais de um
// reconcile simultâneo, dois recontagens interleavadas deixariam a série num
// estado que nunca existiu — a segunda apagaria o que a primeira acabou de
// escrever, no meio da escrita.
var travaAtivos sync.Mutex

// publicarAtivos troca o conjunto inteiro de séries de uma vez. O Reset é o
// que faz um repositório sem nenhum ambiente sumir do gráfico, em vez de
// congelar no último valor para sempre.
func publicarAtivos(porRepositorio map[string]int) {
	travaAtivos.Lock()
	defer travaAtivos.Unlock()

	activeEnvironments.Reset()
	for repositorio, total := range porRepositorio {
		activeEnvironments.WithLabelValues(repositorio).Set(float64(total))
	}
}

func init() {
	metrics.Registry.MustRegister(activeEnvironments, transitions, reconcileDuration, expiryTimestamp)
}

// forgetEnvironment tira o ambiente das séries com rótulo por objeto. Sem
// isso o Prometheus continuaria vendo o vencimento de um preview apagado
// meses atrás, e todo alerta em cima da série ficaria preso no passado.
//
// A remoção é por correspondência parcial: quem chama sabe o namespace e o
// nome do objeto, mas nem sempre o repositório e o PR — no caminho em que o
// CR já sumiu do cluster, o spec não existe mais para consultar.
func forgetEnvironment(namespace, name string) {
	expiryTimestamp.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace,
		"name":      name,
	})
}

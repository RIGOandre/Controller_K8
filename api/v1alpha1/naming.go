package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Rótulos que o operator carimba em tudo que cria. São o contrato de busca:
// `kubectl get all -A -l preview.rigo.dev/owner=<ns>/<nome>` acha o ambiente inteiro.
const (
	LabelOwner       = "preview.rigo.dev/owner"
	LabelRepository  = "preview.rigo.dev/repository"
	LabelPullRequest = "preview.rigo.dev/pull-request"
	LabelCommit      = "preview.rigo.dev/commit"
	LabelManagedBy   = "app.kubernetes.io/managed-by"

	// ManagedByValue identifica quem escreveu o objeto.
	ManagedByValue = "preview-operator"

	// maxLabelLen é o teto de um label DNS-1123, que é o formato de nome de
	// namespace e de cada rótulo de host.
	maxLabelLen = 63
)

// DefaultTTL vale quando o CRD não aplicou default (objeto montado em teste,
// por exemplo) ou quando alguém zerou o campo.
const DefaultTTL = 24 * time.Hour

// slug reduz texto livre a um label DNS-1123: minúsculo, alfanumérico e hífen,
// sem hífen repetido nem nas pontas.
func slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := true // começa true para não abrir com hífen
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// fit corta o nome para caber no teto e cola um sufixo derivado do nome
// inteiro. O corte cego colidiria: dois repositórios com o mesmo prefixo
// longo virariam o mesmo namespace, e um preview apagaria o outro.
func fit(name string, max int) string {
	if len(name) <= max {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:8]
	return strings.Trim(name[:max-len(suffix)-1], "-") + "-" + suffix
}

// RepositorySlug é o repositório em forma de label: owner-name.
func (pe *PreviewEnvironment) RepositorySlug() string {
	owner, name, found := strings.Cut(pe.Spec.Repository, "/")
	if !found {
		return slug(pe.Spec.Repository)
	}
	return slug(owner) + "-" + slug(name)
}

// NamespaceName é o namespace que abriga o ambiente. Determinístico de
// propósito: o reconcile precisa reencontrar o namespace sem ler o status,
// que pode não ter sido gravado se o processo morreu no meio.
func (pe *PreviewEnvironment) NamespaceName() string {
	return fit(fmt.Sprintf("preview-%s-%d", pe.RepositorySlug(), pe.Spec.PullRequest), maxLabelLen)
}

// HostFor devolve o hostname do Ingress. spec.host manda; sem ele, monta a
// partir do domínio base do manager. Sem os dois, devolve vazio e o
// controller não cria Ingress nenhum.
func (pe *PreviewEnvironment) HostFor(baseDomain string) string {
	if pe.Spec.Host != "" {
		return pe.Spec.Host
	}
	if baseDomain == "" {
		return ""
	}
	sub := fit(fmt.Sprintf("pr-%d-%s", pe.Spec.PullRequest, pe.RepositorySlug()), maxLabelLen)
	return sub + "." + strings.TrimPrefix(baseDomain, ".")
}

// TTLDuration é o TTL efetivo, com piso no default.
func (pe *PreviewEnvironment) TTLDuration() time.Duration {
	if pe.Spec.TTL.Duration <= 0 {
		return DefaultTTL
	}
	return pe.Spec.TTL.Duration
}

// ExpiryTime é o instante do vencimento, contado da criação do objeto.
// Contar da criação e não do último reconcile é o que impede um ambiente de
// viver para sempre só porque alguém edita o spec de hora em hora.
func (pe *PreviewEnvironment) ExpiryTime() time.Time {
	return pe.CreationTimestamp.Time.Add(pe.TTLDuration())
}

// CommonLabels são os rótulos comuns a todo objeto criado para este ambiente.
func (pe *PreviewEnvironment) CommonLabels() map[string]string {
	l := map[string]string{
		LabelManagedBy:   ManagedByValue,
		LabelOwner:       fit(pe.Namespace+"-"+pe.Name, maxLabelLen),
		LabelRepository:  fit(pe.RepositorySlug(), maxLabelLen),
		LabelPullRequest: fmt.Sprintf("%d", pe.Spec.PullRequest),
	}
	if pe.Spec.Commit != "" {
		l[LabelCommit] = fit(slug(pe.Spec.Commit), maxLabelLen)
	}
	return l
}

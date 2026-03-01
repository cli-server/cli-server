package namespace

import (
	"context"
	"fmt"
	"log"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// Config holds configuration for the namespace manager.
type Config struct {
	Prefix        string
	NetworkPolicy NetworkPolicyConfig
}

// NetworkPolicyConfig holds NetworkPolicy settings applied to each workspace namespace.
type NetworkPolicyConfig struct {
	Enabled            bool
	DenyCIDRs          []string
	CliServerNamespace string // Allow egress to cli-server namespace (for Anthropic API proxy).
}

// Manager handles per-workspace K8s namespace lifecycle.
type Manager struct {
	clientset kubernetes.Interface
	config    Config
}

// NewManager creates a new namespace Manager.
func NewManager(clientset kubernetes.Interface, config Config) *Manager {
	if config.Prefix == "" {
		config.Prefix = "cli-ws"
	}
	return &Manager{
		clientset: clientset,
		config:    config,
	}
}

// NamespaceName returns the K8s namespace name for a workspace ID.
func (m *Manager) NamespaceName(workspaceID string) string {
	short := workspaceID
	if len(short) > 8 {
		short = short[:8]
	}
	return m.config.Prefix + "-" + short
}

// EnsureNamespace creates the namespace if it does not exist, applies labels
// and NetworkPolicy. Returns the namespace name. Idempotent.
func (m *Manager) EnsureNamespace(ctx context.Context, workspaceID string) (string, error) {
	nsName := m.NamespaceName(workspaceID)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
			Labels: map[string]string{
				"managed-by":   "cli-server",
				"workspace-id": workspaceID,
			},
		},
	}

	_, err := m.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create namespace %s: %w", nsName, err)
	}

	if m.config.NetworkPolicy.Enabled {
		if err := m.ApplyNetworkPolicy(ctx, nsName); err != nil {
			log.Printf("warning: failed to apply network policy to %s: %v", nsName, err)
		}
	}

	return nsName, nil
}

// DeleteNamespace deletes the namespace. K8s cascades all resources within it.
func (m *Manager) DeleteNamespace(ctx context.Context, namespace string) error {
	err := m.clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", namespace, err)
	}
	return nil
}

// ApplyNetworkPolicy creates or updates the sandbox egress NetworkPolicy in the given namespace.
func (m *Manager) ApplyNetworkPolicy(ctx context.Context, namespace string) error {
	np := m.buildNetworkPolicy(namespace)

	_, err := m.clientset.NetworkingV1().NetworkPolicies(namespace).Get(ctx, np.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = m.clientset.NetworkingV1().NetworkPolicies(namespace).Create(ctx, np, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create network policy in %s: %w", namespace, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get network policy in %s: %w", namespace, err)
	}

	_, err = m.clientset.NetworkingV1().NetworkPolicies(namespace).Update(ctx, np, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update network policy in %s: %w", namespace, err)
	}
	return nil
}

func (m *Manager) buildNetworkPolicy(namespace string) *networkingv1.NetworkPolicy {
	dnsPort53 := intstr.FromInt32(53)
	protoUDP := corev1.ProtocolUDP
	protoTCP := corev1.ProtocolTCP

	egress := []networkingv1.NetworkPolicyEgressRule{
		// 1. Allow DNS to kube-system.
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": "kube-system",
					},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &protoUDP, Port: &dnsPort53},
				{Protocol: &protoTCP, Port: &dnsPort53},
			},
		},
		// 2. Allow traffic within the same namespace.
		{
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{},
			}},
		},
	}

	// 3. Allow traffic to cli-server namespace (for Anthropic API proxy).
	if m.config.NetworkPolicy.CliServerNamespace != "" {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": m.config.NetworkPolicy.CliServerNamespace,
					},
				},
			}},
		})
	}

	// 4. Allow internet, optionally blocking denied CIDRs.
	if len(m.config.NetworkPolicy.DenyCIDRs) > 0 {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{
					CIDR:   "0.0.0.0/0",
					Except: m.config.NetworkPolicy.DenyCIDRs,
				},
			}},
		})
	} else {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{
					CIDR: "0.0.0.0/0",
				},
			}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cli-server-sandbox-egress",
			Namespace: namespace,
			Labels: map[string]string{
				"managed-by": "cli-server",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"managed-by": "cli-server",
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// ParseDenyCIDRs splits a comma-separated CIDR string into a slice.
func ParseDenyCIDRs(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var cidrs []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			cidrs = append(cidrs, p)
		}
	}
	return cidrs
}

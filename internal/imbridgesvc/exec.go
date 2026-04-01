package imbridgesvc

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/imbridge"
)

const sandboxContainerName = "agent"

// Compile-time check.
var _ imbridge.ExecCommander = (*K8sExec)(nil)

// K8sExec implements imbridge.ExecCommander using K8s pod exec.
// It is a lightweight alternative to sandbox.Manager that only supports
// one-shot command execution (used for IPC group registration).
type K8sExec struct {
	db        *db.DB
	restCfg   *rest.Config
	clientset kubernetes.Interface
}

// NewK8sExec creates a K8sExec from in-cluster or KUBECONFIG config.
// Returns nil (not an error) if K8s is not available, so imbridge can
// degrade gracefully in non-K8s environments.
func NewK8sExec(database *db.DB) *K8sExec {
	restCfg, err := buildRESTConfig()
	if err != nil {
		log.Printf("imbridge: K8s exec unavailable (no cluster config): %v", err)
		return nil
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Printf("imbridge: K8s exec unavailable (clientset): %v", err)
		return nil
	}
	return &K8sExec{db: database, restCfg: restCfg, clientset: clientset}
}

func (e *K8sExec) ExecSimple(ctx context.Context, sandboxID string, command []string) (string, error) {
	ns, err := e.lookupNamespace(sandboxID)
	if err != nil {
		return "", err
	}

	sandboxName := "agent-sandbox-" + shortID(sandboxID)
	podName, err := e.findRunningPod(ctx, ns, sandboxName)
	if err != nil {
		return "", fmt.Errorf("find pod: %w", err)
	}

	req := e.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: sandboxContainerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	wsExec, err := remotecommand.NewWebSocketExecutor(e.restCfg, "POST", req.URL().String())
	if err != nil {
		return "", err
	}
	spdyExec, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	executor, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(error) bool { return true })
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.String(), nil
}

func (e *K8sExec) lookupNamespace(sandboxID string) (string, error) {
	sbx, err := e.db.GetSandbox(sandboxID)
	if err != nil || sbx == nil {
		return "", fmt.Errorf("sandbox %s not found", sandboxID)
	}
	ws, err := e.db.GetWorkspace(sbx.WorkspaceID)
	if err != nil || ws == nil {
		return "", fmt.Errorf("workspace %s not found", sbx.WorkspaceID)
	}
	if !ws.K8sNamespace.Valid || ws.K8sNamespace.String == "" {
		return "", fmt.Errorf("workspace %s has no k8s namespace", ws.ID)
	}
	return ws.K8sNamespace.String, nil
}

func (e *K8sExec) findRunningPod(ctx context.Context, namespace, sandboxName string) (string, error) {
	// List pods with the sandbox name hash label.
	nameHash := nameHash(sandboxName)
	podList, err := e.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "agents.x-k8s.io/sandbox-name-hash=" + nameHash,
	})
	if err != nil {
		return "", err
	}
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no running pod for sandbox %s", sandboxName)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func nameHash(name string) string {
	h := fnvHash(name)
	return fmt.Sprintf("%08x", h)
}

func fnvHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func buildRESTConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

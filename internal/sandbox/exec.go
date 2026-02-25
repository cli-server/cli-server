package sandbox

import (
	"context"
	"io"
	"net/http"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/imryao/cli-server/internal/process"
)

// Compile-time interface check.
var _ process.Process = (*execProcess)(nil)

// terminalSizeQueue implements remotecommand.TerminalSizeQueue.
type terminalSizeQueue struct {
	ch chan *remotecommand.TerminalSize
}

func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return size
}

// execProcess bridges a client-go remotecommand executor to the process.Process interface.
type execProcess struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	sizeQ   *terminalSizeQueue
	done    chan struct{}
	once    sync.Once
	cancel  context.CancelFunc
}

func (p *execProcess) Read(buf []byte) (int, error) {
	return p.stdoutR.Read(buf)
}

func (p *execProcess) Write(data []byte) (int, error) {
	return p.stdinW.Write(data)
}

func (p *execProcess) Resize(rows, cols uint16) error {
	select {
	case p.sizeQ.ch <- &remotecommand.TerminalSize{Width: cols, Height: rows}:
	default:
		// Drop resize if channel is full; next one will pick up current size.
	}
	return nil
}

func (p *execProcess) Done() <-chan struct{} {
	return p.done
}

func (p *execProcess) close() {
	p.cancel()
	p.stdinW.Close()
	p.stdoutW.Close()
	p.once.Do(func() { close(p.done) })
}

// createExecutor builds a remotecommand executor using WebSocket with SPDY fallback,
// matching the kubectl exec pattern.
func createExecutor(config *rest.Config, clientset kubernetes.Interface, namespace, podName, containerName string, command []string) (remotecommand.Executor, error) {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	wsExec, err := remotecommand.NewWebSocketExecutor(config, http.MethodPost, req.URL().String())
	if err != nil {
		return nil, err
	}

	spdyExec, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, req.URL())
	if err != nil {
		return nil, err
	}

	return remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
		return true
	})
}

// startExec creates an execProcess and runs the remotecommand stream in a goroutine.
func startExec(config *rest.Config, clientset kubernetes.Interface, namespace, podName, containerName string, command []string) (*execProcess, error) {
	executor, err := createExecutor(config, clientset, namespace, podName, containerName, command)
	if err != nil {
		return nil, err
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())

	p := &execProcess{
		stdinR:  stdinR,
		stdinW:  stdinW,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		sizeQ:   &terminalSizeQueue{ch: make(chan *remotecommand.TerminalSize, 1)},
		done:    make(chan struct{}),
		cancel:  cancel,
	}

	go func() {
		defer p.stdinR.Close()
		defer p.stdoutW.Close()
		defer p.once.Do(func() { close(p.done) })

		_ = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:             stdinR,
			Stdout:            stdoutW,
			Tty:               true,
			TerminalSizeQueue: p.sizeQ,
		})
	}()

	return p, nil
}

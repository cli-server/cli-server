package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/notebookjwt"
	"github.com/agentserver/agentserver/internal/notebooksupervisor"
)

// newNotebookTestSupervisor returns a Supervisor backed by a fake k8s
// clientset, with a goroutine pre-armed to mark the to-be-created
// Deployment ready so EnsureRunning succeeds.
func newNotebookTestSupervisor(t *testing.T, wsID, ns string) *notebooksupervisor.Supervisor {
	t.Helper()
	c := fake.NewClientset()
	cfg := notebooksupervisor.Config{
		Image:            "img:tag",
		WorkspacePVCName: "pvc",
		ReadyTimeout:     2 * time.Second,
	}.WithDefaults()
	sup := notebooksupervisor.New(c, cfg, nil)

	// Patch ReadyReplicas after a short delay so waitReady inside
	// EnsureRunning returns success.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = c.AppsV1().Deployments(ns).UpdateStatus(
			context.Background(),
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "notebook-" + wsID, Namespace: ns},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
			},
			metav1.UpdateOptions{},
		)
	}()

	return sup
}

func TestPostNotebookSession_NoSecret_503(t *testing.T) {
	s := &Server{} // NotebookJWTSecret is nil
	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/ws_a/session", nil)
	req = withChiURLParam(req, "ws", "ws_a")
	rr := httptest.NewRecorder()
	s.postNotebookSession(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPostNotebookSession_NoSupervisor_503(t *testing.T) {
	s := &Server{NotebookJWTSecret: []byte("secret-secret-secret-secret-32!!")}
	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/ws_a/session", nil)
	req = withChiURLParam(req, "ws", "ws_a")
	rr := httptest.NewRecorder()
	s.postNotebookSession(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPostNotebookSession_NoUser_401(t *testing.T) {
	s := &Server{
		NotebookJWTSecret:  []byte("secret-secret-secret-secret-32!!"),
		NotebookSupervisor: newNotebookTestSupervisor(t, "ws_a", "ns_a"),
	}
	// No auth.ContextWithUserID — UserIDFromContext returns "".
	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/ws_a/session", nil)
	req = withChiURLParam(req, "ws", "ws_a")
	rr := httptest.NewRecorder()
	s.postNotebookSession(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// --- Integration: needs a real DB so IsWorkspaceMember works. ---

func TestPostNotebookSession_NotMember_403(t *testing.T) {
	d := newCodexTestDBForServer(t)
	seedWorkspaceMember(t, d, "ws_nb_a", "u_other", "owner")
	// Also create the calling user but DON'T add membership.
	if _, err := d.Exec(`INSERT INTO users (id, email) VALUES ($1, $2) ON CONFLICT DO NOTHING`, "u_outsider", "u_outsider@test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	s := &Server{
		DB:                 d,
		NotebookJWTSecret:  []byte("secret-secret-secret-secret-32!!"),
		NotebookSupervisor: newNotebookTestSupervisor(t, "ws_nb_a", "ns-a"),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/ws_nb_a/session", nil).
		WithContext(auth.ContextWithUserID(context.Background(), "u_outsider"))
	req = withChiURLParam(req, "ws", "ws_nb_a")
	rr := httptest.NewRecorder()
	s.postNotebookSession(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPostNotebookSession_HappyPath(t *testing.T) {
	d := newCodexTestDBForServer(t)
	wsID := "ws_nb_happy"
	ns := "ns-nb-happy"
	seedWorkspaceMember(t, d, wsID, "u_member", "developer")
	if err := d.SetWorkspaceNamespace(wsID, ns); err != nil {
		t.Fatalf("set namespace: %v", err)
	}

	secret := []byte("secret-secret-secret-secret-32!!")
	s := &Server{
		DB:                 d,
		NotebookJWTSecret:  secret,
		NotebookSupervisor: newNotebookTestSupervisor(t, wsID, ns),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/notebooks/"+wsID+"/session", nil).
		WithContext(auth.ContextWithUserID(context.Background(), "u_member"))
	req = withChiURLParam(req, "ws", wsID)
	rr := httptest.NewRecorder()
	s.postNotebookSession(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		URL       string `json:"url"`
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
	}
	if want := "/api/notebooks/" + wsID + "/lab"; resp.URL != want {
		t.Errorf("url = %q want %q", resp.URL, want)
	}
	if resp.Token == "" {
		t.Error("token empty")
	}
	// expires_at within (now, now+notebookSessionTTL+slack].
	now := time.Now().Unix()
	if resp.ExpiresAt < now+int64(9*time.Minute/time.Second) || resp.ExpiresAt > now+int64(11*time.Minute/time.Second) {
		t.Errorf("expires_at = %d, now=%d (want ~+10m)", resp.ExpiresAt, now)
	}
	// Token must verify with the configured secret and carry the
	// expected claims.
	claims, err := notebookjwt.Verify(secret, resp.Token)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if claims.UserID != "u_member" || claims.WorkspaceID != wsID {
		t.Errorf("claims = %+v", claims)
	}
}

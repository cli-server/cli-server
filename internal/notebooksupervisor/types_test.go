package notebooksupervisor

import (
	"strings"
	"testing"
	"time"
)

func TestKey_DeploymentName(t *testing.T) {
	k := Key{WorkspaceID: "ws-abc-123", Namespace: "agentserver-ws-abc-123"}
	if got := k.DeploymentName(); got != "notebook-ws-abc-123" {
		t.Fatalf("got %q", got)
	}
}

func TestKey_DeploymentName_TooLong(t *testing.T) {
	k := Key{WorkspaceID: strings.Repeat("x", 100), Namespace: "ns"}
	if _, err := k.SafeDeploymentName(); err == nil {
		t.Fatal("expected error for too-long name")
	}
}

func TestKey_DeploymentName_InvalidChars(t *testing.T) {
	k := Key{WorkspaceID: "Has_Caps_AndUnderscores", Namespace: "ns"}
	if _, err := k.SafeDeploymentName(); err == nil {
		t.Fatal("expected error for invalid chars")
	}
}

func TestConfig_Defaults(t *testing.T) {
	c := Config{}.WithDefaults()
	if c.ReadyTimeout != 60*time.Second {
		t.Errorf("ready timeout = %v", c.ReadyTimeout)
	}
	if c.IdleTTL != 4*time.Hour {
		t.Errorf("idle ttl = %v", c.IdleTTL)
	}
	if c.ReapInterval != 5*time.Minute {
		t.Errorf("reap interval = %v", c.ReapInterval)
	}
	if c.Image == "" {
		t.Error("image must have a default")
	}
}

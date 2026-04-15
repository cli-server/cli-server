package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/agentserver/agentserver/pkg/agentsdk"
)

func main() {
	serverURL := os.Getenv("AGENTSERVER_URL")
	if serverURL == "" {
		serverURL = "https://agent.example.com"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := agentsdk.NewClient(agentsdk.Config{
		ServerURL: serverURL,
		Name:      "Example Agent",
	})

	// Login
	deviceResp, err := agentsdk.RequestDeviceCode(ctx, serverURL)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nVisit: %s\n\n", deviceResp.VerificationURIComplete)

	tokenResp, err := agentsdk.PollForToken(ctx, serverURL, deviceResp)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Authenticated!")

	// Register
	reg, err := client.Register(ctx, tokenResp.AccessToken)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Registered: sandbox=%s short_id=%s\n", reg.SandboxID, reg.ShortID)

	// Connect and serve
	err = client.Connect(ctx, agentsdk.Handlers{
		HTTP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<h1>Hello from Example Agent!</h1><p>Path: %s</p>", r.URL.Path)
		}),
		Task: func(ctx context.Context, task *agentsdk.Task) error {
			log.Printf("Task: %s - %s", task.Skill, task.Prompt)
			return task.Complete(ctx, agentsdk.TaskResult{Output: "done"})
		},
		OnConnect:    func() { log.Println("Connected") },
		OnDisconnect: func(err error) { log.Printf("Disconnected: %v", err) },
	})
	if err != nil {
		log.Fatal(err)
	}
}

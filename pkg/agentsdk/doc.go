// Package agentsdk provides a Go SDK for building custom agents that connect
// to agentserver via WebSocket tunnel.
//
// Quick start:
//
//	package main
//
//	import (
//		"context"
//		"fmt"
//		"log"
//		"net/http"
//
//		"github.com/agentserver/agentserver/pkg/agentsdk"
//	)
//
//	func main() {
//		ctx := context.Background()
//		serverURL := "https://agent.example.com"
//
//		// 1. Authenticate via OAuth Device Flow.
//		deviceResp, err := agentsdk.RequestDeviceCode(ctx, serverURL)
//		if err != nil {
//			log.Fatal(err)
//		}
//		fmt.Printf("Visit: %s\n", deviceResp.VerificationURIComplete)
//
//		token, err := agentsdk.PollForToken(ctx, serverURL, deviceResp)
//		if err != nil {
//			log.Fatal(err)
//		}
//
//		// 2. Create client and register.
//		client := agentsdk.NewClient(agentsdk.Config{
//			ServerURL: serverURL,
//			Name:      "my-agent",
//			Type:      "custom",
//		})
//
//		reg, err := client.Register(ctx, token.AccessToken)
//		if err != nil {
//			log.Fatal(err)
//		}
//		fmt.Printf("Registered sandbox: %s\n", reg.SandboxID)
//
//		// 3. Connect with handlers.
//		err = client.Connect(ctx, agentsdk.Handlers{
//			HTTP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//				fmt.Fprintf(w, "Hello from custom agent!")
//			}),
//			Task: func(task *agentsdk.Task) error {
//				result := agentsdk.TaskResult{Output: "done"}
//				return task.Complete(ctx, result)
//			},
//			OnConnect: func() {
//				log.Println("Connected!")
//			},
//			OnDisconnect: func(err error) {
//				log.Printf("Disconnected: %v", err)
//			},
//		})
//		if err != nil {
//			log.Fatal(err)
//		}
//	}
package agentsdk

package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/container"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/namespace"
	"github.com/agentserver/agentserver/internal/process"
	"github.com/agentserver/agentserver/internal/sandbox"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/server"
	"github.com/agentserver/agentserver/internal/storage"
	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/agentserver/agentserver/opencodeweb"
	"github.com/agentserver/agentserver/web"
	"github.com/spf13/cobra"
)

var (
	port       int
	agentImage string
	backend    string
	dbURL      string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the agentserver HTTP server",
	Long:  `Start the web server that provides a browser-based interface to opencode.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Resolve DB URL from flag or env.
		if dbURL == "" {
			dbURL = os.Getenv("DATABASE_URL")
		}
		if dbURL == "" {
			log.Fatal("--db-url or DATABASE_URL is required")
		}

		// Connect to PostgreSQL.
		database, err := db.Open(dbURL)
		if err != nil {
			log.Fatalf("Database connection failed: %v", err)
		}
		defer database.Close()
		log.Println("Connected to PostgreSQL")

		var staticFS fs.FS
		distFS, err := fs.Sub(web.StaticFS, "dist")
		if err != nil {
			log.Printf("Warning: embedded static files not available: %v", err)
		} else {
			staticFS = distFS
		}

		var opencodeStaticFS fs.FS
		ocDistFS, err := fs.Sub(opencodeweb.StaticFS, "dist")
		if err != nil {
			log.Printf("Warning: embedded opencode static files not available: %v", err)
		} else {
			opencodeStaticFS = ocDistFS
		}

		var procMgr process.Manager
		var driveMgr storage.DriveManager
		var nsMgr *namespace.Manager

		// Load known sandbox/container names from DB to avoid cleaning paused sandboxes.
		knownNames, err := database.ListAllActiveSandboxNames()
		if err != nil {
			log.Printf("Warning: failed to load known sandbox names: %v", err)
		}

		switch backend {
		case "docker":
			cfg := container.DefaultConfig()
			if agentImage != "" {
				cfg.Image = agentImage
			}
			mgr, err := container.NewManager(cfg)
			if err != nil {
				log.Fatalf("Docker backend unavailable: %v", err)
			}
			mgr.CleanOrphans(knownNames)
			log.Printf("Using Docker backend (image: %s)", cfg.Image)
			procMgr = mgr
			driveMgr = storage.NewDockerDriveAdapter(storage.NewDockerWorkspaceDriveManager(database))

		case "k8s":
			cfg := sandbox.DefaultConfig()
			if agentImage != "" {
				cfg.Image = agentImage
			}
			mgr, err := sandbox.NewManager(cfg, database)
			if err != nil {
				log.Fatalf("K8s backend unavailable: %v", err)
			}

			// Set up namespace manager for per-workspace namespace isolation.
			nsPrefix := envOrDefault("SANDBOX_NAMESPACE_PREFIX", "agent-ws")
			npEnabled := os.Getenv("NETWORKPOLICY_ENABLED") == "true"
			npDenyCIDRs := namespace.ParseDenyCIDRs(os.Getenv("NETWORKPOLICY_DENY_CIDRS"))

			restCfg, err := buildRESTConfig()
			if err != nil {
				log.Fatalf("K8s config for namespace manager: %v", err)
			}
			nsClientset, err := kubernetes.NewForConfig(restCfg)
			if err != nil {
				log.Fatalf("K8s clientset for namespace manager: %v", err)
			}
			nsMgr = namespace.NewManager(nsClientset, namespace.Config{
				Prefix: nsPrefix,
				NetworkPolicy: namespace.NetworkPolicyConfig{
					Enabled:            npEnabled,
					DenyCIDRs:          npDenyCIDRs,
					AgentserverNamespace: os.Getenv("AGENTSERVER_NAMESPACE"),
				},
			})

			// Backfill k8s_namespace for existing workspaces that don't have one.
			existingWs, err := database.ListWorkspacesWithoutNamespace()
			if err != nil {
				log.Printf("Warning: failed to list workspaces without namespace: %v", err)
			} else {
				for _, ws := range existingWs {
					ns, err := nsMgr.EnsureNamespace(context.Background(), ws.ID)
					if err != nil {
						log.Printf("Warning: failed to create namespace for workspace %s: %v", ws.ID, err)
						continue
					}
					if err := database.SetWorkspaceNamespace(ws.ID, ns); err != nil {
						log.Printf("Warning: failed to set namespace for workspace %s: %v", ws.ID, err)
					} else {
						log.Printf("Backfilled namespace %s for workspace %s", ns, ws.ID)
					}
				}
			}

			// Clean orphan sandboxes across all workspace namespaces.
			allNamespaces, err := database.GetAllWorkspaceNamespaces()
			if err != nil {
				log.Printf("Warning: failed to get workspace namespaces: %v", err)
			}
			mgr.CleanOrphans(knownNames, allNamespaces)
			log.Printf("Using K8s sandbox backend (namespace prefix: %s, agentserver ns: %s, image: %s)", nsPrefix, cfg.AgentserverNamespace, cfg.Image)
			procMgr = mgr

			workspaceDriveSize := parseEnvBytes("USER_DRIVE_SIZE", 10*1024*1024*1024)
			storageClass := os.Getenv("STORAGE_CLASS")
			workspaceDriveStorageClass := os.Getenv("USER_DRIVE_STORAGE_CLASS")
			if workspaceDriveStorageClass == "" {
				workspaceDriveStorageClass = storageClass
			}
			driveMgr = createK8sDriveManager(database, workspaceDriveSize, workspaceDriveStorageClass)

		default:
			log.Fatalf("Unknown backend: %s (supported: docker, k8s)", backend)
		}

		// Create auth and sandbox store.
		authSvc := auth.New(database)
		sandboxStore := sbxstore.NewStore(database)

		// Initialize OIDC if configured.
		var oidcMgr *auth.OIDCManager
		oidcBaseURL := os.Getenv("OIDC_REDIRECT_BASE_URL")

		ghClientID := os.Getenv("GITHUB_CLIENT_ID")
		ghClientSecret := os.Getenv("GITHUB_CLIENT_SECRET")
		oidcIssuer := os.Getenv("OIDC_ISSUER_URL")
		oidcClientID := os.Getenv("OIDC_CLIENT_ID")
		oidcClientSecret := os.Getenv("OIDC_CLIENT_SECRET")

		if ghClientID != "" || oidcIssuer != "" {
			if oidcBaseURL == "" {
				log.Fatal("OIDC_REDIRECT_BASE_URL is required when OIDC providers are configured")
			}
			oidcMgr = auth.NewOIDCManager(oidcBaseURL, authSvc)

			if ghClientID != "" && ghClientSecret != "" {
				ghRedirect := oidcBaseURL + "/api/auth/oidc/github/callback"
				oidcMgr.RegisterProvider(auth.NewGitHubProvider(ghClientID, ghClientSecret, ghRedirect))
				log.Println("OIDC: GitHub provider registered")
			}

			if oidcIssuer != "" && oidcClientID != "" && oidcClientSecret != "" {
				genericRedirect := oidcBaseURL + "/api/auth/oidc/oidc/callback"
				genericProvider, err := auth.NewGenericOIDCProvider(context.Background(), oidcIssuer, oidcClientID, oidcClientSecret, genericRedirect)
				if err != nil {
					log.Fatalf("Failed to initialize generic OIDC provider: %v", err)
				}
				oidcMgr.RegisterProvider(genericProvider)
				log.Println("OIDC: Generic provider registered")
			}
		}

		srv := server.New(authSvc, oidcMgr, database, sandboxStore, procMgr, driveMgr, nsMgr, tunnel.NewRegistry(), staticFS, opencodeStaticFS, !strings.EqualFold(os.Getenv("PASSWORD_AUTH_ENABLED"), "false"))
		addr := fmt.Sprintf(":%d", port)

		// Start idle watcher with a dynamic timeout getter that reads from the settings chain.
		idleWatcher := sbxstore.NewIdleWatcher(database, procMgr, sandboxStore, func() time.Duration {
			return srv.GetEffectiveIdleTimeout()
		})
		idleWatcher.Start()
		log.Printf("Idle watcher started (effective timeout: %s)", srv.GetEffectiveIdleTimeout())

		httpServer := &http.Server{Addr: addr, Handler: srv.Router()}

		// Graceful shutdown on SIGTERM/SIGINT
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			sig := <-sigCh
			log.Printf("Received %v, shutting down...", sig)
			httpServer.Shutdown(context.Background())
			idleWatcher.Stop()
			log.Println("Cleaning up active sandboxes...")
			procMgr.Close()
		}()

		log.Printf("Starting agentserver on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	},
}

func createK8sDriveManager(database *db.DB, storageSize int64, storageClassName string) storage.DriveManager {
	restCfg, err := buildRESTConfig()
	if err != nil {
		log.Printf("Warning: K8s workspace drive manager unavailable: %v", err)
		return storage.NilDriveManager{}
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Printf("Warning: K8s workspace drive manager unavailable: %v", err)
		return storage.NilDriveManager{}
	}
	mgr := storage.NewWorkspaceDriveManager(database, clientset, storageSize, storageClassName)
	return storage.NewK8sDriveAdapter(mgr)
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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseEnvBytes parses an env var as bytes (int64).
// Tries plain integer first, then falls back to K8s memory format (e.g. "10Gi").
func parseEnvBytes(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	// Try plain integer first.
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	// Fall back to K8s memory format.
	return parseK8sMemoryBytes(v, fallback)
}

// parseK8sMemoryBytes converts a K8s memory string to bytes.
func parseK8sMemoryBytes(s string, fallback int64) int64 {
	multiplier := int64(1)
	numStr := s
	switch {
	case strings.HasSuffix(s, "Gi"):
		multiplier = 1024 * 1024 * 1024
		numStr = strings.TrimSuffix(s, "Gi")
	case strings.HasSuffix(s, "Mi"):
		multiplier = 1024 * 1024
		numStr = strings.TrimSuffix(s, "Mi")
	case strings.HasSuffix(s, "Ki"):
		multiplier = 1024
		numStr = strings.TrimSuffix(s, "Ki")
	case strings.HasSuffix(s, "G"):
		multiplier = 1000 * 1000 * 1000
		numStr = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		multiplier = 1000 * 1000
		numStr = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		multiplier = 1000
		numStr = strings.TrimSuffix(s, "K")
	}
	f, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return fallback
	}
	return int64(f * float64(multiplier))
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to listen on")
	serveCmd.Flags().StringVar(&agentImage, "agent-image", "", "Container image for agent sessions")
	serveCmd.Flags().StringVar(&backend, "backend", "docker", "Session backend: docker or k8s")
	serveCmd.Flags().StringVar(&dbURL, "db-url", "", "PostgreSQL connection URL (or use DATABASE_URL env)")
}

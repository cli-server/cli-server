package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/imryao/cli-server/internal/auth"
	"github.com/imryao/cli-server/internal/container"
	"github.com/imryao/cli-server/internal/db"
	"github.com/imryao/cli-server/internal/namespace"
	"github.com/imryao/cli-server/internal/process"
	"github.com/imryao/cli-server/internal/sandbox"
	"github.com/imryao/cli-server/internal/sbxstore"
	"github.com/imryao/cli-server/internal/server"
	"github.com/imryao/cli-server/internal/storage"
	"github.com/imryao/cli-server/internal/tunnel"
	"github.com/imryao/cli-server/opencodeweb"
	"github.com/imryao/cli-server/web"
	"github.com/spf13/cobra"
)

var (
	port        int
	agentImage  string
	backend     string
	dbURL       string
	idleTimeout time.Duration
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the cli-server HTTP server",
	Long:  `Start the web server that provides a browser-based interface to opencode.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Resolve DB URL from flag or env.
		if dbURL == "" {
			dbURL = os.Getenv("DATABASE_URL")
		}
		if dbURL == "" {
			log.Fatal("--db-url or DATABASE_URL is required")
		}

		// Resolve idle timeout from env if not set via flag.
		if !cmd.Flags().Changed("idle-timeout") {
			if envTimeout := os.Getenv("IDLE_TIMEOUT"); envTimeout != "" {
				if d, err := time.ParseDuration(envTimeout); err == nil {
					idleTimeout = d
				}
			}
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
			nsPrefix := envOrDefault("SANDBOX_NAMESPACE_PREFIX", "cli-ws")
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
					CliServerNamespace: os.Getenv("CLI_SERVER_NAMESPACE"),
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
			log.Printf("Using K8s sandbox backend (namespace prefix: %s, cli-server ns: %s, image: %s)", nsPrefix, cfg.CliServerNamespace, cfg.Image)
			procMgr = mgr

			workspaceDriveSize := envOrDefault("USER_DRIVE_SIZE", "10Gi")
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

		srv := server.New(authSvc, oidcMgr, database, sandboxStore, procMgr, driveMgr, nsMgr, tunnel.NewRegistry(), staticFS, opencodeStaticFS)
		addr := fmt.Sprintf(":%d", port)

		// Start idle watcher.
		var idleWatcher *sbxstore.IdleWatcher
		if idleTimeout > 0 {
			idleWatcher = sbxstore.NewIdleWatcher(database, procMgr, sandboxStore, idleTimeout)
			idleWatcher.Start()
			log.Printf("Idle watcher started (timeout: %s)", idleTimeout)
		}

		httpServer := &http.Server{Addr: addr, Handler: srv.Router()}

		// Graceful shutdown on SIGTERM/SIGINT
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			sig := <-sigCh
			log.Printf("Received %v, shutting down...", sig)
			httpServer.Shutdown(context.Background())
			if idleWatcher != nil {
				idleWatcher.Stop()
			}
			log.Println("Cleaning up active sandboxes...")
			procMgr.Close()
		}()

		log.Printf("Starting cli-server on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	},
}

func createK8sDriveManager(database *db.DB, storageSize, storageClassName string) storage.DriveManager {
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

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to listen on")
	serveCmd.Flags().StringVar(&agentImage, "agent-image", "", "Container image for agent sessions")
	serveCmd.Flags().StringVar(&backend, "backend", "docker", "Session backend: docker or k8s")
	serveCmd.Flags().StringVar(&dbURL, "db-url", "", "PostgreSQL connection URL (or use DATABASE_URL env)")
	serveCmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 30*time.Minute, "Idle session auto-pause timeout (0 to disable)")
}

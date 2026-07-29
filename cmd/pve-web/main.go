package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/local/pve-web/internal/cache"
	"github.com/local/pve-web/internal/config"
	"github.com/local/pve-web/internal/credentials"
	"github.com/local/pve-web/internal/httpapi"
	"github.com/local/pve-web/internal/proxmox"
	runtimestate "github.com/local/pve-web/internal/runtime"
	"github.com/local/pve-web/internal/service"
	"github.com/local/pve-web/internal/tasks"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "dev"
var commit = "unknown"
var buildTime = "unknown"

func main() {
	configPath := flag.String("config", "pve-web.yaml", "configuration path")
	credentialsPath := flag.String("credentials", "pve-web-credentials.json", "credential path")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if logFile := setupLogging(cfg); logFile != nil {
		defer logFile.Close()
	}
	if _, statErr := os.Stat(*credentialsPath); os.IsNotExist(statErr) {
		portable := filepath.Join(filepath.Dir(*credentialsPath), "pve-web-credential.json")
		if _, portableErr := os.Stat(portable); portableErr == nil {
			if len(cfg.Targets) == 0 {
				bundle, bundleErr := credentials.LoadBundle(portable)
				if bundleErr != nil {
					log.Fatal(bundleErr)
				}
				for _, entry := range bundle.Targets {
					cfg.Targets = append(cfg.Targets, config.Target{ID: entry.ID, Name: entry.Name, Type: entry.Type, Enabled: entry.Enabled, VerifyTLS: entry.VerifyTLS, DetectHA: entry.DetectHA, DetectCeph: entry.DetectCeph, Endpoints: entry.Endpoints})
				}
			}
			data, readErr := os.ReadFile(portable)
			if readErr != nil {
				log.Fatal(readErr)
			}
			if writeErr := os.WriteFile(*credentialsPath, data, 0600); writeErr != nil {
				log.Fatal(writeErr)
			}
			log.Printf("imported portable credential bundle into %s", *credentialsPath)
		}
	}
	creds, err := credentials.Load(*credentialsPath)
	if err != nil {
		log.Fatal(err)
	}
	if bundle, bundleErr := credentials.LoadBundle(*credentialsPath); bundleErr == nil && targetsNeedBundle(cfg.Targets, creds) {
		cfg.Targets = make([]config.Target, 0, len(bundle.Targets))
		for _, entry := range bundle.Targets {
			cfg.Targets = append(cfg.Targets, config.Target{ID: entry.ID, Name: entry.Name, Type: entry.Type, Enabled: entry.Enabled, VerifyTLS: entry.VerifyTLS, DetectHA: entry.DetectHA, DetectCeph: entry.DetectCeph, Endpoints: entry.Endpoints})
		}
		log.Printf("using target definitions from credential bundle")
	}
	clients := map[string]*proxmox.Client{}
	for _, t := range cfg.Targets {
		c, ok := creds[t.ID]
		if !ok {
			continue
		}
		clients[t.ID] = proxmox.NewClientWithConsole(proxmox.Target{ID: t.ID, Endpoints: t.Endpoints, VerifyTLS: t.VerifyTLS}, c.User, c.TokenName, c.TokenValue, c.ConsoleUser, c.ConsolePassword)
	}
	runtimeState := runtimestate.New(cfg, creds, clients)
	store := cache.New()
	jobs := tasks.New()
	httpapi.Version = httpVersion()
	httpapi.Commit = commit
	httpapi.BuildTime = buildTime
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go service.NewRefresher(runtimeState, store).Run(ctx)
	server := &http.Server{Addr: cfg.Server.Listen, Handler: httpapi.New(runtimeState, store, jobs, *configPath, *credentialsPath).Handler()}
	log.Printf("pve-web %s listening on %s", httpVersion(), cfg.Server.Listen)
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			log.Printf("HTTP shutdown failed: %v", shutdownErr)
		}
	}()
	if cfg.Server.HTTPS.Enabled {
		err = server.ListenAndServeTLS(cfg.Server.HTTPS.CertFile, cfg.Server.HTTPS.KeyFile)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func targetsNeedBundle(targets []config.Target, creds map[string]credentials.Credential) bool {
	if len(targets) == 0 {
		return true
	}
	for _, target := range targets {
		if _, ok := creds[target.ID]; !ok {
			return true
		}
	}
	return false
}

func httpVersion() string {
	if version != "" {
		return version
	}
	return filepath.Base(os.Args[0])
}

// setupLogging makes the previously-unused config.Logging settings actually
// do something: when enabled, all log.Printf/log.Fatal output is written to
// the configured file instead of the process's default stderr. This gives
// every deployment platform (not just FreeBSD, where the rc.d script already
// redirects stdout/stderr via `daemon -o`) a working, predictable log file
// that whoever is diagnosing a problem can read, without needing to also
// have access to the OS-level service wrapper's output redirection.
//
// If the file cannot be opened (bad path, permission issue, missing parent
// directory), logging falls back to stderr and a warning is printed there;
// a broken log file must never prevent pve-web from starting.
func setupLogging(cfg config.Config) *os.File {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if !cfg.Logging.Enabled || cfg.Logging.File == "" {
		return nil
	}
	f, err := os.OpenFile(cfg.Logging.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		log.Printf("logging: unable to open log file %q, continuing with the default output: %v", cfg.Logging.File, err)
		return nil
	}
	log.SetOutput(f)
	log.Printf("logging: writing to %s", cfg.Logging.File)
	return f
}

var _ = fmt.Sprintf

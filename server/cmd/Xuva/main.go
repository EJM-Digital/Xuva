package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/EJM-Digital/Xuva/server/internal/app"
	"github.com/EJM-Digital/Xuva/server/internal/backup"
	"github.com/EJM-Digital/Xuva/server/internal/config"
	"github.com/EJM-Digital/Xuva/server/internal/firewall"
	xuvalogging "github.com/EJM-Digital/Xuva/server/internal/logging"
	"github.com/EJM-Digital/Xuva/server/internal/tlscert"
)

// portFromHTTPAddr extracts the TCP port from a Go ListenAndServe addr.
// Addrs come in three flavours: ":8097", "0.0.0.0:8097", "192.168.1.5:8097".
// Returns 0 on any parse failure (caller treats that as "skip firewall").
func portFromHTTPAddr(addr string) int {
	// Handle pure ":N" form first — net.SplitHostPort wants a non-empty host.
	if len(addr) > 0 && addr[0] == ':' {
		var port int
		if _, err := fmt.Sscanf(addr[1:], "%d", &port); err == nil {
			return port
		}
		return 0
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0
	}
	return port
}

// hostIsLoopback reports whether a ListenAndServe addr binds only to loopback.
// Treats the empty host (":8097") and the all-interfaces wildcards as NON-loopback,
// since those expose the listener to the LAN.
func hostIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Pure ":8097" form has no host → all interfaces → not loopback.
		host = strings.TrimSpace(addr)
		if strings.HasPrefix(host, ":") {
			return false
		}
	}
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// guardAuthDisabledBind refuses to start an auth-disabled server that is reachable
// from anything other than loopback — that combination serves the entire library,
// unauthenticated, to the local network. An explicit env override is provided for
// trusted isolated deployments (e.g. a private reverse proxy that adds its own auth).
func guardAuthDisabledBind(cfg config.Config) error {
	if !cfg.AuthDisabled {
		return nil
	}
	if allow, _ := strconv.ParseBool(os.Getenv("XUVA_AUTH_DISABLED_ALLOW_NONLOOPBACK")); allow {
		slog.Warn("authentication is DISABLED on a non-loopback bind — proceeding because XUVA_AUTH_DISABLED_ALLOW_NONLOOPBACK=true",
			"httpAddr", cfg.HTTPAddr)
		return nil
	}
	exposed := !hostIsLoopback(cfg.HTTPAddr)
	if cfg.TLSEnabled && !hostIsLoopback(cfg.HTTPSAddr) {
		exposed = true
	}
	if exposed {
		return fmt.Errorf("refusing to start: XUVA_AUTH_DISABLED is set but the server binds to a non-loopback address (%s) — this exposes the library unauthenticated to the network; bind to 127.0.0.1, enable auth, or set XUVA_AUTH_DISABLED_ALLOW_NONLOOPBACK=true to override", cfg.HTTPAddr)
	}
	return nil
}

func main() {
	// Windows service control verbs (install/uninstall/start/stop) run as a
	// normal console process and exit. No-op / returns false on non-Windows.
	if handleServiceControlCommand(os.Args) {
		return
	}

	// `xuva-server update` — the prompt-free MSI self-updater (registered as a
	// SYSTEM scheduled task by the installer). Runs as a console process and
	// exits; never reached under the SCM. No-op / returns false on non-Windows.
	if handleUpdateCommand(os.Args) {
		return
	}

	// When the Windows SCM launches us, default the runtime home to ProgramData
	// before config is read so state lands in C:\ProgramData\Xuva rather than
	// under Program Files. No-op on non-Windows and on non-service launches.
	if isWindowsService() {
		ensureServiceRuntimeDefaults()
	}

	cfg := config.FromEnv()
	logCloser, err := xuvalogging.Configure(xuvalogging.Config{
		Format:   cfg.LogFormat,
		Level:    cfg.LogLevel,
		Dir:      cfg.LogDir,
		MaxMB:    cfg.LogMaxMB,
		MaxFiles: cfg.LogMaxFiles,
	})
	if err != nil {
		slog.Warn("structured file logging disabled", "logDir", cfg.LogDir, "error", err)
	}
	defer logCloser.Close()
	if restored, err := backup.ApplyIfPending(cfg.DataDir); err != nil {
		slog.Error("backup restore failed", "error", err)
		os.Exit(1)
	} else if restored {
		slog.Info("backup restore applied — starting with restored database")
	}
	slog.Info("xuva server starting",
		"runtimeHome", cfg.RuntimeHome,
		"runtimeScope", cfg.RuntimeScope,
		"dataDir", cfg.DataDir,
		"logDir", cfg.LogDir,
		"logMaxMB", cfg.LogMaxMB,
		"logMaxFiles", cfg.LogMaxFiles,
		"httpAddr", cfg.HTTPAddr,
		"httpsEnabled", cfg.TLSEnabled,
		"httpsAddr", cfg.HTTPSAddr,
		"serverName", cfg.ServerName,
	)

	if err := guardAuthDisabledBind(cfg); err != nil {
		slog.Error("startup blocked", "error", err)
		os.Exit(1)
	}

	// Under the Windows SCM, hand control to the service dispatcher (it owns the
	// start/stop lifecycle). Otherwise run as a normal foreground process.
	if isWindowsService() {
		runService(cfg)
		return
	}
	runConsole(cfg)
}

// serverRuntime owns the started HTTP/HTTPS listeners and the application so the
// console path and the Windows service handler share one start/stop lifecycle.
type serverRuntime struct {
	application *app.Application
	httpServer  *http.Server
	httpsServer *http.Server
}

// startRuntime builds the application + listeners and starts serving in the
// background. It returns once the listeners are launched; callers wait for a
// stop signal (console) or an SCM stop request (service) and then call shutdown.
func startRuntime(cfg config.Config) (*serverRuntime, error) {
	application, err := app.New(context.Background(), cfg)
	if err != nil {
		return nil, err
	}

	router := application.Router()

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: router,
		// ReadHeaderTimeout bounds the Slowloris vector (slow header drip).
		// IdleTimeout reaps leaked keep-alive connections.
		// ReadTimeout and WriteTimeout are intentionally left unset: the server
		// streams large media bodies (a WriteTimeout would sever long playbacks)
		// and accepts variable-size uploads (a ReadTimeout would cut slow but
		// legitimate ones). Body size is bounded per-handler instead — JSON via
		// decodeJSON's MaxBytesReader, uploads via their own MaxBytesReader.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Ensure the host firewall lets other LAN devices reach us. On Windows
	// this is the single biggest reason "the server is fine on localhost but
	// my phone can't connect": Windows Defender Firewall blocks unsolicited
	// inbound TCP by default, even for processes listening on 0.0.0.0. On
	// non-Windows platforms this is a no-op (see firewall_other.go).
	//
	// Failure here is non-fatal — the warn log includes the exact manual
	// netsh command the user can run as Administrator.
	if port := portFromHTTPAddr(cfg.HTTPAddr); port > 0 {
		fwCtx, fwCancel := context.WithTimeout(context.Background(), 5*time.Second)
		firewall.LogResult(fwCtx, slog.Default(), port, "Xuva Server")
		// Also open inbound UDP 5353 so the LAN can discover us via mDNS
		// (the discovery package advertises _xuva._tcp via UDP multicast).
		// Without this, an OS-default-firewalled Windows box accepts HTTP
		// fine on TCP 8097 but is invisible to mobile/TV clients walking
		// the LAN looking for Xuva instances. WiX firewall extension was
		// dropped in #444; this is the server-side replacement (#452).
		firewall.LogResultUDP(fwCtx, slog.Default(), 5353, "Xuva Local Discovery (mDNS)")
		fwCancel()
	}

	// Bind the listener synchronously, before returning, so a bind failure
	// (port already in use, bad address) surfaces as a startRuntime error the
	// caller reports cleanly. This matters most under the Windows SCM: the old
	// goroutine path called os.Exit(1) on bind failure, which during startup
	// crashes the service (no clean start-failure status) and, if the bind died
	// after the Running signal, would crash-loop a service the SCM thinks is
	// healthy. With the listener already bound here, Serve only fails on a real
	// runtime fault, which we log and let the SCM/graceful-shutdown path handle.
	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		application.Shutdown(shutdownCtx)
		cancel()
		return nil, fmt.Errorf("bind http listener %s: %w", cfg.HTTPAddr, err)
	}

	go func() {
		slog.Info("xuva server listening", "addr", cfg.HTTPAddr)
		if err := httpServer.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped unexpectedly", "error", err)
		}
	}()

	// Optional HTTPS listener — enabled via XUVA_TLS_ENABLED=true or
	// tlsEnabled in settings.json. When no cert/key files are configured,
	// a self-signed certificate is auto-generated in <DataDir>/tls/ and its
	// SHA-256 fingerprint is logged for browser-trust verification.
	var httpsServer *http.Server
	if cfg.TLSEnabled {
		certFile := cfg.TLSCertFile
		keyFile := cfg.TLSKeyFile
		if certFile == "" {
			certFile = filepath.Join(cfg.DataDir, "tls", "cert.pem")
		}
		if keyFile == "" {
			keyFile = filepath.Join(cfg.DataDir, "tls", "key.pem")
		}

		// Collect SANs: the bind host (if a specific IP was given) plus hostname.
		var hosts []string
		if h, _, err := net.SplitHostPort(cfg.HTTPSAddr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
			hosts = append(hosts, h)
		}
		if hostname, err := os.Hostname(); err == nil {
			hosts = append(hosts, hostname)
		}

		tlsCert, fingerprint, err := tlscert.Ensure(certFile, keyFile, hosts)
		if err != nil {
			slog.Error("tls setup failed — HTTPS listener not started", "error", err)
		} else {
			if fingerprint != "" {
				slog.Info("tls certificate fingerprint (SHA-256)",
					"fingerprint", fingerprint,
					"certFile", certFile,
				)
			}
			httpsServer = &http.Server{
				Addr:    cfg.HTTPSAddr,
				Handler: router,
				// Same timeout rationale as the HTTP listener above.
				ReadHeaderTimeout: 5 * time.Second,
				IdleTimeout:       120 * time.Second,
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{tlsCert},
					MinVersion:   tls.VersionTLS12,
				},
			}
			if port := portFromHTTPAddr(cfg.HTTPSAddr); port > 0 {
				fwCtx, fwCancel := context.WithTimeout(context.Background(), 5*time.Second)
				firewall.LogResult(fwCtx, slog.Default(), port, "Xuva Server (HTTPS)")
				fwCancel()
			}
			// Pre-bind like the HTTP listener. HTTPS is optional, so a bind
			// failure here is non-fatal: log and continue without TLS rather
			// than failing the whole server (or crashing a goroutine).
			httpsListener, lerr := net.Listen("tcp", cfg.HTTPSAddr)
			if lerr != nil {
				slog.Error("https listener bind failed — HTTPS not started", "addr", cfg.HTTPSAddr, "error", lerr)
				httpsServer = nil
			} else {
				go func() {
					slog.Info("xuva server listening (https)", "addr", cfg.HTTPSAddr)
					if err := httpsServer.ServeTLS(httpsListener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
						slog.Error("https server stopped unexpectedly", "error", err)
					}
				}()
			}
		}
	}

	return &serverRuntime{
		application: application,
		httpServer:  httpServer,
		httpsServer: httpsServer,
	}, nil
}

// shutdown gracefully drains the listeners and the application. Safe to call once.
func (rt *serverRuntime) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt.application.Shutdown(ctx)
	if err := rt.httpServer.Shutdown(ctx); err != nil {
		slog.Error("server shutdown failed", "error", err)
	}
	if rt.httpsServer != nil {
		if err := rt.httpsServer.Shutdown(ctx); err != nil {
			slog.Error("https server shutdown failed", "error", err)
		}
	}
	slog.Info("xuva server stopped")
}

// runConsole runs the server as a normal foreground process, stopping on
// SIGINT/SIGTERM. This is the dev path and the path the desktop app uses when it
// spawns xuva-server.exe as a child process.
func runConsole(cfg config.Config) {
	rt, err := startRuntime(cfg)
	if err != nil {
		slog.Error("server startup failed", "error", err)
		os.Exit(1)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	rt.shutdown()
}

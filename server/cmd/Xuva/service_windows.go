//go:build windows

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/EJM-Digital/Xuva/server/internal/config"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName        = "XuvaMediaServer"
	serviceDisplayName = "Xuva Media Server"
	serviceDescription = "Xuva self-hosted media server (headless)."
	serviceUsage       = "usage: xuva-server service <install|uninstall|start|stop>"
)

// isWindowsService reports whether the Windows SCM launched this process (as
// opposed to a console/dev run or being spawned by the desktop app).
func isWindowsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

// ensureServiceRuntimeDefaults points the runtime home at C:\ProgramData\Xuva for
// SCM launches when the operator hasn't pinned it explicitly. A service runs as
// LocalSystem with no Program Files write expectation, so state belongs in
// ProgramData. The MSI will normally set XUVA_RUNTIME_HOME in the service
// environment; this is the safety net for a hand-installed service.
func ensureServiceRuntimeDefaults() {
	if os.Getenv("XUVA_RUNTIME_HOME") != "" || os.Getenv("XUVA_DATA_DIR") != "" {
		return
	}
	programData := os.Getenv("PROGRAMDATA")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	_ = os.Setenv("XUVA_RUNTIME_HOME", filepath.Join(programData, "Xuva"))
}

// xuvaService adapts the shared serverRuntime to the SCM control protocol.
type xuvaService struct{ cfg config.Config }

func (s *xuvaService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}
	rt, err := startRuntime(s.cfg)
	if err != nil {
		slog.Error("windows service: startup failed", "error", err)
		// Non-zero service-specific exit code so the SCM reports a failure.
		return false, 1
	}
	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	// Register (or, when opted out, remove) the SYSTEM auto-updater scheduled
	// task. Done from the running LocalSystem service — which has the rights to
	// manage a SYSTEM task — so it self-heals a missing task and applies an
	// XUVA_AUTO_UPDATE opt-out toggle on the next start. Best-effort: a failure
	// here must never stop the media server.
	enabled := autoUpdateEnabled()
	if err := ensureUpdaterTask(enabled); err != nil {
		slog.Warn("windows service: could not ensure updater task", "enabled", enabled, "error", err)
	} else {
		slog.Info("windows service: updater task ensured", "enabled", enabled)
	}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			rt.shutdown()
			return false, 0
		default:
			slog.Warn("windows service: unexpected control request", "cmd", c.Cmd)
		}
	}
	return false, 0
}

// runService dispatches to the SCM. Called only when isWindowsService() is true.
func runService(cfg config.Config) {
	if err := svc.Run(serviceName, &xuvaService{cfg: cfg}); err != nil {
		slog.Error("windows service: dispatcher failed", "error", err)
		os.Exit(1)
	}
}

// handleServiceControlCommand implements `xuva-server service <verb>`. These run
// as a normal (elevated) console process — not under the SCM — and exit when done.
// Returns true when a service verb was handled (so main should stop).
func handleServiceControlCommand(args []string) bool {
	if len(args) < 2 || args[1] != "service" {
		return false
	}
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, serviceUsage)
		os.Exit(2)
	}

	var err error
	switch args[2] {
	case "install":
		err = installService()
	case "uninstall":
		err = uninstallService()
	case "start":
		err = startServiceControl()
	case "stop":
		err = stopServiceControl()
	default:
		fmt.Fprintln(os.Stderr, serviceUsage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "xuva service %s: %v\n", args[2], err)
		os.Exit(1)
	}
	fmt.Printf("xuva service %s: ok\n", args[2])
	return true
}

func installService() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(serviceName); err == nil {
		existing.Close()
		return fmt.Errorf("service %q is already installed", serviceName)
	}

	s, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed: %w", serviceName, err)
	}
	defer s.Close()

	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

func startServiceControl() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed: %w", serviceName, err)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func stopServiceControl() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed: %w", serviceName, err)
	}
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	return nil
}

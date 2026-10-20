//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/EJM-Digital/Xuva/server/internal/updater"
)

// CREATE_NO_WINDOW keeps the detached applier (and the msiexec it launches) from
// flashing a console window. Harmless under a hidden SYSTEM scheduled task; nice
// for a manual `xuva-server update` from an interactive shell.
const createNoWindow = 0x08000000

func newUpdateApplier() updateApplier { return windowsApplier{} }

type windowsApplier struct{}

// apply re-execs a temp copy of this binary as `update --apply <msi>` and
// returns once that detached process has started. Running msiexec from the temp
// copy — not from C:\Program Files\Xuva\xuva-server.exe — is what lets the
// MajorUpgrade delete/replace the installed exe without hitting a file lock or
// deferring to a reboot. msiexec itself is hosted by the Windows Installer
// service, so it survives this process exiting; and it stops/replaces/restarts
// the XuvaMediaServer service (a different process) on its own.
func (windowsApplier) apply(_ context.Context, msiPath, version string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own path: %w", err)
	}
	tempCopy := filepath.Join(os.TempDir(), "xuva-update-"+updater.SafePathName(version)+".exe")
	if err := copyExecutable(self, tempCopy); err != nil {
		return fmt.Errorf("stage updater copy: %w", err)
	}

	cmd := exec.Command(tempCopy, "update", "--apply", msiPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch update applier: %w", err)
	}
	slog.Info("xuva update: launched detached applier", "applier", tempCopy, "msi", msiPath, "pid", cmd.Process.Pid)
	// Don't Wait — we want this (stage 1) process to exit promptly so the
	// installed exe is free by the time msiexec reaches RemoveExistingProducts.
	return nil
}

// runApplyStage is stage 2: run msiexec on the already-verified MSI and wait for
// it. Quiet (/qn) and /norestart so it's prompt-free; MajorUpgrade handles
// stop-old-service / install / start-new-service.
func runApplyStage(msiPath string) int {
	slog.Info("xuva update apply: running msiexec", "msi", msiPath)
	cmd := exec.Command("msiexec", "/i", msiPath, "/qn", "/norestart")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := cmd.Run(); err != nil {
		slog.Error("xuva update apply: msiexec failed", "msi", msiPath, "error", err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
	slog.Info("xuva update apply: msiexec succeeded", "msi", msiPath)
	// Best-effort cleanup of the downloaded MSI. The temp self-copy can't delete
	// itself while running; %TEMP% hygiene reclaims it later.
	_ = os.Remove(msiPath)
	return 0
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

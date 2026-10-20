//go:build !windows

package main

import "github.com/EJM-Digital/Xuva/server/internal/config"

// These are no-op stubs on non-Windows platforms so main.go can call the service
// hooks unconditionally. The Windows implementations live in service_windows.go.

// handleServiceControlCommand handles `xuva-server service <verb>` on Windows.
// On other platforms there is no Service Control Manager, so it never matches.
func handleServiceControlCommand(_ []string) bool { return false }

// isWindowsService reports whether the process was launched by the Windows SCM.
// Always false off Windows.
func isWindowsService() bool { return false }

// ensureServiceRuntimeDefaults defaults the runtime home for SCM launches. No-op
// off Windows.
func ensureServiceRuntimeDefaults() {}

// runService runs under the Windows SCM. Never reached off Windows (callers gate
// on isWindowsService); present only so main.go compiles cross-platform.
func runService(_ config.Config) {}

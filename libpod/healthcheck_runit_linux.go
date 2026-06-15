//go:build !remote && linux && !systemd && runit

package libpod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containers/podman/v5/pkg/errorhandling"
	"github.com/sirupsen/logrus"
)

const (
	runitHealthcheckDir       = "healthcheck-runit"
	runitHealthcheckStopFile  = "podman-stop"
	runitHealthcheckServiceEn = "PODMAN_RUNIT_HC_SERVICE"
)

// createTimer creates a runit service directory for healthchecks of a container.
func (c *Container) createTimer(interval string, isStartup bool) error {
	if c.disableHealthCheckRunit(interval, isStartup) {
		return nil
	}

	if _, err := exec.LookPath("runsv"); err != nil {
		return fmt.Errorf("unable to find runsv to add healthchecks: %w", err)
	}
	if _, err := exec.LookPath("sv"); err != nil {
		return fmt.Errorf("unable to find sv to add healthchecks: %w", err)
	}

	hcUnitName := c.hcUnitName(isStartup, false)
	serviceDir, err := c.runitHealthCheckServiceDir(hcUnitName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(serviceDir); err != nil {
		return fmt.Errorf("removing stale runit healthcheck service directory %q: %w", serviceDir, err)
	}
	if err := os.MkdirAll(serviceDir, 0o700); err != nil {
		return fmt.Errorf("creating runit healthcheck service directory %q: %w", serviceDir, err)
	}

	podman, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get path for podman for a health check service: %w", err)
	}

	cmd := []string{podman}
	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		cmd = append(cmd, "--log-level=debug", "--syslog")
	}
	cmd = append(cmd, "healthcheck", "run-loop", "--interval", interval, "--service-dir", serviceDir, c.ID())

	if err := os.WriteFile(filepath.Join(serviceDir, "run"), []byte(runitRunScript(cmd)), 0o700); err != nil {
		return fmt.Errorf("writing runit healthcheck run script: %w", err)
	}
	if err := os.WriteFile(filepath.Join(serviceDir, "finish"), []byte(runitFinishScript(serviceDir)), 0o700); err != nil {
		return fmt.Errorf("writing runit healthcheck finish script: %w", err)
	}
	// The service must not run until startTimer() is called after the container
	// has transitioned to running.
	if err := os.WriteFile(filepath.Join(serviceDir, "down"), nil, 0o600); err != nil {
		return fmt.Errorf("writing runit healthcheck down file: %w", err)
	}

	c.state.HCUnitName = hcUnitName
	if err := c.save(); err != nil {
		return fmt.Errorf("saving container %s healthcheck unit name: %w", c.ID(), err)
	}

	return nil
}

// startTimer starts a runit service for the healthchecks.
func (c *Container) startTimer(isStartup bool) error {
	hcUnitName := c.state.HCUnitName
	if hcUnitName == "" {
		hcUnitName = c.hcUnitName(isStartup, true)
	}

	serviceDir, err := c.runitHealthCheckServiceDir(hcUnitName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(serviceDir, "run")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("checking runit healthcheck service %q: %w", serviceDir, err)
	}

	if err := os.Remove(filepath.Join(serviceDir, runitHealthcheckStopFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing runit healthcheck stop marker: %w", err)
	}

	if !runitSupervisorActive(serviceDir) {
		cmd := exec.Command("runsv", serviceDir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to execute runsv: %w", err)
		}
		if err := cmd.Process.Release(); err != nil {
			return fmt.Errorf("releasing runsv process: %w", err)
		}
		if err := waitForRunitControl(serviceDir, 5*time.Second); err != nil {
			return err
		}
	}

	if err := os.Remove(filepath.Join(serviceDir, "down")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing runit healthcheck down file: %w", err)
	}
	cmd := exec.Command("sv", "-w", "5", "up", serviceDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("starting runit healthcheck service %q: %w: output: %s", serviceDir, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// removeTransientFiles removes the runit service directory for the container.
func (c *Container) removeTransientFiles(_ context.Context, isStartup bool, unitName string) error {
	if unitName == "" {
		unitName = c.hcUnitName(isStartup, true)
	}
	serviceDir, err := c.runitHealthCheckServiceDir(unitName)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(serviceDir, runitHealthcheckStopFile), nil, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("writing runit healthcheck stop marker: %w", err)
	}

	// When the run-loop removes its own startup service, do not wait for runsv
	// to observe process exit. The current command must return first.
	currentService := os.Getenv(runitHealthcheckServiceEn)
	if currentService == serviceDir {
		if runitSupervisorActive(serviceDir) {
			cmd := exec.Command("sv", "exit", serviceDir)
			if output, err := cmd.CombinedOutput(); err != nil {
				logrus.Debugf("exiting current runit healthcheck service %q: %v: output: %s", serviceDir, err, strings.TrimSpace(string(output)))
			}
		}
		return nil
	}

	stopErrors := []error{}
	if runitSupervisorActive(serviceDir) {
		cmd := exec.Command("sv", "-w", "5", "force-shutdown", serviceDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("stopping runit healthcheck service %q: %w: output: %s", serviceDir, err, strings.TrimSpace(string(output))))
		}
	}
	if err := os.RemoveAll(serviceDir); err != nil {
		stopErrors = append(stopErrors, fmt.Errorf("removing runit healthcheck service directory %q: %w", serviceDir, err))
	}
	return errorhandling.JoinErrors(stopErrors)
}

func (c *Container) disableHealthCheckRunit(interval string, isStartup bool) bool {
	if os.Getenv("DISABLE_HC_RUNIT") == "true" {
		return true
	}
	if isStartup {
		if c.config.StartupHealthCheckConfig == nil || c.config.StartupHealthCheckConfig.Interval == 0 {
			return true
		}
	} else if c.config.HealthCheckConfig == nil || c.config.HealthCheckConfig.Interval == 0 {
		return true
	}
	duration, err := time.ParseDuration(interval)
	return err != nil || duration <= 0
}

func (c *Container) runitHealthCheckServiceDir(unitName string) (string, error) {
	if c.state.RunDir == "" {
		return "", fmt.Errorf("cannot create runit healthcheck service without a container run directory")
	}
	return filepath.Join(c.state.RunDir, runitHealthcheckDir, unitName), nil
}

func runitRunScript(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return "#!/bin/sh\nexec 2>&1\nexec " + strings.Join(quoted, " ") + "\n"
}

func runitFinishScript(serviceDir string) string {
	return "#!/bin/sh\n" +
		"if [ -e ./" + runitHealthcheckStopFile + " ]; then\n" +
		"    cd /\n" +
		"    rm -rf " + shellQuote(serviceDir) + "\n" +
		"fi\n" +
		"exit 0\n"
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func runitSupervisorActive(serviceDir string) bool {
	info, err := os.Stat(filepath.Join(serviceDir, "supervise", "control"))
	return err == nil && info.Mode()&os.ModeNamedPipe != 0
}

func waitForRunitControl(serviceDir string, timeout time.Duration) error {
	control := filepath.Join(serviceDir, "supervise", "control")
	deadline := time.Now().Add(timeout)
	for {
		if info, err := os.Stat(control); err == nil && info.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for runit healthcheck service %q control pipe", serviceDir)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

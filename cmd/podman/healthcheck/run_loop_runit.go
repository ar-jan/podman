//go:build !remote && linux && !systemd && runit

package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"go.podman.io/podman/v6/cmd/podman/common"
	"go.podman.io/podman/v6/cmd/podman/registry"
	"go.podman.io/podman/v6/pkg/domain/entities"
)

const (
	runitHealthcheckStopFile  = "podman-stop"
	runitHealthcheckServiceEn = "PODMAN_RUNIT_HC_SERVICE"
)

var runLoopOptions struct {
	interval   string
	serviceDir string
}

var runLoopCmd = &cobra.Command{
	Use:               "run-loop [options] CONTAINER",
	Short:             "Run a container health check loop",
	Long:              "Run a container health check loop",
	RunE:              runLoop,
	Args:              cobra.ExactArgs(1),
	Hidden:            true,
	ValidArgsFunction: common.AutocompleteContainersRunning,
}

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: runLoopCmd,
		Parent:  healthCmd,
	})

	flags := runLoopCmd.Flags()
	flags.StringVar(&runLoopOptions.interval, "interval", "", "healthcheck interval")
	flags.StringVar(&runLoopOptions.serviceDir, "service-dir", "", "runit service directory")
	_ = flags.MarkHidden("interval")
	_ = flags.MarkHidden("service-dir")
}

func runLoop(_ *cobra.Command, args []string) error {
	interval, err := time.ParseDuration(runLoopOptions.interval)
	if err != nil {
		return fmt.Errorf("invalid healthcheck interval: %w", err)
	}
	if interval <= 0 {
		return nil
	}
	if runLoopOptions.serviceDir == "" {
		return errors.New("runit service directory must be set")
	}
	if err := os.Setenv(runitHealthcheckServiceEn, runLoopOptions.serviceDir); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTimer(0)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		if runLoopStopped(runLoopOptions.serviceDir) {
			return nil
		}
		if _, err := registry.ContainerEngine().HealthCheckRun(ctx, args[0], entities.HealthCheckOptions{}); err != nil {
			logrus.Errorf("Running healthcheck for container %s: %v", args[0], err)
		}
		if runLoopStopped(runLoopOptions.serviceDir) {
			return nil
		}
		ticker.Reset(interval)
	}
}

func runLoopStopped(serviceDir string) bool {
	_, err := os.Stat(filepath.Join(serviceDir, runitHealthcheckStopFile))
	return err == nil
}

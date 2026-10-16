package containermgr

import (
	"bytes"
	"context"
	"errors"
	"github.com/docker/docker/api/types/container"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/spf13/viper"
)

type Container struct {
	ID string
}

func FindByService(cli *client.Client, serviceID string, allowOff bool) (*Container, error) {
	containers, err := FindAllByService(cli, serviceID, allowOff)

	if err != nil {
		containerLogger().Debug("FindContainerByService: Error", "error", err.Error())
		return nil, err
	}

	containerLogger().Debug("FindRunningContainer: Found Containers", "containers", strconv.Itoa(len(containers)))

	if len(containers) < 1 {
		return nil, errors.New("no containers found")
	}

	return containers[0], nil

}

func FindAllByService(cli *client.Client, serviceID string, allowOff bool) (containers []*Container, err error) {
	ctx := context.Background()
	findArgs := filters.NewArgs(
		filters.Arg("label", "com.computestacks.service_id="+serviceID),
	)
	findOpts := container.ListOptions{
		All:     true,
		Filters: findArgs,
	}
	findContainer, err := cli.ContainerList(ctx, findOpts)

	if err != nil {
		return containers, err
	}

	skipContainer := false

	for _, c := range findContainer {
		containerLogger().Debug("Looking for container", "container", c.ID, "state", c.State)
		if allowOff {
			for k, v := range c.Labels {
				if k == "com.computestacks.role" {
					if v == "backup" || v == "bastion" {
						skipContainer = true
					}
				}
			}
			if !skipContainer {
				newContainer := Container{ID: c.ID}
				containers = append(containers, &newContainer)
			}
		} else {
			if c.State == "running" {
				for k, v := range c.Labels {
					if k == "com.computestacks.role" {
						if v == "backup" || v == "bastion" {
							skipContainer = true
						}
					}
				}
				if !skipContainer {
					newContainer := Container{ID: c.ID}
					containers = append(containers, &newContainer)
				}
			}
		}
		skipContainer = false
	}

	return containers, nil
}

// Helper to exec inside a container when you don't specifically know the container ID
func ServiceExec(serviceID string, jobCommands []string) (int, string, error) {
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		return 1, "", err
	}

	c, err := FindByService(cli, serviceID, false)

	if err != nil {
		return 1, "", err
	}
	return c.Exec(jobCommands)
}

func (c *Container) Stop() bool {
	if c == nil {
		return true
	}
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		containerLogger().Warn("Failed to connect to docker daemon", "error", err.Error(), "function", "ContainerStop")
		return false
	}
	ctx := context.Background()
	timeout := 15
	if containerStopError := cli.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &timeout}); containerStopError != nil {
		if strings.Contains(containerStopError.Error(), "no such container") {
			return true
		}
		containerLogger().Warn("Error stopping backup container", "error", containerStopError.Error(), "backupContainerID", c.ID)
		return false
	}

	// Ensure container actually stops.
	isStopped := false
	for counter := 1; counter < 12; counter++ {
		dockerContainer, errRunning := cli.ContainerInspect(ctx, c.ID)
		if errRunning != nil { // if container does not exist, an error is returned.
			isStopped = true
			break
		} else if !dockerContainer.State.Running {
			isStopped = true
			break
		}
		containerLogger().Debug("Waiting for container to stop", "container", dockerContainer.ID, "state", dockerContainer.State.Status)
		time.Sleep(500 * time.Millisecond)
	}
	return isStopped
}

func (c *Container) Start() bool {
	if c == nil {
		return true
	}
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		containerLogger().Warn("Failed to connect to docker daemon", "error", err.Error(), "function", "ContainerStart")
		return false
	}
	ctx := context.Background()
	if containerStartError := cli.ContainerStart(ctx, c.ID, container.StartOptions{}); containerStartError != nil {
		containerLogger().Warn("Error starting backup container", "error", containerStartError.Error(), "backupContainerID", c.ID)
		return false
	}

	// Ensure container actually stops.
	isStarted := false
	for counter := 1; counter < 12; counter++ {
		dockerContainer, errRunning := cli.ContainerInspect(ctx, c.ID)
		if errRunning == nil && dockerContainer.State.Running { // if container does not exist, an error is returned.
			isStarted = true
			break
		} else if errRunning == nil {
			containerLogger().Debug("Waiting for container to start", "container", dockerContainer.ID, "state", dockerContainer.State.Status)
		} else {
			containerLogger().Debug("Fatal error while trying to start container", "error", errRunning.Error())
			if counter > 2 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return isStarted
}

func (c *Container) Exec(jobCommands []string) (exitCode int, response string, err error) {
	return c.ExecEnv(jobCommands, nil)
}

// ExecEnv is Exec with extra environment variables (KEY=value) set for the command only.
// Use it to hand a secret to a command without putting it on the command line.
func (c *Container) ExecEnv(jobCommands []string, env []string) (exitCode int, response string, err error) {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		return 1, "", err
	}
	isReady := false

	for counter := 1; counter < 12; counter++ {
		dockerContainer, errRunning := cli.ContainerInspect(ctx, c.ID)
		if errRunning == nil && dockerContainer.State.Running {
			isReady = true
			break
		} else if errRunning == nil {
			containerLogger().Debug("Waiting for container before executing command", "container", dockerContainer.ID, "state", dockerContainer.State.Status)
		} else {
			containerLogger().Debug("Waiting for container before executing command", "error", errRunning.Error())
			// For fatal errors, reduce the total count to 3 instead of 12.
			if counter > 2 {
				break
			}
		}
		time.Sleep(time.Second)
	}

	if !isReady {
		return 1, "", errors.New("container never came online")
	}

	execConfig := container.ExecOptions{
		Cmd:          jobCommands,
		Env:          env,
		Tty:          true,
		AttachStderr: true,
		AttachStdout: true,
		AttachStdin:  true,
		Detach:       false,
	}
	execResponse, err := cli.ContainerExecCreate(ctx, c.ID, execConfig)

	if err != nil {
		return 1, "", err
	}

	execStartCheck := container.ExecStartOptions{
		Tty: execConfig.Tty,
	}

	resp, err := cli.ContainerExecAttach(ctx, execResponse.ID, execStartCheck)
	if err != nil {
		return 1, "", err
	}
	defer resp.Close()

	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Reader)
	s := buf.String()

	respStatus, err := cli.ContainerExecInspect(ctx, execResponse.ID)

	if err != nil {
		return 1, s, err
	}

	return respStatus.ExitCode, s, nil
}

// ExecStream runs cmd in the container and streams stdout to w. Unlike Exec, it
// does not buffer output in memory and does not allocate a TTY: a TTY merges
// stderr into stdout and would corrupt a binary stream such as a borg
// export-tar. stderr (borg --log-json diagnostics) is collected separately and
// returned for the caller to parse.
//
// The exit code is read only after the stream reaches EOF (the exec has
// finished); a still-running exec is treated as a failure so the caller never
// mistakes a truncated stream for success.
func (c *Container) ExecStream(ctx context.Context, cmd []string, stdout io.Writer) (exitCode int, stderr string, err error) {
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		return 1, "", err
	}

	isReady := false
	for counter := 1; counter < 12; counter++ {
		dockerContainer, errRunning := cli.ContainerInspect(ctx, c.ID)
		if errRunning == nil && dockerContainer.State.Running {
			isReady = true
			break
		} else if errRunning == nil {
			containerLogger().Debug("Waiting for container before executing command", "container", dockerContainer.ID, "state", dockerContainer.State.Status)
		} else {
			containerLogger().Debug("Waiting for container before executing command", "error", errRunning.Error())
			if counter > 2 {
				break
			}
		}
		time.Sleep(time.Second)
	}
	if !isReady {
		return 1, "", errors.New("container never came online")
	}

	execResponse, err := cli.ContainerExecCreate(ctx, c.ID, container.ExecOptions{
		Cmd:          cmd,
		Tty:          false,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 1, "", err
	}

	resp, err := cli.ContainerExecAttach(ctx, execResponse.ID, container.ExecStartOptions{Tty: false})
	if err != nil {
		return 1, "", err
	}
	defer resp.Close()

	// If ctx is cancelled/times out, close the hijacked connection so a StdCopy
	// blocked on a hung borg unblocks — cancelling ctx alone does not interrupt
	// the in-flight Read on the attached stream.
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		select {
		case <-ctx.Done():
			resp.Close()
		case <-watchdogDone:
		}
	}()

	// Demux the multiplexed stream: stdout (tar bytes) -> w, stderr -> buffer.
	var stderrBuf bytes.Buffer
	if _, copyErr := stdcopy.StdCopy(stdout, &stderrBuf, resp.Reader); copyErr != nil {
		if ctx.Err() != nil {
			return 1, stderrBuf.String(), ctx.Err()
		}
		return 1, stderrBuf.String(), copyErr
	}
	if ctx.Err() != nil {
		return 1, stderrBuf.String(), ctx.Err()
	}

	// The stream is fully drained, so the exec has finished: read the exit code.
	respStatus, inspectErr := cli.ContainerExecInspect(ctx, execResponse.ID)
	if inspectErr != nil {
		return 1, stderrBuf.String(), inspectErr
	}
	if respStatus.Running {
		return 1, stderrBuf.String(), errors.New("exec still running after stream EOF")
	}
	return respStatus.ExitCode, stderrBuf.String(), nil
}

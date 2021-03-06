// +build remoteclient

package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/containers/libpod/cmd/podman/cliconfig"
	"github.com/containers/libpod/cmd/podman/shared"
	iopodman "github.com/containers/libpod/cmd/podman/varlink"
	"github.com/containers/libpod/libpod"
	"github.com/containers/libpod/pkg/inspect"
	"github.com/containers/libpod/pkg/varlinkapi/virtwriter"
	"github.com/docker/docker/pkg/term"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/varlink/go/varlink"
	"golang.org/x/crypto/ssh/terminal"
	"k8s.io/client-go/tools/remotecommand"
)

// Inspect returns an inspect struct from varlink
func (c *Container) Inspect(size bool) (*inspect.ContainerInspectData, error) {
	reply, err := iopodman.ContainerInspectData().Call(c.Runtime.Conn, c.ID(), size)
	if err != nil {
		return nil, err
	}
	data := inspect.ContainerInspectData{}
	if err := json.Unmarshal([]byte(reply), &data); err != nil {
		return nil, err
	}
	return &data, err
}

// ID returns the ID of the container
func (c *Container) ID() string {
	return c.config.ID
}

// Pause a container
func (c *Container) Pause() error {
	_, err := iopodman.PauseContainer().Call(c.Runtime.Conn, c.ID())
	return err
}

// Unpause a container
func (c *Container) Unpause() error {
	_, err := iopodman.UnpauseContainer().Call(c.Runtime.Conn, c.ID())
	return err
}

// Config returns a container config
func (r *LocalRuntime) Config(name string) *libpod.ContainerConfig {
	// TODO the Spec being returned is not populated.  Matt and I could not figure out why.  Will defer
	// further looking into it for after devconf.
	// The libpod function for this has no errors so we are kind of in a tough
	// spot here.  Logging the errors for now.
	reply, err := iopodman.ContainerConfig().Call(r.Conn, name)
	if err != nil {
		logrus.Error("call to container.config failed")
	}
	data := libpod.ContainerConfig{}
	if err := json.Unmarshal([]byte(reply), &data); err != nil {
		logrus.Error("failed to unmarshal container inspect data")
	}
	return &data

}

// ContainerState returns the "state" of the container.
func (r *LocalRuntime) ContainerState(name string) (*libpod.ContainerState, error) { // no-lint
	reply, err := iopodman.ContainerStateData().Call(r.Conn, name)
	if err != nil {
		return nil, err
	}
	data := libpod.ContainerState{}
	if err := json.Unmarshal([]byte(reply), &data); err != nil {
		return nil, err
	}
	return &data, err

}

// Spec obtains the container spec.
func (r *LocalRuntime) Spec(name string) (*specs.Spec, error) {
	reply, err := iopodman.Spec().Call(r.Conn, name)
	if err != nil {
		return nil, err
	}
	data := specs.Spec{}
	if err := json.Unmarshal([]byte(reply), &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// LookupContainers is a wrapper for LookupContainer
func (r *LocalRuntime) LookupContainers(idsOrNames []string) ([]*Container, error) {
	var containers []*Container
	for _, name := range idsOrNames {
		ctr, err := r.LookupContainer(name)
		if err != nil {
			return nil, err
		}
		containers = append(containers, ctr)
	}
	return containers, nil
}

// LookupContainer gets basic information about container over a varlink
// connection and then translates it to a *Container
func (r *LocalRuntime) LookupContainer(idOrName string) (*Container, error) {
	state, err := r.ContainerState(idOrName)
	if err != nil {
		return nil, err
	}
	config := r.Config(idOrName)
	return &Container{
		remoteContainer{
			r,
			config,
			state,
		},
	}, nil
}

func (r *LocalRuntime) LookupContainersWithStatus(filters []string) ([]*Container, error) {
	var containers []*Container
	ctrs, err := iopodman.GetContainersByStatus().Call(r.Conn, filters)
	if err != nil {
		return nil, err
	}
	// This is not performance savy; if this turns out to be a problematic series of lookups, we need to
	// create a new endpoint to speed things up
	for _, ctr := range ctrs {
		container, err := r.LookupContainer(ctr.Id)
		if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func (r *LocalRuntime) GetLatestContainer() (*Container, error) {
	reply, err := iopodman.GetContainersByContext().Call(r.Conn, false, true, nil)
	if err != nil {
		return nil, err
	}
	if len(reply) > 0 {
		return r.LookupContainer(reply[0])
	}
	return nil, errors.New("no containers exist")
}

// GetArtifact returns a container's artifacts
func (c *Container) GetArtifact(name string) ([]byte, error) {
	var data []byte
	reply, err := iopodman.ContainerArtifacts().Call(c.Runtime.Conn, c.ID(), name)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(reply), &data); err != nil {
		return nil, err
	}
	return data, err
}

// Config returns a container's Config ... same as ctr.Config()
func (c *Container) Config() *libpod.ContainerConfig {
	if c.config != nil {
		return c.config
	}
	return c.Runtime.Config(c.ID())
}

// Name returns the name of the container
func (c *Container) Name() string {
	return c.config.Name
}

// StopContainers stops requested containers using varlink.
// Returns the list of stopped container ids, map of failed to stop container ids + errors, or any non-container error
func (r *LocalRuntime) StopContainers(ctx context.Context, cli *cliconfig.StopValues) ([]string, map[string]error, error) {
	var (
		ok       = []string{}
		failures = map[string]error{}
	)

	ids, err := iopodman.GetContainersByContext().Call(r.Conn, cli.All, cli.Latest, cli.InputArgs)
	if err != nil {
		return ok, failures, err
	}

	for _, id := range ids {
		stopped, err := iopodman.StopContainer().Call(r.Conn, id, int64(cli.Timeout))
		if err != nil {
			failures[id] = err
		} else {
			ok = append(ok, stopped)
		}
	}
	return ok, failures, nil
}

// KillContainers sends signal to container(s) based on varlink.
// Returns list of successful id(s), map of failed id(s) + error, or error not from container
func (r *LocalRuntime) KillContainers(ctx context.Context, cli *cliconfig.KillValues, signal syscall.Signal) ([]string, map[string]error, error) {
	var (
		ok       = []string{}
		failures = map[string]error{}
	)

	ids, err := iopodman.GetContainersByContext().Call(r.Conn, cli.All, cli.Latest, cli.InputArgs)
	if err != nil {
		return ok, failures, err
	}

	for _, id := range ids {
		killed, err := iopodman.KillContainer().Call(r.Conn, id, int64(signal))
		if err != nil {
			failures[id] = err
		} else {
			ok = append(ok, killed)
		}
	}
	return ok, failures, nil
}

// RemoveContainer removes container(s) based on varlink inputs.
func (r *LocalRuntime) RemoveContainers(ctx context.Context, cli *cliconfig.RmValues) ([]string, map[string]error, error) {
	ids, err := iopodman.GetContainersByContext().Call(r.Conn, cli.All, cli.Latest, cli.InputArgs)
	if err != nil {
		return nil, nil, err
	}

	var (
		ok       = []string{}
		failures = map[string]error{}
	)

	for _, id := range ids {
		_, err := iopodman.RemoveContainer().Call(r.Conn, id, cli.Force, cli.Volumes)
		if err != nil {
			failures[id] = err
		} else {
			ok = append(ok, id)
		}
	}
	return ok, failures, nil
}

// UmountRootFilesystems umounts container(s) root filesystems based on varlink inputs
func (r *LocalRuntime) UmountRootFilesystems(ctx context.Context, cli *cliconfig.UmountValues) ([]string, map[string]error, error) {
	ids, err := iopodman.GetContainersByContext().Call(r.Conn, cli.All, cli.Latest, cli.InputArgs)
	if err != nil {
		return nil, nil, err
	}

	var (
		ok       = []string{}
		failures = map[string]error{}
	)

	for _, id := range ids {
		err := iopodman.UnmountContainer().Call(r.Conn, id, cli.Force)
		if err != nil {
			failures[id] = err
		} else {
			ok = append(ok, id)
		}
	}
	return ok, failures, nil
}

// WaitOnContainers waits for all given container(s) to stop.
// interval is currently ignored.
func (r *LocalRuntime) WaitOnContainers(ctx context.Context, cli *cliconfig.WaitValues, interval time.Duration) ([]string, map[string]error, error) {
	var (
		ok       = []string{}
		failures = map[string]error{}
	)

	ids, err := iopodman.GetContainersByContext().Call(r.Conn, false, cli.Latest, cli.InputArgs)
	if err != nil {
		return ok, failures, err
	}

	for _, id := range ids {
		stopped, err := iopodman.WaitContainer().Call(r.Conn, id, int64(interval))
		if err != nil {
			failures[id] = err
		} else {
			ok = append(ok, strconv.FormatInt(stopped, 10))
		}
	}
	return ok, failures, nil
}

// BatchContainerOp is wrapper func to mimic shared's function with a similar name meant for libpod
func BatchContainerOp(ctr *Container, opts shared.PsOptions) (shared.BatchContainerStruct, error) {
	// TODO If pod ps ever shows container's sizes, re-enable this code; otherwise it isn't needed
	// and would be a perf hit
	// data, err := ctr.Inspect(true)
	// if err != nil {
	// 	return shared.BatchContainerStruct{}, err
	// }
	//
	// size := new(shared.ContainerSize)
	// size.RootFsSize = data.SizeRootFs
	// size.RwSize = data.SizeRw

	bcs := shared.BatchContainerStruct{
		ConConfig:   ctr.config,
		ConState:    ctr.state.State,
		ExitCode:    ctr.state.ExitCode,
		Pid:         ctr.state.PID,
		StartedTime: ctr.state.StartedTime,
		ExitedTime:  ctr.state.FinishedTime,
		// Size: size,
	}
	return bcs, nil
}

// Logs one or more containers over a varlink connection
func (r *LocalRuntime) Log(c *cliconfig.LogsValues, options *libpod.LogOptions) error {
	// GetContainersLogs
	reply, err := iopodman.GetContainersLogs().Send(r.Conn, uint64(varlink.More), c.InputArgs, c.Follow, c.Latest, options.Since.Format(time.RFC3339Nano), int64(c.Tail), c.Timestamps)
	if err != nil {
		return errors.Wrapf(err, "failed to get container logs")
	}
	if len(c.InputArgs) > 1 {
		options.Multi = true
	}
	for {
		log, flags, err := reply()
		if err != nil {
			return err
		}
		if log.Time == "" && log.Msg == "" {
			// We got a blank log line which can signal end of stream
			break
		}
		lTime, err := time.Parse(time.RFC3339Nano, log.Time)
		if err != nil {
			return errors.Wrapf(err, "unable to parse time of log %s", log.Time)
		}
		logLine := libpod.LogLine{
			Device:       log.Device,
			ParseLogType: log.ParseLogType,
			Time:         lTime,
			Msg:          log.Msg,
			CID:          log.Cid,
		}
		fmt.Println(logLine.String(options))
		if flags&varlink.Continues == 0 {
			break
		}
	}
	return nil
}

// CreateContainer creates a container from the cli over varlink
func (r *LocalRuntime) CreateContainer(ctx context.Context, c *cliconfig.CreateValues) (string, error) {
	results := shared.NewIntermediateLayer(&c.PodmanCommand, true)
	return iopodman.CreateContainer().Call(r.Conn, results.MakeVarlink())
}

// Run creates a container overvarlink and then starts it
func (r *LocalRuntime) Run(ctx context.Context, c *cliconfig.RunValues, exitCode int) (int, error) {
	// TODO the exit codes for run need to be figured out for remote connections
	results := shared.NewIntermediateLayer(&c.PodmanCommand, true)
	cid, err := iopodman.CreateContainer().Call(r.Conn, results.MakeVarlink())
	if err != nil {
		return 0, err
	}
	if c.Bool("detach") {
		_, err := iopodman.StartContainer().Call(r.Conn, cid)
		fmt.Println(cid)
		return 0, err
	}
	errChan, err := r.attach(ctx, os.Stdin, os.Stdout, cid, true, c.String("detach-keys"))
	if err != nil {
		return 0, err
	}
	finalError := <-errChan
	return 0, finalError
}

func ReadExitFile(runtimeTmp, ctrID string) (int, error) {
	return 0, libpod.ErrNotImplemented
}

// Ps lists containers based on criteria from user
func (r *LocalRuntime) Ps(c *cliconfig.PsValues, opts shared.PsOptions) ([]shared.PsContainerOutput, error) {
	var psContainers []shared.PsContainerOutput
	last := int64(c.Last)
	PsOpts := iopodman.PsOpts{
		All:     c.All,
		Filters: &c.Filter,
		Last:    &last,
		Latest:  &c.Latest,
		NoTrunc: &c.NoTrunct,
		Pod:     &c.Pod,
		Quiet:   &c.Quiet,
		Sort:    &c.Sort,
		Sync:    &c.Sync,
	}
	containers, err := iopodman.Ps().Call(r.Conn, PsOpts)
	if err != nil {
		return nil, err
	}
	for _, ctr := range containers {
		createdAt, err := time.Parse(time.RFC3339Nano, ctr.CreatedAt)
		if err != nil {
			return nil, err
		}
		exitedAt, err := time.Parse(time.RFC3339Nano, ctr.ExitedAt)
		if err != nil {
			return nil, err
		}
		startedAt, err := time.Parse(time.RFC3339Nano, ctr.StartedAt)
		if err != nil {
			return nil, err
		}
		containerSize := shared.ContainerSize{
			RootFsSize: ctr.RootFsSize,
			RwSize:     ctr.RwSize,
		}
		state, err := libpod.StringToContainerStatus(ctr.State)
		if err != nil {
			return nil, err
		}
		psc := shared.PsContainerOutput{
			ID:        ctr.Id,
			Image:     ctr.Image,
			Command:   ctr.Command,
			Created:   ctr.Created,
			Ports:     ctr.Ports,
			Names:     ctr.Names,
			IsInfra:   ctr.IsInfra,
			Status:    ctr.Status,
			State:     state,
			Pid:       int(ctr.PidNum),
			Size:      &containerSize,
			Pod:       ctr.Pod,
			CreatedAt: createdAt,
			ExitedAt:  exitedAt,
			StartedAt: startedAt,
			Labels:    ctr.Labels,
			PID:       ctr.NsPid,
			Cgroup:    ctr.Cgroup,
			IPC:       ctr.Ipc,
			MNT:       ctr.Mnt,
			NET:       ctr.Net,
			PIDNS:     ctr.PidNs,
			User:      ctr.User,
			UTS:       ctr.Uts,
			Mounts:    ctr.Mounts,
		}
		psContainers = append(psContainers, psc)
	}
	return psContainers, nil
}

func (r *LocalRuntime) attach(ctx context.Context, stdin, stdout *os.File, cid string, start bool, detachKeys string) (chan error, error) {
	var (
		oldTermState *term.State
	)
	errChan := make(chan error)
	spec, err := r.Spec(cid)
	if err != nil {
		return nil, err
	}
	resize := make(chan remotecommand.TerminalSize)
	haveTerminal := terminal.IsTerminal(int(os.Stdin.Fd()))

	// Check if we are attached to a terminal. If we are, generate resize
	// events, and set the terminal to raw mode
	if haveTerminal && spec.Process.Terminal {
		logrus.Debugf("Handling terminal attach")

		subCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		resizeTty(subCtx, resize)
		oldTermState, err = term.SaveState(os.Stdin.Fd())
		if err != nil {
			return nil, errors.Wrapf(err, "unable to save terminal state")
		}

		logrus.SetFormatter(&RawTtyFormatter{})
		term.SetRawTerminal(os.Stdin.Fd())

	}
	// TODO add detach keys support
	_, err = iopodman.Attach().Send(r.Conn, varlink.Upgrade, cid, detachKeys, start)
	if err != nil {
		restoreTerminal(oldTermState)
		return nil, err
	}

	// These are the varlink sockets
	reader := r.Conn.Reader
	writer := r.Conn.Writer

	// These are the special writers that encode input from the client.
	varlinkStdinWriter := virtwriter.NewVirtWriteCloser(writer, virtwriter.ToStdin)
	varlinkResizeWriter := virtwriter.NewVirtWriteCloser(writer, virtwriter.TerminalResize)

	go func() {
		// Read from the wire and direct to stdout or stderr
		err := virtwriter.Reader(reader, stdout, os.Stderr, nil, nil)
		defer restoreTerminal(oldTermState)
		errChan <- err
	}()

	go func() {
		for termResize := range resize {
			b, err := json.Marshal(termResize)
			if err != nil {
				defer restoreTerminal(oldTermState)
				errChan <- err
			}
			_, err = varlinkResizeWriter.Write(b)
			if err != nil {
				defer restoreTerminal(oldTermState)
				errChan <- err
			}
		}
	}()

	// Takes stdinput and sends it over the wire after being encoded
	go func() {
		if _, err := io.Copy(varlinkStdinWriter, stdin); err != nil {
			defer restoreTerminal(oldTermState)
			errChan <- err
		}

	}()
	return errChan, nil

}

// Attach to a remote terminal
func (r *LocalRuntime) Attach(ctx context.Context, c *cliconfig.AttachValues) error {
	ctr, err := r.LookupContainer(c.InputArgs[0])
	if err != nil {
		return nil
	}
	if ctr.state.State != libpod.ContainerStateRunning {
		return errors.New("you can only attach to running containers")
	}
	inputStream := os.Stdin
	if c.NoStdin {
		inputStream = nil
	}
	errChan, err := r.attach(ctx, inputStream, os.Stdout, c.InputArgs[0], false, c.DetachKeys)
	if err != nil {
		return err
	}
	return <-errChan
}

// Checkpoint one or more containers
func (r *LocalRuntime) Checkpoint(c *cliconfig.CheckpointValues, options libpod.ContainerCheckpointOptions) error {
	var lastError error
	ids, err := iopodman.GetContainersByContext().Call(r.Conn, c.All, c.Latest, c.InputArgs)
	if err != nil {
		return err
	}
	if c.All {
		// We dont have a great way to get all the running containers, so need to get all and then
		// check status on them bc checkpoint considers checkpointing a stopped container an error
		var runningIds []string
		for _, id := range ids {
			ctr, err := r.LookupContainer(id)
			if err != nil {
				return err
			}
			if ctr.state.State == libpod.ContainerStateRunning {
				runningIds = append(runningIds, id)
			}
		}
		ids = runningIds
	}

	for _, id := range ids {
		if _, err := iopodman.ContainerCheckpoint().Call(r.Conn, id, options.Keep, options.KeepRunning, options.TCPEstablished); err != nil {
			if lastError != nil {
				fmt.Fprintln(os.Stderr, lastError)
			}
			lastError = errors.Wrapf(err, "failed to checkpoint container %v", id)
		} else {
			fmt.Println(id)
		}
	}
	return lastError
}

// Restore one or more containers
func (r *LocalRuntime) Restore(c *cliconfig.RestoreValues, options libpod.ContainerCheckpointOptions) error {
	var lastError error
	ids, err := iopodman.GetContainersByContext().Call(r.Conn, c.All, c.Latest, c.InputArgs)
	if err != nil {
		return err
	}
	if c.All {
		// We dont have a great way to get all the exited containers, so need to get all and then
		// check status on them bc checkpoint considers restoring a running container an error
		var exitedIDs []string
		for _, id := range ids {
			ctr, err := r.LookupContainer(id)
			if err != nil {
				return err
			}
			if ctr.state.State != libpod.ContainerStateRunning {
				exitedIDs = append(exitedIDs, id)
			}
		}
		ids = exitedIDs
	}

	for _, id := range ids {
		if _, err := iopodman.ContainerRestore().Call(r.Conn, id, options.Keep, options.TCPEstablished); err != nil {
			if lastError != nil {
				fmt.Fprintln(os.Stderr, lastError)
			}
			lastError = errors.Wrapf(err, "failed to restore container %v", id)
		} else {
			fmt.Println(id)
		}
	}
	return lastError
}

// Start starts an already created container
func (r *LocalRuntime) Start(ctx context.Context, c *cliconfig.StartValues, sigProxy bool) (int, error) {
	var (
		finalErr error
		exitCode = 125
	)
	// TODO Figure out how to deal with exit codes
	inputStream := os.Stdin
	if !c.Interactive {
		inputStream = nil
	}

	containerIDs, err := iopodman.GetContainersByContext().Call(r.Conn, false, c.Latest, c.InputArgs)
	if err != nil {
		return exitCode, err
	}
	if len(containerIDs) < 1 {
		return exitCode, errors.New("failed to find containers to start")
	}
	// start.go makes sure that if attach, there can be only one ctr
	if c.Attach {
		errChan, err := r.attach(ctx, inputStream, os.Stdout, containerIDs[0], true, c.DetachKeys)
		if err != nil {
			return exitCode, nil
		}
		err = <-errChan
		return 0, err
	}

	// TODO the notion of starting a pod container and its deps still needs to be worked through
	//	Everything else is detached
	for _, cid := range containerIDs {
		reply, err := iopodman.StartContainer().Call(r.Conn, cid)
		if err != nil {
			if finalErr != nil {
				fmt.Println(err)
			}
			finalErr = err
		} else {
			fmt.Println(reply)
		}
	}
	return exitCode, finalErr
}

// PauseContainers pauses container(s) based on CLI inputs.
func (r *LocalRuntime) PauseContainers(ctx context.Context, cli *cliconfig.PauseValues) ([]string, map[string]error, error) {
	var (
		ok       []string
		failures = map[string]error{}
		ctrs     []*Container
		err      error
	)

	if cli.All {
		filters := []string{libpod.ContainerStateRunning.String()}
		ctrs, err = r.LookupContainersWithStatus(filters)
	} else {
		ctrs, err = r.LookupContainers(cli.InputArgs)
	}
	if err != nil {
		return ok, failures, err
	}

	for _, c := range ctrs {
		c := c
		err := c.Pause()
		if err != nil {
			failures[c.ID()] = err
		} else {
			ok = append(ok, c.ID())
		}
	}
	return ok, failures, nil
}

// UnpauseContainers unpauses containers based on input
func (r *LocalRuntime) UnpauseContainers(ctx context.Context, cli *cliconfig.UnpauseValues) ([]string, map[string]error, error) {
	var (
		ok       = []string{}
		failures = map[string]error{}
		ctrs     []*Container
		err      error
	)

	maxWorkers := shared.DefaultPoolSize("unpause")
	if cli.GlobalIsSet("max-workers") {
		maxWorkers = cli.GlobalFlags.MaxWorks
	}
	logrus.Debugf("Setting maximum rm workers to %d", maxWorkers)

	if cli.All {
		filters := []string{libpod.ContainerStatePaused.String()}
		ctrs, err = r.LookupContainersWithStatus(filters)
	} else {
		ctrs, err = r.LookupContainers(cli.InputArgs)
	}
	if err != nil {
		return ok, failures, err
	}
	for _, c := range ctrs {
		c := c
		err := c.Unpause()
		if err != nil {
			failures[c.ID()] = err
		} else {
			ok = append(ok, c.ID())
		}
	}
	return ok, failures, nil
}

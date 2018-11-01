package parent

import (
	"context"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/gorilla/mux"
	"github.com/opencontainers/runc/libcontainer/user"
	"github.com/pkg/errors"
	"github.com/theckman/go-flock"

	"github.com/rootless-containers/rootlesskit/pkg/api/router"
	"github.com/rootless-containers/rootlesskit/pkg/common"
	"github.com/rootless-containers/rootlesskit/pkg/msgutil"
	"github.com/rootless-containers/rootlesskit/pkg/network"
	"github.com/rootless-containers/rootlesskit/pkg/port"
)

type Opt struct {
	StateDir      string
	NetworkDriver network.ParentDriver // nil for HostNetwork
	PortDriver    port.ParentDriver    // nil for --port-driver=none
}

// Documented state files. Undocumented ones are subject to change.
const (
	StateFileLock     = "lock"
	StateFileChildPID = "child_pid" // decimal pid number text
	StateFileAPISock  = "api.sock"  // REST API Socket
)

func Parent(pipeFDEnvKey string, opt *Opt) error {
	if opt == nil {
		opt = &Opt{}
	}
	if opt.StateDir == "" {
		var err error
		opt.StateDir, err = ioutil.TempDir("", "rootlesskit")
		if err != nil {
			return errors.Wrap(err, "creating a state directory")
		}
	} else {
		if err := os.MkdirAll(opt.StateDir, 0755); err != nil {
			return errors.Wrapf(err, "creating a state directory %s", opt.StateDir)
		}
	}
	lockPath := filepath.Join(opt.StateDir, StateFileLock)
	lock := flock.NewFlock(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return errors.Wrapf(err, "failed to lock %s", lockPath)
	}
	if !locked {
		return errors.Errorf("failed to lock %s, another RootlessKit is running with the same state directory?", lockPath)
	}
	defer os.RemoveAll(opt.StateDir)
	defer lock.Unlock()
	// when the previous execution crashed, the state dir may not be removed successfully.
	// explicitly remove everything in the state dir except the lock file here.
	for _, f := range []string{StateFileChildPID} {
		p := filepath.Join(opt.StateDir, f)
		if err := os.RemoveAll(p); err != nil {
			return errors.Wrapf(err, "failed to remove %s", p)
		}
	}

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig:    syscall.SIGKILL,
		Cloneflags:   syscall.CLONE_NEWUSER,
		Unshareflags: syscall.CLONE_NEWNS,
	}
	if opt.NetworkDriver != nil {
		cmd.SysProcAttr.Unshareflags |= syscall.CLONE_NEWNET
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{pipeR}
	cmd.Env = append(os.Environ(), pipeFDEnvKey+"=3")
	if err := cmd.Start(); err != nil {
		return errors.Wrap(err, "failed to start the child")
	}
	childPIDPath := filepath.Join(opt.StateDir, StateFileChildPID)
	if err := ioutil.WriteFile(childPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0444); err != nil {
		return errors.Wrapf(err, "failed to write the child PID %d to %s", cmd.Process.Pid, childPIDPath)
	}
	if err := setupUIDGIDMap(cmd.Process.Pid); err != nil {
		return errors.Wrap(err, "failed to setup UID/GID map")
	}

	// configure Network driver
	msg := common.Message{
		StateDir: opt.StateDir,
	}
	if opt.NetworkDriver != nil {
		netMsg, cleanupNetwork, err := opt.NetworkDriver.ConfigureNetwork(cmd.Process.Pid, opt.StateDir)
		if cleanupNetwork != nil {
			defer cleanupNetwork()
		}
		if err != nil {
			return errors.Wrapf(err, "failed to setup network %+v", opt.NetworkDriver)
		}
		msg.Network = *netMsg
	}

	// configure Port driver
	portDriverInitComplete := make(chan struct{})
	portDriverQuit := make(chan struct{})
	portDriverErr := make(chan error)
	if opt.PortDriver != nil {
		msg.Port.Opaque = opt.PortDriver.OpaqueForChild()
		go func() {
			portDriverErr <- opt.PortDriver.RunParentDriver(portDriverInitComplete,
				portDriverQuit, cmd.Process.Pid)
		}()
	}

	// wake up the child
	if _, err := msgutil.MarshalToWriter(pipeW, &msg); err != nil {
		return err
	}
	if err := pipeW.Close(); err != nil {
		return err
	}
	// wait for port driver to be ready
	if opt.PortDriver != nil {
		<-portDriverInitComplete
	}
	// listens the API
	apiSockPath := filepath.Join(opt.StateDir, StateFileAPISock)
	apiCloser, err := listenServeAPI(apiSockPath, &router.Backend{PortDriver: opt.PortDriver})
	if err != nil {
		return err
	}
	// block until the child exits
	if err := cmd.Wait(); err != nil {
		return errors.Wrap(err, "child exited")
	}
	// close the API socket
	if err := apiCloser.Close(); err != nil {
		return errors.Wrapf(err, "failed to close %s", apiSockPath)
	}
	// shut down port driver
	if opt.PortDriver != nil {
		portDriverQuit <- struct{}{}
		err = <-portDriverErr
	}
	return err
}

func newuidmapArgs() ([]string, error) {
	u, err := user.CurrentUser()
	if err != nil {
		return nil, err
	}
	res := []string{
		"0",
		strconv.Itoa(u.Uid),
		"1",
	}
	subs, err := user.CurrentUserSubUIDs()
	if err != nil {
		return nil, err
	}
	// TODO: continue with non-subuid on ENOENT maybe
	last := 1
	for _, sub := range subs {
		res = append(res, []string{
			strconv.Itoa(last),
			strconv.Itoa(int(sub.SubID)),
			strconv.Itoa(int(sub.Count)),
		}...)
		last += int(sub.Count)
	}
	return res, nil
}

func newgidmapArgs() ([]string, error) {
	g, err := user.CurrentGroup()
	if err != nil {
		return nil, err
	}
	res := []string{
		"0",
		strconv.Itoa(g.Gid),
		"1",
	}
	subs, err := user.CurrentUserSubGIDs()
	if err != nil {
		return nil, err
	}
	// TODO: continue with non-subgid on ENOENT maybe
	last := 1
	for _, sub := range subs {
		res = append(res, []string{
			strconv.Itoa(last),
			strconv.Itoa(int(sub.SubID)),
			strconv.Itoa(int(sub.Count)),
		}...)
		last += int(sub.Count)
	}
	return res, nil
}

func setupUIDGIDMap(pid int) error {
	uArgs, err := newuidmapArgs()
	if err != nil {
		return errors.Wrap(err, "failed to compute uid map")
	}
	gArgs, err := newgidmapArgs()
	if err != nil {
		return errors.Wrap(err, "failed to compute gid map")
	}
	pidS := strconv.Itoa(pid)
	cmd := exec.Command("newuidmap", append([]string{pidS}, uArgs...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "newuidmap %s %v failed: %s", pidS, uArgs, string(out))
	}
	cmd = exec.Command("newgidmap", append([]string{pidS}, gArgs...)...)
	out, err = cmd.CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "newgidmap %s %v failed: %s", pidS, gArgs, string(out))
	}
	return nil
}

// apiCloser is implemented by *http.Server
type apiCloser interface {
	Close() error
	Shutdown(context.Context) error
}

func listenServeAPI(socketPath string, backend *router.Backend) (apiCloser, error) {
	r := mux.NewRouter()
	router.AddRoutes(r, backend)
	srv := &http.Server{Handler: r}
	err := os.RemoveAll(socketPath)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	go srv.Serve(l)
	return srv, nil
}

package builtin

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/rootless-containers/rootlesskit/pkg/msgutil"
	"github.com/rootless-containers/rootlesskit/pkg/port"
)

const (
	opaqueKeySocketPath         = "builtin.socketpath"
	opaqueKeyChildReadyPipePath = "builtin.readypipepath"
)

// NewParentDriver for builtin driver.
// DNM! RemovePort() is broken
func NewParentDriver(logWriter io.Writer, stateDir string) (port.ParentDriver, error) {
	// TODO: consider using socketpair FD instead of socket file
	socketPath := filepath.Join(stateDir, ".bp.sock")
	childReadyPipePath := filepath.Join(stateDir, ".bp-ready.pipe")
	if err := syscall.Mkfifo(childReadyPipePath, 0600); err != nil {
		return nil, err
	}
	d := driver{
		logWriter:          logWriter,
		socketPath:         socketPath,
		childReadyPipePath: childReadyPipePath,
		ports:              make(map[int]*port.Status, 0),
		stoppers:           make(map[int]func() error, 0),
		nextID:             1,
	}
	return &d, nil
}

type driver struct {
	logWriter          io.Writer
	socketPath         string
	childReadyPipePath string
	mu                 sync.Mutex
	ports              map[int]*port.Status
	stoppers           map[int]func() error
	nextID             int
}

func (d *driver) OpaqueForChild() map[string]string {
	return map[string]string{
		opaqueKeySocketPath:         d.socketPath,
		opaqueKeyChildReadyPipePath: d.childReadyPipePath,
	}
}

func (d *driver) RunParentDriver(initComplete chan struct{}, quit <-chan struct{}, _ int) error {
	childReadyPipeR, err := os.OpenFile(d.childReadyPipePath, os.O_RDONLY, os.ModeNamedPipe)
	if err != nil {
		return err
	}
	if _, err = ioutil.ReadAll(childReadyPipeR); err != nil {
		return err
	}
	childReadyPipeR.Close()
	var dialer net.Dialer
	conn, err := dialer.Dial("unix", d.socketPath)
	if err != nil {
		return err
	}
	err = initiate(conn.(*net.UnixConn))
	conn.Close()
	if err != nil {
		return err
	}
	initComplete <- struct{}{}
	<-quit
	return nil
}

func (d *driver) AddPort(ctx context.Context, spec port.Spec) (*port.Status, error) {
	d.mu.Lock()
	for id, p := range d.ports {
		sp := p.Spec
		// FIXME: add more ParentIP checks
		if sp.Proto == spec.Proto && sp.ParentIP == spec.ParentIP && sp.ParentPort == spec.ParentPort {
			d.mu.Unlock()
			return nil, errors.Errorf("conflict with ID %d", id)
		}
	}
	d.mu.Unlock()
	routineErrorCh := make(chan error)
	routineStopCh := make(chan struct{})
	routineStop := func() error {
		close(routineStopCh)
		return <-routineErrorCh
	}
	go portRoutine(d.socketPath, spec, routineStopCh, routineErrorCh, d.logWriter)
	d.mu.Lock()
	id := d.nextID
	st := port.Status{
		ID:   id,
		Spec: spec,
	}
	d.ports[id] = &st
	d.stoppers[id] = routineStop
	d.nextID++
	d.mu.Unlock()
	return &st, nil
}

func (d *driver) ListPorts(ctx context.Context) ([]port.Status, error) {
	var ports []port.Status
	d.mu.Lock()
	for _, p := range d.ports {
		ports = append(ports, *p)
	}
	d.mu.Unlock()
	return ports, nil
}

func (d *driver) RemovePort(ctx context.Context, id int) error {
	return nil
	// FIXME
	d.mu.Lock()
	defer d.mu.Unlock()
	stop, ok := d.stoppers[id]
	if !ok {
		return errors.Errorf("unknown id: %d", id)
	}
	err := stop()
	delete(d.stoppers, id)
	delete(d.ports, id)
	return err
}

func initiate(c *net.UnixConn) error {
	req := request{
		Type: requestTypeInit,
	}
	if _, err := msgutil.MarshalToWriter(c, &req); err != nil {
		return err
	}
	if err := c.CloseWrite(); err != nil {
		return err
	}
	var rep reply
	if _, err := msgutil.UnmarshalFromReader(c, &rep); err != nil {
		return err
	}
	return c.CloseRead()
}

func connectToChild(socketPath string, spec port.Spec) (int, error) {
	var dialer net.Dialer
	conn, err := dialer.Dial("unix", socketPath)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	c := conn.(*net.UnixConn)
	req := request{
		Type:  requestTypeConnect,
		Proto: spec.Proto,
		Port:  spec.ChildPort,
	}
	if _, err := msgutil.MarshalToWriter(c, &req); err != nil {
		return 0, err
	}
	if err := c.CloseWrite(); err != nil {
		return 0, err
	}
	oobSpace := unix.CmsgSpace(4)
	oob := make([]byte, oobSpace)
	_, oobN, _, _, err := c.ReadMsgUnix(nil, oob)
	if err != nil {
		return 0, err
	}
	if oobN != oobSpace {
		return 0, errors.Errorf("expected OOB space %d, got %d", oobSpace, oobN)
	}
	oob = oob[:oobN]
	fd, err := parseFDFromOOB(oob)
	if err != nil {
		return 0, err
	}
	if err := c.CloseRead(); err != nil {
		return 0, err
	}
	return fd, nil
}

func connectToChildWithRetry(socketPath string, spec port.Spec, retries int) (int, error) {
	for i := 0; i < retries; i++ {
		fd, err := connectToChild(socketPath, spec)
		if i == retries-1 && err != nil {
			return 0, err
		}
		if err == nil {
			return fd, err
		}
		// TODO: backoff
		time.Sleep(time.Duration(i*5) * time.Millisecond)
	}
	// NOT REACHED
	return 0, errors.New("reached max retry")
}

func parseFDFromOOB(oob []byte) (int, error) {
	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0, err
	}
	if len(scms) != 1 {
		return 0, errors.Errorf("unexpected scms: %v", scms)
	}
	scm := scms[0]
	fds, err := unix.ParseUnixRights(&scm)
	if err != nil {
		return 0, err
	}
	if len(fds) != 1 {
		return 0, errors.Errorf("unexpected fds: %v", fds)
	}
	return fds[0], nil
}

func portRoutine(socketPath string, spec port.Spec, stopCh <-chan struct{}, errWCh chan error, logWriter io.Writer) {
	if spec.Proto != "tcp" {
		errWCh <- errors.Errorf("unexpected proto: %s", spec.Proto)
		return
	}
	ln, err := net.Listen(spec.Proto, fmt.Sprintf("%s:%d", spec.ParentIP, spec.ParentPort))
	if err != nil {
		errWCh <- err
		return
	}
	defer ln.Close()
	newConns := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				newConns <- nil
				return
			}
			newConns <- c
		}
	}()
	for {
		select {
		case c := <-newConns:
			go func() {
				defer c.Close()
				// get fd from the child as an SCM_RIGHTS cmsg
				fd, err := connectToChildWithRetry(socketPath, spec, 10)
				if err != nil {
					errWCh <- err
					return
				}
				f := os.NewFile(uintptr(fd), "")
				defer f.Close()
				fc, err := net.FileConn(f)
				if err != nil {
					errWCh <- err
					return
				}
				defer fc.Close()
				// TODO: support non-TCP
				bicopy(c.(*net.TCPConn), fc.(*net.TCPConn), stopCh)
			}()
		case <-stopCh:
			ln.Close()
			return
		}
	}
}

// bicopy is based on libnetwork/cmd/proxy/tcp_proxy.go .
// NOTE: sendfile(2) cannot be used for sockets
func bicopy(x, y *net.TCPConn, quit <-chan struct{}) {
	var wg sync.WaitGroup
	var broker = func(to, from *net.TCPConn) {
		io.Copy(to, from)
		from.CloseRead()
		to.CloseWrite()
		wg.Done()
	}

	wg.Add(2)
	go broker(x, y)
	go broker(y, x)
	finish := make(chan struct{})
	go func() {
		wg.Wait()
		close(finish)
	}()

	select {
	case <-quit:
	case <-finish:
	}
	x.Close()
	y.Close()
	<-finish
}

const (
	requestTypeInit    = "init"
	requestTypeConnect = "connect"
)

// request and response are encoded as JSON with uint32le length header.
type request struct {
	Type  string // "init" or "connect"
	Proto string // "tcp" or "udp"
	Port  int
}

// may contain FD as OOB
type reply struct {
	Error string
}

func NewChildDriver(logWriter io.Writer) port.ChildDriver {
	return &childDriver{
		logWriter: logWriter,
	}
}

type childDriver struct {
	logWriter io.Writer
}

func (d *childDriver) RunChildDriver(opaque map[string]string, quit <-chan struct{}) error {
	socketPath := opaque[opaqueKeySocketPath]
	if socketPath == "" {
		return errors.New("socket path not set")
	}
	childReadyPipePath := opaque[opaqueKeyChildReadyPipePath]
	if childReadyPipePath == "" {
		return errors.New("child ready pipe path not set")
	}
	childReadyPipeW, err := os.OpenFile(childReadyPipePath, os.O_WRONLY, os.ModeNamedPipe)
	if err != nil {
		return err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: socketPath,
		Net:  "unix",
	})
	if err != nil {
		return err
	}
	// write nothing, just close
	if err = childReadyPipeW.Close(); err != nil {
		return err
	}
	stopAccept := make(chan struct{}, 1)
	go func() {
		<-quit
		stopAccept <- struct{}{}
		ln.Close()
	}()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			select {
			case <-stopAccept:
				return nil
			default:
			}
			return err
		}
		go func() {
			if rerr := d.routine(c); rerr != nil {
				rep := reply{
					Error: rerr.Error(),
				}
				msgutil.MarshalToWriter(c, &rep)
			}
			c.Close()
		}()
	}
	return nil
}

func (d *childDriver) routine(c *net.UnixConn) error {
	var req request
	if _, err := msgutil.UnmarshalFromReader(c, &req); err != nil {
		return err
	}
	switch req.Type {
	case requestTypeInit:
		return d.handleConnectInit(c, &req)
	case requestTypeConnect:
		return d.handleConnectRequest(c, &req)
	default:
		return errors.Errorf("unknown request type %q", req.Type)
	}
}

func (d *childDriver) handleConnectInit(c *net.UnixConn, req *request) error {
	_, err := msgutil.MarshalToWriter(c, nil)
	return err
}

func (d *childDriver) handleConnectRequest(c *net.UnixConn, req *request) error {
	switch req.Proto {
	case "tcp":
	default:
		return errors.Errorf("unknown proto: %q", req.Proto)
	}
	var dialer net.Dialer
	targetConn, err := dialer.Dial(req.Proto, fmt.Sprintf("127.0.0.1:%d", req.Port))
	if err != nil {
		return err
	}
	defer targetConn.Close() // no effect on duplicated FD
	targetConnTCP, ok := targetConn.(*net.TCPConn)
	if !ok {
		return errors.Errorf("unknown target connection: %+v", targetConn)
	}
	targetConnFile, err := targetConnTCP.File()
	if err != nil {
		return err
	}
	oob := unix.UnixRights(int(targetConnFile.Fd()))
	f, err := c.File()
	if err != nil {
		return err
	}
	err = unix.Sendmsg(int(f.Fd()), []byte("dummy"), oob, nil, 0)
	return err
}

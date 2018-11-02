package builtin

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/rootless-containers/rootlesskit/pkg/msgutil"
	"github.com/rootless-containers/rootlesskit/pkg/port"
)

const (
	opaqueKeySocketPath = "builtin.socketpath"
)

// NewParentDriver for builtin driver.
// socketPath must not exist. The socket API is opaque.
//
// TODO: consider using socketpair FD instead of socket file
func NewParentDriver(logWriter io.Writer, socketPath string) (port.ParentDriver, error) {
	d := driver{
		logWriter:  logWriter,
		socketPath: socketPath,
		ports:      make(map[int]*port.Status, 0),
		nextID:     1,
	}
	return &d, nil
}

type driver struct {
	logWriter   io.Writer
	socketPath  string
	mu          sync.Mutex
	ports       map[int]*port.Status
	cmdStoppers map[int]func() error
	nextID      int
}

func (d *driver) OpaqueForChild() map[string]string {
	return map[string]string{
		opaqueKeySocketPath: d.socketPath,
	}
}

func (d *driver) RunParentDriver(initComplete chan struct{}, quit <-chan struct{}, _ int) error {
	var dialer net.Dialer
	// FIXME: socket might not exist yet!!
	conn, err := dialer.Dial("unix", d.socketPath)
	if err != nil {
		return err
	}
	err = sendInitRequest(conn.(*net.UnixConn))
	conn.Close()
	if err != nil {
		return err
	}
	initComplete <- struct{}{}
	<-quit
	return nil
}

func (d *driver) AddPort(ctx context.Context, spec port.Spec) (*port.Status, error) {
	return nil, errors.New("unimplemented")
}

func (d *driver) ListPorts(ctx context.Context) ([]port.Status, error) {
	return nil, nil
}

func (d *driver) RemovePort(ctx context.Context, id int) error {
	return errors.Errorf("unknown id: %d", id)
}

func sendInitRequest(c *net.UnixConn) error {
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
	ln, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: socketPath,
		Net:  "unix",
	})
	if err != nil {
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
				fmt.Fprintf(d.logWriter, "%v\n", rerr)
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
	resp, _ := msgutil.Marshal(nil)
	_, _, err = c.WriteMsgUnix(resp, oob, nil)
	return err
}

package port

import (
	"context"
)

type Spec struct {
	Proto      string `json:"proto,omitempty"`    // either "tcp" or "udp". in future "sctp" will be supported as well.
	ParentIP   string `json:"parentIP,omitempty"` // IPv4 address. can be empty (0.0.0.0).
	ParentPort int    `json:"parentPort,omitempty"`
	ChildPort  int    `json:"childPort,omitempty"`
}

type Status struct {
	ID   int  `json:"id"`
	Spec Spec `json:"spec"`
}

// Manager MUST be thread-safe.
type Manager interface {
	AddPort(ctx context.Context, spec Spec) (*Status, error)
	ListPorts(ctx context.Context) ([]Status, error)
	RemovePort(ctx context.Context, id int) error
}

// ParentDriver is a driver for the parent process.
type ParentDriver interface {
	Manager
	// OpaqueForChild typically consists of socket path
	// for controlling child from parent
	OpaqueForChild() map[string]string
	// RunParentDriver signals initComplete when ParentDriver is ready to
	// serve as Manager.
	// RunParentDriver blocks until quit is signaled.
	// childPID can be used for ns-entering to the child namespaces.
	//
	// TODO: remove childPID from RunParentDriver, let the parent receive the PID
	// from the child via a socket specified in opaque, as SCM_CREDENTIALS instead?
	RunParentDriver(initComplete chan struct{}, quit <-chan struct{}, childPID int) error
}

type ChildDriver interface {
	RunChildDriver(opaque map[string]string, quit <-chan struct{}) error
}

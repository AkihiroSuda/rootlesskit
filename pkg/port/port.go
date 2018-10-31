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
	SetChildPID(int)
}

// TODO: add ChildDriver

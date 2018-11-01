package socat

import (
	"testing"

	"github.com/rootless-containers/rootlesskit/pkg/port"
	"github.com/rootless-containers/rootlesskit/pkg/port/testsuite"
)

func TestMain(m *testing.M) {
	cf := func() port.ChildDriver {
		return NewChildDriver()
	}
	testsuite.Main(m, cf)
}

func TestSocat(t *testing.T) {
	pf := func() port.ParentDriver {
		d, err := NewParentDriver(testsuite.TLogWriter(t, "socat.Driver"))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	testsuite.Run(t, pf)
}

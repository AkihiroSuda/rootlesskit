package main

import (
	"fmt"
	"os"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/AkihiroSuda/rootlesskit/pkg/child"
	"github.com/AkihiroSuda/rootlesskit/pkg/common"
	"github.com/AkihiroSuda/rootlesskit/pkg/parent"
)

func main() {
	pipeFDEnvKey := "_ROOTLESSKIT_PIPEFD_UNDOCUMENTED"
	iAmChild := os.Getenv(pipeFDEnvKey) != ""
	debug := false
	app := cli.NewApp()
	app.Name = "rootlesskit"
	app.Usage = "the gate to the rootless world"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:        "debug",
			Usage:       "debug mode",
			Destination: &debug,
		},
		cli.StringFlag{
			Name:  "net",
			Usage: "host, vdeplug_slirp",
			Value: "host",
		},
	}
	app.Before = func(context *cli.Context) error {
		if debug {
			logrus.SetLevel(logrus.DebugLevel)
		}
		return nil
	}
	app.Action = func(clicontext *cli.Context) error {
		if clicontext.NArg() < 1 {
			return errors.New("no command specified")
		}
		if iAmChild {
			return child.Child(pipeFDEnvKey, clicontext.Args())
		}
		parentOpt, err := createParentOpt(clicontext)
		if err != nil {
			return err
		}
		return parent.Parent(pipeFDEnvKey, parentOpt)
	}
	if err := app.Run(os.Args); err != nil {
		id := "parent"
		if iAmChild {
			id = "child " // padded to len("parent")
		}
		if debug {
			fmt.Fprintf(os.Stderr, "[rootlesskit:%s] error: %+v\n", id, err)
		} else {
			fmt.Fprintf(os.Stderr, "[rootlesskit:%s] error: %v\n", id, err)
		}
		// propagate the exit code
		code, ok := common.GetExecExitStatus(err)
		if !ok {
			code = 1
		}
		os.Exit(code)
	}
}

func parseNetworkMode(s string) (common.NetworkMode, error) {
	switch s {
	case "host":
		return common.HostNetwork, nil
	case "vdeplug_slirp":
		return common.VDEPlugSlirp, nil
	default:
		return -1, errors.Errorf("unknown network mode: %s", s)
	}
}

func createParentOpt(clicontext *cli.Context) (*parent.Opt, error) {
	opt := &parent.Opt{}
	var err error
	opt.NetworkMode, err = parseNetworkMode(clicontext.String("net"))
	if err != nil {
		return nil, err
	}
	return opt, nil
}

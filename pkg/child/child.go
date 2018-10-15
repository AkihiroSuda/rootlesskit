package child

import (
	"encoding/binary"
	"encoding/json"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/rootless-containers/rootlesskit/pkg/common"
	"github.com/rootless-containers/rootlesskit/pkg/network"
	"github.com/rootless-containers/rootlesskit/pkg/network/slirp4netns"
	"github.com/rootless-containers/rootlesskit/pkg/network/vdeplugslirp"
	"github.com/rootless-containers/rootlesskit/pkg/network/vpnkit"
)

func waitForParentSync(pipeFDStr string) (*common.Message, error) {
	pipeFD, err := strconv.Atoi(pipeFDStr)
	if err != nil {
		return nil, errors.Wrapf(err, "unexpected fd value: %s", pipeFDStr)
	}
	pipeR := os.NewFile(uintptr(pipeFD), "")
	hdr := make([]byte, 4)
	n, err := pipeR.Read(hdr)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read fd %d", pipeFD)
	}
	if n != 4 {
		return nil, errors.Errorf("read %d bytes, expected 4 bytes", n)
	}
	bLen := binary.LittleEndian.Uint32(hdr)
	if bLen > 1<<16 || bLen < 1 {
		return nil, errors.Errorf("bad message size: %d", bLen)
	}
	b := make([]byte, bLen)
	n, err = pipeR.Read(b)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read fd %d", pipeFD)
	}
	if n != int(bLen) {
		return nil, errors.Errorf("read %d bytes, expected %d bytes", n, bLen)
	}
	var msg common.Message
	if err := json.Unmarshal(b, &msg); err != nil {
		return nil, errors.Wrapf(err, "parsing message from fd %d: %q (length %d)", pipeFD, string(b), bLen)
	}
	if err := pipeR.Close(); err != nil {
		return nil, errors.Wrapf(err, "failed to close fd %d", pipeFD)
	}
	if msg.StateDir == "" {
		return nil, errors.New("got empty StateDir")
	}
	return &msg, nil
}

func createCmd(targetCmd []string) (*exec.Cmd, error) {
	var args []string
	if len(targetCmd) > 1 {
		args = targetCmd[1:]
	}
	cmd := exec.Command(targetCmd[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	return cmd, nil
}

// mountSysfs is needed for mounting /sys/class/net
// when netns is unshared.
func mountSysfs() error {
	tmp, err := ioutil.TempDir("/tmp", "rksys")
	if err != nil {
		return errors.Wrap(err, "creating a directory under /tmp")
	}
	defer os.RemoveAll(tmp)
	cmds := [][]string{{"mount", "--rbind", "/sys/fs/cgroup", tmp}}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	cmds = [][]string{{"mount", "-t", "sysfs", "none", "/sys"}}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		// when the sysfs in the parent namespace is RO,
		// we can't mount RW sysfs even in the child namespace.
		// https://github.com/rootless-containers/rootlesskit/pull/23#issuecomment-429292632
		// https://github.com/torvalds/linux/blob/9f203e2f2f065cd74553e6474f0ae3675f39fb0f/fs/namespace.c#L3326-L3328
		cmdsRo := [][]string{{"mount", "-t", "sysfs", "-o", "ro", "none", "/sys"}}
		logrus.Warnf("failed to mount sysfs (%v), falling back to read-only mount (%v): %v",
			cmds, cmdsRo, err)
		if err := common.Execs(os.Stderr, os.Environ(), cmdsRo); err != nil {
			// when /sys/firmware is masked, even RO sysfs can't be mounted
			logrus.Warnf("failed to mount sysfs (%v): %v", cmdsRo, err)
		}
	}
	cmds = [][]string{{"mount", "-n", "--move", tmp, "/sys/fs/cgroup"}}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	return nil
}

func activateLoopback() error {
	cmds := [][]string{
		{"ip", "link", "set", "lo", "up"},
	}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	return nil
}

func activateTap(tap, ip string, netmask int, gateway string, mtu int) error {
	cmds := [][]string{
		{"ip", "link", "set", tap, "up"},
		{"ip", "link", "set", "dev", tap, "mtu", strconv.Itoa(mtu)},
		{"ip", "addr", "add", ip + "/" + strconv.Itoa(netmask), "dev", tap},
		{"ip", "route", "add", "default", "via", gateway, "dev", tap},
	}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	return nil
}

func getNetworkDriver(netmsg common.NetworkMessage) (network.ChildDriver, error) {
	switch netmsg.NetworkMode {
	case common.VDEPlugSlirp:
		return vdeplugslirp.NewChildDriver(), nil
	case common.Slirp4NetNS:
		return slirp4netns.NewChildDriver(), nil
	case common.VPNKit:
		return vpnkit.NewChildDriver(), nil
	default:
		// HostNetwork does not have driver
		return nil, errors.Errorf("invalid network mode: %+v", netmsg.NetworkMode)
	}
}

func setupNet(msg common.Message, etcWasCopied bool) error {
	if msg.Network.NetworkMode == common.HostNetwork {
		return nil
	}
	// for /sys/class/net
	if err := mountSysfs(); err != nil {
		return err
	}
	if err := activateLoopback(); err != nil {
		return err
	}
	driver, err := getNetworkDriver(msg.Network)
	if err != nil {
		return err
	}
	tap, err := driver.ConfigureTap(msg.Network)
	if err != nil {
		return err
	}
	if err := activateTap(tap, msg.Network.IP, msg.Network.Netmask, msg.Network.Gateway, msg.Network.MTU); err != nil {
		return err
	}
	if etcWasCopied {
		if err := writeResolvConf(msg.Network.DNS); err != nil {
			return err
		}
		if err := writeEtcHosts(); err != nil {
			return err
		}
	} else {
		logrus.Warn("Mounting /etc/resolv.conf without copying-up /etc. " +
			"Note that /etc/resolv.conf in the namespace will be unmounted when it is recreated on the host. " +
			"Unless /etc/resolv.conf is statically configured, copying-up /etc is highly recommended. " +
			"Please refer to RootlessKit documentation for further information.")
		if err := mountResolvConf(msg.StateDir, msg.Network.DNS); err != nil {
			return err
		}
		if err := mountEtcHosts(msg.StateDir); err != nil {
			return err
		}
	}
	return nil
}

func setupCopyUp(msg common.Message) ([]string, error) {
	switch msg.CopyUpMode {
	case common.TmpfsWithSymlinkCopyUp:
	default:
		return nil, errors.Errorf("invalid copy-up mode: %+v", msg.CopyUpMode)
	}
	// we create bind0 outside of msg.StateDir so as to allow
	// copying up /run with stateDir=/run/user/1001/rootlesskit/default.
	bind0, err := ioutil.TempDir("/tmp", "rootlesskit-b")
	if err != nil {
		return nil, errors.Wrap(err, "creating bind0 directory under /tmp")
	}
	defer os.RemoveAll(bind0)
	var copied []string
	for _, d := range msg.CopyUpDirs {
		d := filepath.Clean(d)
		if d == "/tmp" {
			// TODO: we can support copy-up /tmp by changing bind0TempDir
			return copied, errors.New("/tmp cannot be copied up")
		}
		cmds := [][]string{
			// TODO: read-only bind (does not work well for /run)
			{"mount", "--rbind", d, bind0},
			{"mount", "-n", "-t", "tmpfs", "none", d},
		}
		if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
			return copied, errors.Wrapf(err, "executing %v", cmds)
		}
		bind1, err := ioutil.TempDir(d, ".ro")
		if err != nil {
			return copied, errors.Wrapf(err, "creating a directory under %s", d)
		}
		cmds = [][]string{
			{"mount", "-n", "--move", bind0, bind1},
		}
		if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
			return copied, errors.Wrapf(err, "executing %v", cmds)
		}
		files, err := ioutil.ReadDir(bind1)
		if err != nil {
			return copied, errors.Wrapf(err, "reading dir %s", bind1)
		}
		for _, f := range files {
			fFull := filepath.Join(bind1, f.Name())
			var symlinkSrc string
			if f.Mode()&os.ModeSymlink != 0 {
				symlinkSrc, err = os.Readlink(fFull)
				if err != nil {
					return copied, errors.Wrapf(err, "reading dir %s", fFull)
				}
			} else {
				symlinkSrc = filepath.Join(filepath.Base(bind1), f.Name())
			}
			symlinkDst := filepath.Join(d, f.Name())
			if err := os.Symlink(symlinkSrc, symlinkDst); err != nil {
				return copied, errors.Wrapf(err, "symlinking %s to %s", symlinkSrc, symlinkDst)
			}
		}
		copied = append(copied, d)
	}
	return copied, nil
}

func Child(pipeFDEnvKey string, targetCmd []string) error {
	pipeFDStr := os.Getenv(pipeFDEnvKey)
	if pipeFDStr == "" {
		return errors.Errorf("%s is not set", pipeFDEnvKey)
	}
	os.Unsetenv(pipeFDEnvKey)
	msg, err := waitForParentSync(pipeFDStr)
	if err != nil {
		return err
	}
	logrus.Debugf("child: got msg from parent: %+v", msg)
	copied, err := setupCopyUp(*msg)
	if err != nil {
		return err
	}
	etcWasCopied := false
	for _, d := range copied {
		if d == "/etc" {
			etcWasCopied = true
			break
		}
	}
	if err := setupNet(*msg, etcWasCopied); err != nil {
		return err
	}
	cmd, err := createCmd(targetCmd)
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "command %v exited", targetCmd)
	}
	return nil
}

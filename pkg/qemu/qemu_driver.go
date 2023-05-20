package qemu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
	"github.com/digitalocean/go-qemu/qmp/raw"
	"github.com/lima-vm/lima/pkg/driver"
	"github.com/lima-vm/lima/pkg/limayaml"
	"github.com/lima-vm/lima/pkg/networks/usernet"
	"github.com/lima-vm/lima/pkg/store"
	"github.com/lima-vm/lima/pkg/store/filenames"
	"github.com/sirupsen/logrus"
)

type LimaQemuDriver struct {
	*driver.BaseDriver
	qCmd    *exec.Cmd
	qWaitCh chan error
}

func New(driver *driver.BaseDriver) *LimaQemuDriver {
	return &LimaQemuDriver{
		BaseDriver: driver,
	}
}

func (l *LimaQemuDriver) Validate() error {
	if *l.Yaml.MountType == limayaml.VIRTIOFS {
		return fmt.Errorf("field `mountType` must be %q or %q for QEMU driver , got %q", limayaml.REVSSHFS, limayaml.NINEP, *l.Yaml.MountType)
	}
	return nil
}

func (l *LimaQemuDriver) CreateDisk() error {
	qCfg := Config{
		Name:        l.Instance.Name,
		InstanceDir: l.Instance.Dir,
		LimaYAML:    l.Yaml,
	}
	if err := EnsureDisk(qCfg); err != nil {
		return err
	}

	return nil
}

func (l *LimaQemuDriver) Start(ctx context.Context) (chan error, error) {
	qCfg := Config{
		Name:         l.Instance.Name,
		InstanceDir:  l.Instance.Dir,
		LimaYAML:     l.Yaml,
		SSHLocalPort: l.SSHLocalPort,
	}
	qExe, qArgs, err := Cmdline(qCfg)
	if err != nil {
		return nil, err
	}

	var qArgsFinal []string
	applier := &qArgTemplateApplier{}
	for _, unapplied := range qArgs {
		applied, err := applier.applyTemplate(unapplied)
		if err != nil {
			return nil, err
		}
		qArgsFinal = append(qArgsFinal, applied)
	}
	qCmd := exec.CommandContext(ctx, qExe, qArgsFinal...)
	qCmd.ExtraFiles = append(qCmd.ExtraFiles, applier.files...)
	qStdout, err := qCmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	go logPipeRoutine(qStdout, "qemu[stdout]")
	qStderr, err := qCmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	go logPipeRoutine(qStderr, "qemu[stderr]")

	logrus.Infof("Starting QEMU (hint: to watch the boot progress, see %q)", filepath.Join(qCfg.InstanceDir, filenames.SerialLog))
	logrus.Debugf("qCmd.Args: %v", qCmd.Args)
	if err := qCmd.Start(); err != nil {
		return nil, err
	}
	l.qCmd = qCmd
	l.qWaitCh = make(chan error)
	go func() {
		l.qWaitCh <- qCmd.Wait()
	}()
	go func() {
		if usernetIndex := limayaml.FirstUsernetIndex(l.Yaml); usernetIndex != -1 {
			client := newUsernetClient(l.Yaml.Networks[usernetIndex].Lima)
			err = client.ResolveAndForwardSSH(limayaml.MACAddress(l.Instance.Dir), l.SSHLocalPort)
		}
	}()
	return l.qWaitCh, nil
}

func (l *LimaQemuDriver) Stop(ctx context.Context) error {
	return l.shutdownQEMU(ctx, 3*time.Minute, l.qCmd, l.qWaitCh)
}

func (l *LimaQemuDriver) ChangeDisplayPassword(ctx context.Context, password string) error {
	return l.changeVNCPassword(ctx, password)
}

func (l *LimaQemuDriver) GetDisplayConnection(ctx context.Context) (string, error) {
	return l.getVNCDisplayPort(ctx)
}

func waitFileExists(path string, timeout time.Duration) error {
	startWaiting := time.Now()
	for {
		_, err := os.Stat(path)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if time.Since(startWaiting) > timeout {
			return fmt.Errorf("timeout waiting for %s", path)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func (l *LimaQemuDriver) changeVNCPassword(ctx context.Context, password string) error {
	qmpSockPath := filepath.Join(l.Instance.Dir, filenames.QMPSock)
	err := waitFileExists(qmpSockPath, 30*time.Second)
	if err != nil {
		return err
	}
	qmpClient, err := qmp.NewSocketMonitor("unix", qmpSockPath, 5*time.Second)
	if err != nil {
		return err
	}
	if err := qmpClient.Connect(); err != nil {
		return err
	}
	defer func() { _ = qmpClient.Disconnect() }()
	rawClient := raw.NewMonitor(qmpClient)
	err = rawClient.ChangeVNCPassword(password)
	if err != nil {
		return err
	}
	return nil
}

func (l *LimaQemuDriver) getVNCDisplayPort(ctx context.Context) (string, error) {
	qmpSockPath := filepath.Join(l.Instance.Dir, filenames.QMPSock)
	qmpClient, err := qmp.NewSocketMonitor("unix", qmpSockPath, 5*time.Second)
	if err != nil {
		return "", err
	}
	if err := qmpClient.Connect(); err != nil {
		return "", err
	}
	defer func() { _ = qmpClient.Disconnect() }()
	rawClient := raw.NewMonitor(qmpClient)
	info, err := rawClient.QueryVNC()
	if err != nil {
		return "", err
	}
	return *info.Service, nil
}

func (l *LimaQemuDriver) removeVNCFiles() error {
	vncfile := filepath.Join(l.Instance.Dir, filenames.VNCDisplayFile)
	err := os.RemoveAll(vncfile)
	if err != nil {
		return err
	}
	vncpwdfile := filepath.Join(l.Instance.Dir, filenames.VNCPasswordFile)
	err = os.RemoveAll(vncpwdfile)
	if err != nil {
		return err
	}
	return nil
}

func (l *LimaQemuDriver) shutdownQEMU(ctx context.Context, timeout time.Duration, qCmd *exec.Cmd, qWaitCh <-chan error) error {
	logrus.Info("Shutting down QEMU with ACPI")
	if usernetIndex := limayaml.FirstUsernetIndex(l.Yaml); usernetIndex != -1 {
		client := newUsernetClient(l.Yaml.Networks[usernetIndex].Lima)
		err := client.UnExposeSSH(l.SSHLocalPort)
		if err != nil {
			logrus.Warnf("Failed to remove SSH binding for port %d", l.SSHLocalPort)
		}
	}
	qmpSockPath := filepath.Join(l.Instance.Dir, filenames.QMPSock)
	qmpClient, err := qmp.NewSocketMonitor("unix", qmpSockPath, 5*time.Second)
	if err != nil {
		logrus.WithError(err).Warnf("failed to open the QMP socket %q, forcibly killing QEMU", qmpSockPath)
		return l.killQEMU(ctx, timeout, qCmd, qWaitCh)
	}
	if err := qmpClient.Connect(); err != nil {
		logrus.WithError(err).Warnf("failed to connect to the QMP socket %q, forcibly killing QEMU", qmpSockPath)
		return l.killQEMU(ctx, timeout, qCmd, qWaitCh)
	}
	defer func() { _ = qmpClient.Disconnect() }()
	rawClient := raw.NewMonitor(qmpClient)
	logrus.Info("Sending QMP system_powerdown command")
	if err := rawClient.SystemPowerdown(); err != nil {
		logrus.WithError(err).Warnf("failed to send system_powerdown command via the QMP socket %q, forcibly killing QEMU", qmpSockPath)
		return l.killQEMU(ctx, timeout, qCmd, qWaitCh)
	}
	deadline := time.After(timeout)
	select {
	case qWaitErr := <-qWaitCh:
		logrus.WithError(qWaitErr).Info("QEMU has exited")
		l.removeVNCFiles()
		return qWaitErr
	case <-deadline:
	}
	logrus.Warnf("QEMU did not exit in %v, forcibly killing QEMU", timeout)
	return l.killQEMU(ctx, timeout, qCmd, qWaitCh)
}

func (l *LimaQemuDriver) killQEMU(_ context.Context, _ time.Duration, qCmd *exec.Cmd, qWaitCh <-chan error) error {
	var qWaitErr error
	if qCmd.ProcessState == nil {
		if killErr := qCmd.Process.Kill(); killErr != nil {
			logrus.WithError(killErr).Warn("failed to kill QEMU")
		}
		qWaitErr = <-qWaitCh
		logrus.WithError(qWaitErr).Info("QEMU has exited, after killing forcibly")
	} else {
		logrus.Info("QEMU has already exited")
	}
	qemuPIDPath := filepath.Join(l.Instance.Dir, filenames.PIDFile(*l.Yaml.VMType))
	_ = os.RemoveAll(qemuPIDPath)
	l.removeVNCFiles()
	return qWaitErr
}

func newUsernetClient(nwName string) *usernet.Client {
	endpointSock, err := usernet.Sock(nwName, usernet.EndpointSock)
	if err != nil {
		return nil
	}
	return usernet.NewClient(endpointSock)
}

func logPipeRoutine(r io.Reader, header string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		logrus.Debugf("%s: %s", header, line)
	}
}

func (l *LimaQemuDriver) DeleteSnapshot(ctx context.Context, tag string) error {
	qCfg := Config{
		Name:        l.Instance.Name,
		InstanceDir: l.Instance.Dir,
		LimaYAML:    l.Yaml,
	}
	return Del(qCfg, l.Instance.Status == store.StatusRunning, tag)
}

func (l *LimaQemuDriver) CreateSnapshot(ctx context.Context, tag string) error {
	qCfg := Config{
		Name:        l.Instance.Name,
		InstanceDir: l.Instance.Dir,
		LimaYAML:    l.Yaml,
	}
	return Save(qCfg, l.Instance.Status == store.StatusRunning, tag)
}

func (l *LimaQemuDriver) ApplySnapshot(ctx context.Context, tag string) error {
	qCfg := Config{
		Name:        l.Instance.Name,
		InstanceDir: l.Instance.Dir,
		LimaYAML:    l.Yaml,
	}
	return Load(qCfg, l.Instance.Status == store.StatusRunning, tag)
}

func (l *LimaQemuDriver) ListSnapshots(ctx context.Context) (string, error) {
	qCfg := Config{
		Name:        l.Instance.Name,
		InstanceDir: l.Instance.Dir,
		LimaYAML:    l.Yaml,
	}
	return List(qCfg, l.Instance.Status == store.StatusRunning)
}

type qArgTemplateApplier struct {
	files []*os.File
}

func (a *qArgTemplateApplier) applyTemplate(qArg string) (string, error) {
	if !strings.Contains(qArg, "{{") {
		return qArg, nil
	}
	funcMap := template.FuncMap{
		"fd_connect": func(v interface{}) string {
			fn := func(v interface{}) (string, error) {
				s, ok := v.(string)
				if !ok {
					return "", fmt.Errorf("non-string argument %+v", v)
				}
				addr := &net.UnixAddr{
					Net:  "unix",
					Name: s,
				}
				conn, err := net.DialUnix("unix", nil, addr)
				if err != nil {
					return "", err
				}
				f, err := conn.File()
				if err != nil {
					return "", err
				}
				if err := conn.Close(); err != nil {
					return "", err
				}
				a.files = append(a.files, f)
				fd := len(a.files) + 2 // the first FD is 3
				return strconv.Itoa(fd), nil
			}
			res, err := fn(v)
			if err != nil {
				panic(fmt.Errorf("fd_connect: %w", err))
			}
			return res
		},
	}
	tmpl, err := template.New("").Funcs(funcMap).Parse(qArg)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, nil); err != nil {
		return "", err
	}
	return b.String(), nil
}

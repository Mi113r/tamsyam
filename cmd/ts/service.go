package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/kardianos/service"
)

const serviceName = "tamsyam"

// maxLogSize is when the service log is started afresh, so it cannot fill the disk.
const maxLogSize = 10 << 20

const serviceUsage = `Usage: ts service <install|uninstall|start|stop|restart|status> [--config ts.yml] [--user]

Runs ts start as an OS service that comes back after a reboot:
systemd on Linux, launchd on macOS, the Service Control Manager on Windows.
The service reads the config file and the saved login from where they are now,
and logs to ts.log next to the config file.

  --config   config file (default: ./ts.yml, then ts.yml next to the saved login)
  --user     Linux/macOS: install for the current user, without root
             (on Linux run 'loginctl enable-linger' so it starts at boot)

Installing a system service needs root (sudo) or an elevated PowerShell on Windows.
`

func cmdService(args []string) error {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, serviceUsage)
		return errors.New("missing action")
	}
	action := args[0]
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, serviceUsage) }
	cfgPath := fs.String("config", "", "config file")
	user := fs.Bool("user", false, "install for the current user (Linux/macOS)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	svcCfg := &service.Config{
		Name:        serviceName,
		DisplayName: "tamsyam tunnels",
		Description: "Keeps the tunnels from ts.yml up (ts start).",
		Option:      service.KeyValue{"UserService": *user},
	}
	if action == "install" {
		args, err := serviceArgs(*cfgPath)
		if err != nil {
			return err
		}
		svcCfg.Arguments = args
		if *user && runtime.GOOS == "windows" {
			return errors.New("--user is not supported on Windows; run from an elevated PowerShell instead")
		}
	}
	svc, err := service.New(&program{}, svcCfg)
	if err != nil {
		return err
	}

	switch action {
	case "install":
		if err := svc.Install(); err != nil {
			return fmt.Errorf("install: %w (needs root / an elevated shell, or --user)", err)
		}
		if err := svc.Start(); err != nil {
			return fmt.Errorf("installed, but start failed: %w", err)
		}
		fmt.Printf("Service %q installed and started. It runs: ts %v\n", serviceName, svcCfg.Arguments)
		fmt.Printf("Log: %s\n", filepath.Join(filepath.Dir(svcCfg.Arguments[3]), "ts.log"))
	case "status":
		st, err := svc.Status()
		switch {
		case errors.Is(err, service.ErrNotInstalled):
			fmt.Println("not installed")
			return nil
		case err != nil:
			// SysV init scripts report a stopped service as a failed status call.
			fmt.Printf("not running (%v)\n", err)
			return nil
		}
		switch st {
		case service.StatusRunning:
			fmt.Println("running")
		case service.StatusStopped:
			fmt.Println("stopped")
		default:
			fmt.Println("unknown")
		}
	case "uninstall", "start", "stop", "restart":
		if action == "uninstall" {
			_ = svc.Stop()
		}
		if err := service.Control(svc, action); err != nil {
			return err
		}
		fmt.Printf("Service %q: %s done\n", serviceName, action)
	default:
		fmt.Fprint(os.Stderr, serviceUsage)
		return fmt.Errorf("unknown action %q", action)
	}
	return nil
}

// serviceArgs builds the service command line with absolute paths, because the
// service runs from another directory and possibly as another account.
func serviceArgs(cfgPath string) ([]string, error) {
	path, err := findConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	fc, err := loadFileConfig(path)
	if err != nil {
		return nil, err
	}
	args := []string{"start", "--service", "--config", path, "-q"}
	if fc.Server == "" || fc.Token == "" {
		login, err := configPath()
		if err != nil {
			return nil, err
		}
		if _, err := resolveLogin(config{Server: fc.Server, Token: fc.Token}); err != nil {
			return nil, err
		}
		args = append(args, "--login-file", login)
	}
	return args, nil
}

// program adapts ts start to the service manager.
type program struct {
	run    func(context.Context) error
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		if err := p.run(ctx); err != nil {
			fmt.Fprintln(out, "error:", err)
		}
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// runService runs ts start under the service manager, logging next to the config.
func runService(cfgPath string, run func(context.Context) error) error {
	logPath := filepath.Join(filepath.Dir(cfgPath), "ts.log")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(logPath, logPath+".1")
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer f.Close()
		// The file goes first: on Windows a service has no usable stdout.
		out = io.MultiWriter(f, os.Stdout)
		fmt.Fprintf(f, "\n--- %s service start\n", time.Now().Format(time.RFC3339))
	}
	svc, err := service.New(&program{run: run}, &service.Config{Name: serviceName})
	if err != nil {
		return err
	}
	return svc.Run()
}

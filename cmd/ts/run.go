package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Mi113r/tamsyam/internal/client"
	"github.com/Mi113r/tamsyam/internal/protocol"
)

// runTunnels keeps every spec's tunnel up until ctx ends. Each tunnel has its
// own connection, so one rejected tunnel does not take the others down.
func runTunnels(ctx context.Context, login config, specs []tunnelSpec, quiet bool) error {
	multi := len(specs) > 1
	width := 0
	for _, sp := range specs {
		width = max(width, len(sp.Name))
	}
	var printMu sync.Mutex
	printf := func(name, format string, args ...any) {
		printMu.Lock()
		defer printMu.Unlock()
		if multi {
			fmt.Fprintf(out, "%-*s  ", width, name)
		}
		fmt.Fprintf(out, format, args...)
	}

	fmt.Fprintf(out, "tamsyam %s · connecting to %s\n", protocol.Version, login.Server)
	var (
		wg   sync.WaitGroup
		errs = make([]error, len(specs))
		sums = make([]summary, len(specs))
	)
	for i, sp := range specs {
		target, local, cleanup, err := resolveTarget(sp)
		if err != nil {
			errs[i] = err
			continue
		}
		defer cleanup()

		opts := client.Options{
			Server:    login.Server,
			Token:     login.Token,
			Proto:     sp.Proto,
			Subdomain: sp.Subdomain,
			Port:      sp.Port,
			Allow:     sp.Allow.String(),
			Auth:      sp.Auth,
			Target:    target,
			OnConnect: func(url string) {
				if multi {
					printf(sp.Name, "%s  ->  %s\n", url, local)
					return
				}
				printf("", "\n  Forwarding  %s  ->  %s\n", url, local)
				if sp.Allow.String() != "" {
					printf("", "  Allowed     %s\n", sp.Allow)
				}
				if sp.Auth != "" {
					user, _, _ := strings.Cut(sp.Auth, ":")
					printf("", "  Auth        %s:***\n", user)
				}
				printf("", "\n  Ctrl+C to stop\n\n")
			},
			OnDisconnect: func(err error, retry time.Duration) {
				printf(sp.Name, "connection lost (%v), reconnecting in %s...\n", err, retry)
			},
			OnEvent: func(ev protocol.Event) {
				sums[i].add(ev)
				if !quiet {
					printf(sp.Name, "%s\n", formatEvent(ev))
				}
			},
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.Run(ctx, opts); err != nil {
				errs[i] = err
				if multi {
					printf(sp.Name, "error: %v\n", err)
				}
			}
		}()
	}
	wg.Wait()

	for i := range sums {
		if s := sums[i].String(); s != "" {
			if multi {
				s = specs[i].Name + ":\n" + s
			}
			fmt.Fprintln(out, "\n"+s)
		}
	}
	if !multi {
		return errs[0]
	}
	var all []error
	for i, err := range errs {
		if err != nil {
			all = append(all, fmt.Errorf("%s: %w", specs[i].Name, err))
		}
	}
	return errors.Join(all...)
}

// resolveTarget turns a spec's addr into a host:port to dial and a label to
// show. A directory is served as static files on a loopback port.
func resolveTarget(sp tunnelSpec) (target, local string, cleanup func(), err error) {
	addr := sp.Addr
	if addr == "" {
		return "", "", nil, errors.New("addr is required (port, host:port or a directory)")
	}
	if sp.Proto == "http" && isDirArg(addr) {
		target, stop, err := client.ServeDir(addr)
		if err != nil {
			return "", "", nil, err
		}
		return target, "files in " + addr, stop, nil
	}
	if !strings.Contains(addr, ":") {
		addr = "localhost:" + addr
	}
	local = addr
	if sp.Proto == "http" {
		local = "http://" + addr
	}
	return addr, local, func() {}, nil
}

// isDirArg reports whether addr names a directory rather than a port.
func isDirArg(addr string) bool {
	if strings.Trim(addr, "0123456789") == "" {
		return false
	}
	fi, err := os.Stat(addr)
	return err == nil && fi.IsDir()
}

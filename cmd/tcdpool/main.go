// Command tcdpool runs a pool of nostar tcd transcoder processes behind
// an authenticated WebSocket, and manages its access keys.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dbehnke/tcdpool/internal/backend/nostar"
	"github.com/dbehnke/tcdpool/internal/config"
	"github.com/dbehnke/tcdpool/internal/keys"
	"github.com/dbehnke/tcdpool/internal/pool"
	"github.com/dbehnke/tcdpool/internal/server"
)

const usage = `usage:
  tcdpool serve [-config FILE]
  tcdpool key create [-keys FILE] -label TEXT [-max-processes N]
  tcdpool key list [-keys FILE]
  tcdpool key disable [-keys FILE] ID
  tcdpool key enable [-keys FILE] ID
  tcdpool stats [-admin ADDR]
  tcdpool check-config [-config FILE]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "key":
		err = key(os.Args[2:], os.Stdout)
	case "stats":
		err = stats(os.Args[2:], os.Stdout)
	case "check-config":
		fs := flag.NewFlagSet("check-config", flag.ExitOnError)
		path := fs.String("config", "tcdpool.json", "config file")
		fs.Parse(os.Args[2:])
		if _, err = config.Load(*path); err == nil {
			fmt.Println("config is good")
		}
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tcdpool:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "tcdpool.json", "config file")
	fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	logf := log.Printf

	ks, err := keys.Open(cfg.KeysFile)
	if err != nil {
		return fmt.Errorf("keys file: %w (make a key with: tcdpool key create)", err)
	}
	nb, err := nostar.New(nostar.Options{
		Binary:         cfg.Backend.Binary,
		WorkDir:        cfg.Backend.WorkDir,
		PortMin:        cfg.Backend.PortMin,
		PortMax:        cfg.Backend.PortMax,
		Framing:        cfg.Backend.Framing,
		Gains:          cfg.Backend.Gains,
		ConnectTimeout: cfg.Backend.ConnectTimeout.D(),
		Watchdog:       cfg.Backend.Watchdog.D(),
		Logf:           logf,
	})
	if err != nil {
		return err
	}
	p := pool.New(pool.Options{
		Backend:      nb,
		IdleTimeout:  cfg.IdleTimeout.D(),
		MaxProcesses: cfg.MaxProcesses,
		LateAfter:    cfg.LateAfter.D(),
		MissingAfter: cfg.MissingAfter.D(),
		MinBackoff:   cfg.Restart.MinBackoff.D(),
		MaxBackoff:   cfg.Restart.MaxBackoff.D(),
		MaxFailures:  cfg.Restart.MaxFailures,
		Logf:         logf,
	})
	srv := server.New(server.Options{
		Pool:            p,
		Keys:            ks,
		Backend:         nb,
		KeyMaxProcesses: cfg.KeyMaxProcesses,
		MaxProcesses:    cfg.MaxProcesses,
		Version:         buildVersion(),
		IdleTimeout:     cfg.IdleTimeout.D(),
		LateAfter:       cfg.LateAfter.D(),
		Logf:            logf,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.WatchKeys(ctx, 2*time.Second)

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.Default()}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	scheme := "wss"
	switch {
	case cfg.TLS.Disable:
		scheme = "ws"
		logf("tls is off: use this only behind a TLS proxy")
	case cfg.TLS.Cert != "":
		cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return err
		}
		hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	default:
		cert, fp, err := server.SelfSigned()
		if err != nil {
			return err
		}
		hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		logf("tls: self-signed certificate for development, sha256 %s", fp)
	}
	if hs.TLSConfig != nil {
		ln = tls.NewListener(ln, hs.TLSConfig)
	}

	var admin *http.Server
	if cfg.AdminListen != "" {
		admin = &http.Server{Addr: cfg.AdminListen, Handler: srv.AdminHandler(cfg.MaxProcesses), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("admin: %v", err)
			}
		}()
		logf("admin on http://%s (/stats, /metrics)", cfg.AdminListen)
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	logf("tcdpool on %s://%s/v1/stream, backend %s (%s framing, %s), max %d processes, idle %s",
		scheme, cfg.Listen, nb.Name(), cfg.Backend.Framing, cfg.Backend.Binary, cfg.MaxProcesses, cfg.IdleTimeout.D())

	select {
	case <-ctx.Done():
		logf("stopping")
	case err := <-errc:
		p.Close()
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.Close() // tells the clients shutting_down and stops tcd
	hs.Shutdown(sctx)
	if admin != nil {
		admin.Shutdown(sctx)
	}
	return nil
}

func key(args []string, out io.Writer) error {
	if len(args) < 1 {
		return errors.New("key: give create, list, disable or enable")
	}
	fs := flag.NewFlagSet("key "+args[0], flag.ExitOnError)
	path := fs.String("keys", "keys.json", "keys file")
	label := fs.String("label", "", "label for the key (create)")
	maxp := fs.Int("max-processes", 0, "most processes for this key; 0 = server default (create)")
	fs.Parse(args[1:])
	switch args[0] {
	case "create":
		if *label == "" {
			return errors.New("key create: give -label")
		}
		k, token, err := keys.Create(*path, *label, *maxp)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "key %s created (%s)\n", k.ID, k.Label)
		fmt.Fprintf(out, "token (shown one time only, keep it secret):\n%s\n", token)
		fmt.Fprintf(out, "the client sends: Authorization: Bearer <token>\n")
		return nil
	case "list":
		list, err := keys.List(*path)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tENABLED\tMAX\tCREATED\tLABEL")
		for _, k := range list {
			m := "default"
			if k.MaxProcesses > 0 {
				m = fmt.Sprint(k.MaxProcesses)
			}
			fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\n", k.ID, k.Enabled, m, k.Created.UTC().Format(time.RFC3339), k.Label)
		}
		return tw.Flush()
	case "disable", "enable":
		if fs.NArg() != 1 {
			return fmt.Errorf("key %s: give one key ID", args[0])
		}
		if err := keys.SetEnabled(*path, fs.Arg(0), args[0] == "enable"); err != nil {
			return err
		}
		fmt.Fprintf(out, "key %s %sd (a running server reads the change in about 2 s)\n", fs.Arg(0), args[0])
		return nil
	}
	return fmt.Errorf("key: unknown command %q", args[0])
}

func stats(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	addr := fs.String("admin", "127.0.0.1:9180", "admin address")
	fs.Parse(args)
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + *addr + "/stats")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("admin: %s", resp.Status)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

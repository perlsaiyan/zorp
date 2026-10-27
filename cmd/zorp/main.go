// Command zorp is a terminal mail client for Microsoft 365.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tom/zorp/config"
	"github.com/tom/zorp/internal/auth"
	"github.com/tom/zorp/internal/graph"
)

const usage = `usage: zorp <command> [flags]

commands:
  login [--device | --browser]   sign in (default: device code, falling back to browser)
  logout                         forget the cached sign-in
  whoami                         show the signed-in user (calls Graph /me)
  probe                          list the 10 newest Inbox messages (auth go/no-go check)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "login":
		err = runLogin(ctx, args)
	case "logout":
		err = runLogout(ctx)
	case "whoami":
		err = runWhoami(ctx)
	case "probe":
		err = runProbe(ctx)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "zorp: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "zorp %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func newAuth() (*config.Config, *auth.Authenticator, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	a, err := auth.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cfg, a, nil
}

func runLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	device := fs.Bool("device", false, "use device code flow only")
	browser := fs.Bool("browser", false, "use browser (auth code + PKCE) flow only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mode := auth.ModeAuto
	switch {
	case *device && *browser:
		return errors.New("--device and --browser are mutually exclusive")
	case *device:
		mode = auth.ModeDevice
	case *browser:
		mode = auth.ModeBrowser
	}

	_, a, err := newAuth()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	acct, err := a.Login(ctx, mode, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("\nSigned in as %s\n", acct.PreferredUsername)
	return nil
}

func runLogout(ctx context.Context) error {
	_, a, err := newAuth()
	if err != nil {
		return err
	}
	if err := a.Logout(ctx); err != nil {
		return err
	}
	fmt.Println("Signed out.")
	return nil
}

func runWhoami(ctx context.Context) error {
	_, a, err := newAuth()
	if err != nil {
		return err
	}
	me, err := graph.New(a).Me(ctx)
	if err != nil {
		return err
	}
	addr := me.Mail
	if addr == "" {
		addr = me.UserPrincipalName
	}
	fmt.Printf("%s <%s>\n", me.DisplayName, addr)
	return nil
}

func runProbe(ctx context.Context) error {
	_, a, err := newAuth()
	if err != nil {
		return err
	}
	g := graph.New(a)

	fmt.Print("1. Cached sign-in ............ ")
	acct, err := a.Account(ctx)
	if err != nil {
		fmt.Println("FAIL")
		return err
	}
	fmt.Println("ok:", acct.PreferredUsername)

	fmt.Print("2. Graph /me (User.Read) ..... ")
	me, err := g.Me(ctx)
	if err != nil {
		fmt.Println("FAIL")
		return err
	}
	fmt.Println("ok:", me.DisplayName)

	fmt.Print("3. Inbox (Mail.Read) ......... ")
	msgs, err := g.RecentInbox(ctx, 10)
	if err != nil {
		fmt.Println("FAIL")
		return err
	}
	fmt.Printf("ok: %d messages\n\n", len(msgs))

	for _, m := range msgs {
		mark := " "
		if !m.IsRead {
			mark = "N"
		}
		from := "(unknown sender)"
		if m.From != nil {
			from = m.From.EmailAddress.Name
			if from == "" {
				from = m.From.EmailAddress.Address
			}
		}
		fmt.Printf("  %s %s  %-24s  %s\n", mark, m.ReceivedDateTime.Local().Format("Jan 02 15:04"), truncate(from, 24), truncate(m.Subject, 60))
	}
	fmt.Println("\nAuth go/no-go: GO")
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

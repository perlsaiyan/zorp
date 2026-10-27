// Command zorp is a terminal mail client for Microsoft 365.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/config"
	"github.com/perlsaiyan/zorp/internal/auth"
	"github.com/perlsaiyan/zorp/internal/daemon"
	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/ipc"
	"github.com/perlsaiyan/zorp/internal/mailsync"
	"github.com/perlsaiyan/zorp/internal/store"
	"github.com/perlsaiyan/zorp/internal/tui"
)

const usage = `usage: zorp [command] [flags]

commands:
  (none), tui                    open the mail client
  login [--device | --browser]   sign in (default: device code, falling back to browser)
  logout                         forget the cached sign-in
  whoami                         show the signed-in user (calls Graph /me)
  probe                          list the 10 newest Inbox messages (auth go/no-go check)
  sync [--full]                  sync now via the daemon (starting it if needed); --full rebuilds the cache
  daemon start                   start the sync daemon in the background (zorp and zorp sync do this for you)
  daemon status | stop           show the running daemon's state, or stop it
  daemon                         run the sync daemon in the foreground (for systemd or debugging)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := "tui", []string(nil)
	if len(os.Args) > 1 {
		cmd, args = os.Args[1], os.Args[2:]
	}
	var err error
	switch cmd {
	case "tui":
		err = runTUI(ctx)
	case "login":
		err = runLogin(ctx, args)
	case "logout":
		err = runLogout(ctx)
	case "whoami":
		err = runWhoami(ctx)
	case "probe":
		err = runProbe(ctx)
	case "sync":
		err = runSync(ctx, args)
	case "daemon":
		err = runDaemon(ctx, args)
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

	cfg, a, err := newAuth()
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
	notifyDaemonAuth(ctx, cfg)
	return nil
}

// notifyDaemonAuth tells a running daemon that the token cache changed.
// A daemon that isn't running will pick it up when it starts.
func notifyDaemonAuth(ctx context.Context, cfg *config.Config) {
	c, err := daemon.Connect(ctx, cfg, false)
	if err != nil {
		return
	}
	defer c.Close()
	if err := c.Call(ctx, ipc.MethodReloadAuth, nil, nil); err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't notify the daemon: %v\n", err)
	}
}

func runLogout(ctx context.Context) error {
	cfg, a, err := newAuth()
	if err != nil {
		return err
	}
	if err := a.Logout(ctx); err != nil {
		return err
	}
	fmt.Println("Signed out.")
	notifyDaemonAuth(ctx, cfg)
	return nil
}

func runTUI(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	st, account, err := openCache(ctx, cfg)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		// First run: fill the cache before opening the UI.
		fmt.Println("No local mail cache yet; running the first sync (this can take a minute)...")
		if err := runSync(ctx, nil); err != nil {
			return err
		}
		st, account, err = openCache(ctx, cfg)
	}
	if err != nil {
		return err
	}
	defer st.Close()

	m, err := tui.New(ctx, cfg, st, account)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil // interrupted by a signal
	}
	return err
}

// openCache opens the local cache read-only and returns its account.
func openCache(ctx context.Context, cfg *config.Config) (*store.Store, store.Account, error) {
	if _, err := os.Stat(cfg.Paths.DB); err != nil {
		return nil, store.Account{}, err
	}
	st, err := store.OpenReadOnly(cfg.Paths.DB)
	if err != nil {
		return nil, store.Account{}, err
	}
	account, err := st.DefaultAccount(ctx)
	if err != nil {
		st.Close()
		return nil, store.Account{}, err
	}
	return st, account, nil
}

func runSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	full := fs.Bool("full", false, "discard the local cache and resync from scratch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	c, err := daemon.Connect(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer c.Close()

	fmt.Println("Syncing...")
	var res mailsync.Result
	if err := c.Call(ctx, ipc.MethodSync, ipc.SyncParams{Full: *full}, &res); err != nil {
		return err
	}
	fmt.Printf("%d folders in mailbox, synced in %s\n\n", res.Folders, res.Duration.Round(time.Millisecond))
	for _, fr := range res.FolderResults {
		kind := "incremental"
		if fr.Initial {
			kind = "initial"
		}
		fmt.Printf("  %-20s %-11s  +%d/-%d  →  %d cached, %d unread\n",
			fr.Folder, kind, fr.Upserted, fr.Removed, fr.Total, fr.Unread)
	}
	return nil
}

func runDaemon(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "":
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		return daemon.Run(ctx, cfg, log)
	case "start":
		c, err := daemon.Connect(ctx, cfg, true)
		if err != nil {
			return err
		}
		defer c.Close()
		var st ipc.Status
		if err := c.Call(ctx, ipc.MethodStatus, nil, &st); err != nil {
			return err
		}
		fmt.Printf("Daemon running (pid %d), logging to %s\n", st.PID, cfg.Paths.DaemonLog)
		return nil
	case "status", "stop":
		c, err := daemon.Connect(ctx, cfg, false)
		if err != nil {
			return err
		}
		defer c.Close()
		if sub == "stop" {
			var st ipc.Status
			if err := c.Call(ctx, ipc.MethodStatus, nil, &st); err != nil {
				return err
			}
			if err := c.Call(ctx, ipc.MethodShutdown, nil, nil); err != nil {
				return err
			}
			// Wait for the process to exit, not just the socket to close:
			// the daemon releases its instance lock last, after closing the
			// database, and a new daemon can't start until then.
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if err := syscall.Kill(st.PID, 0); errors.Is(err, syscall.ESRCH) {
					fmt.Println("Daemon stopped.")
					return nil
				}
			}
			return errors.New("daemon acknowledged shutdown but is still running")
		}
		var st ipc.Status
		if err := c.Call(ctx, ipc.MethodStatus, nil, &st); err != nil {
			return err
		}
		fmt.Printf("state:     %s\npid:       %d\n", st.State, st.PID)
		if st.LastSync != 0 {
			fmt.Printf("last sync: %s\n", time.Unix(st.LastSync, 0).Format(time.DateTime))
		}
		if st.LastError != "" {
			fmt.Printf("error:     %s\n", st.LastError)
		}
		return nil
	}
	return fmt.Errorf("unknown daemon subcommand %q", sub)
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
	fmt.Printf("ok: %d messages\n", len(msgs))

	fmt.Print("4. Write access (Mail.ReadWrite) ")
	scopes, err := a.GrantedScopes(ctx)
	if err != nil {
		fmt.Println("FAIL")
		return err
	}
	if !hasScope(scopes, "Mail.ReadWrite") {
		fmt.Println("MISSING")
		return fmt.Errorf("token lacks Mail.ReadWrite (granted: %s); add it to the app registration and grant consent", strings.Join(scopes, " "))
	}
	fmt.Println("ok")
	fmt.Println()

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

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		// Entra may report scopes with or without the resource prefix.
		if strings.EqualFold(s, want) || strings.HasSuffix(strings.ToLower(s), "/"+strings.ToLower(want)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

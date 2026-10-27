// Package auth signs the user in to Microsoft Entra ID with MSAL and hands out
// Graph access tokens from a persistent cache.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"

	"github.com/perlsaiyan/zorp/config"
)

// Scopes requested so far. Kept to what shipped features need, so each
// consent request in the work tenant is as small as possible. Mail.Send is
// added when sending lands. MSAL adds openid, profile and offline_access itself.
var Scopes = []string{"User.Read", "Mail.Read", "Mail.ReadWrite"}

// ErrNeedsLogin means there is no usable cached sign-in; run `zorp login`.
var ErrNeedsLogin = errors.New("not signed in (run `zorp login`)")

type Mode int

const (
	// ModeAuto tries device code first and falls back to the browser.
	ModeAuto Mode = iota
	ModeDevice
	ModeBrowser
)

type Authenticator struct {
	client public.Client
	cache  *fileCache
}

func New(cfg *config.Config) (*Authenticator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return nil, err
	}
	fc := &fileCache{path: cfg.Paths.TokenCache}
	client, err := public.New(cfg.Auth.ClientID,
		public.WithAuthority(cfg.Authority()),
		public.WithCache(fc),
	)
	if err != nil {
		return nil, fmt.Errorf("creating MSAL client: %w", err)
	}
	return &Authenticator{client: client, cache: fc}, nil
}

// Login runs an interactive sign-in, writing prompts for the user to out.
func (a *Authenticator) Login(ctx context.Context, mode Mode, out io.Writer) (public.Account, error) {
	switch mode {
	case ModeDevice:
		return a.loginDevice(ctx, out)
	case ModeBrowser:
		return a.loginBrowser(ctx, out)
	}
	acct, err := a.loginDevice(ctx, out)
	if err == nil || ctx.Err() != nil {
		return acct, err
	}
	fmt.Fprintf(out, "\nDevice code sign-in failed: %v\nFalling back to browser sign-in.\n\n", err)
	return a.loginBrowser(ctx, out)
}

func (a *Authenticator) loginDevice(ctx context.Context, out io.Writer) (public.Account, error) {
	dc, err := a.client.AcquireTokenByDeviceCode(ctx, Scopes)
	if err != nil {
		return public.Account{}, Explain(err)
	}
	fmt.Fprintln(out, dc.Result.Message)
	res, err := dc.AuthenticationResult(ctx)
	if err != nil {
		return public.Account{}, Explain(err)
	}
	return res.Account, nil
}

func (a *Authenticator) loginBrowser(ctx context.Context, out io.Writer) (public.Account, error) {
	res, err := a.client.AcquireTokenInteractive(ctx, Scopes,
		public.WithRedirectURI("http://localhost"),
		public.WithOpenURL(func(url string) error {
			fmt.Fprintf(out, "Opening your browser to sign in. If it doesn't open, visit:\n\n  %s\n\n", url)
			// Ignore failures: the URL is printed above for manual use.
			_ = exec.Command("xdg-open", url).Start()
			return nil
		}),
	)
	if err != nil {
		return public.Account{}, Explain(err)
	}
	return res.Account, nil
}

// Account returns the cached signed-in account, or ErrNeedsLogin.
func (a *Authenticator) Account(ctx context.Context) (public.Account, error) {
	accts, err := a.client.Accounts(ctx)
	if err != nil {
		return public.Account{}, err
	}
	if len(accts) == 0 {
		return public.Account{}, ErrNeedsLogin
	}
	return accts[0], nil
}

// Token returns a Graph access token, refreshing silently if needed. It
// never prompts; if interaction is required it returns ErrNeedsLogin.
func (a *Authenticator) Token(ctx context.Context) (string, error) {
	acct, err := a.Account(ctx)
	if err != nil {
		return "", err
	}
	res, err := a.client.AcquireTokenSilent(ctx, Scopes, public.WithSilentAccount(acct))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %v", ErrNeedsLogin, Explain(err))
	}
	return res.AccessToken, nil
}

// GrantedScopes returns the scopes on a current access token, as Entra
// reports them (e.g. "Mail.ReadWrite").
func (a *Authenticator) GrantedScopes(ctx context.Context) ([]string, error) {
	acct, err := a.Account(ctx)
	if err != nil {
		return nil, err
	}
	res, err := a.client.AcquireTokenSilent(ctx, Scopes, public.WithSilentAccount(acct))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNeedsLogin, Explain(err))
	}
	return res.GrantedScopes, nil
}

// Logout forgets all cached accounts and deletes the token cache.
func (a *Authenticator) Logout(ctx context.Context) error {
	accts, err := a.client.Accounts(ctx)
	if err != nil {
		return err
	}
	for _, acct := range accts {
		if err := a.client.RemoveAccount(ctx, acct); err != nil {
			return err
		}
	}
	return a.cache.clear()
}

var aadstsRe = regexp.MustCompile(`AADSTS\d+`)

// hints explains the Entra errors most likely when a personal app registration
// signs in to a locked-down work tenant.
var hints = map[string]string{
	"AADSTS65001":   "consent has not been granted for this app; an admin may need to approve it (see README: Admin consent)",
	"AADSTS90094":   "your organization requires an admin to approve this app (see README: Admin consent)",
	"AADSTS90095":   "your organization requires an admin to approve this app (see README: Admin consent)",
	"AADSTS53003":   "blocked by a Conditional Access policy; try `zorp login --browser`, or ask IT",
	"AADSTS50105":   "your account is not assigned to this app in your organization; ask IT",
	"AADSTS700016":  "app not found; check auth.client_id and that the registration is multi-tenant",
	"AADSTS7000218": "public client flows are disabled; set \"Allow public client flows\" to Yes in the app registration",
	"AADSTS50011":   "redirect URI mismatch; add http://localhost as a Public client/native redirect URI",
}

// Explain adds a human-readable hint to known Entra error codes.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	code := aadstsRe.FindString(err.Error())
	if hint, ok := hints[code]; ok {
		return fmt.Errorf("%s: %w", hint, err)
	}
	return err
}

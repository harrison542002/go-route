package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/clitime"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/drivers/postgresql"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/admin"
)

const credentialOpTimeout = 15 * time.Second

func createTokenCmd() *cobra.Command {
	var name, role, expiresIn string

	cmd := &cobra.Command{
		Use:   "create-token",
		Short: "Mint an admin credential",
		Long: `Prints the new token once. It is stored only as a SHA-256, so
there is no way to show it again: if it is lost, revoke the credential
and mint another.

--role says what the credential may do. admin may do everything, which
is what every credential could do before roles existed and is therefore
the default, so existing scripts keep working unchanged. readonly may
only read -- GET and HEAD -- which is what a dashboard viewer who must
not change a quota should hold.

--expires-in mints a credential that stops working after a while, in the
same forms --since takes elsewhere: 90d, 24h, 90m. It is checked when
the token is presented, so an expired credential is refused immediately
and everywhere, and its row stays where it is -- the audit rows it wrote
name it. Without the flag the credential does not expire.`,
		Args: cobra.NoArgs,
		Example: `  go-route-admin create-token --name billing-sync
  go-route-admin create-token --name dashboard-viewer --role readonly
  go-route-admin create-token --name quarterly-audit --role readonly --expires-in 90d`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := credentialSpec(name, role, expiresIn, time.Now())
			if err != nil {
				return err
			}
			return withCredentials(cmd.Context(), func(ctx context.Context, creds *admin.Credentials) error {
				issued, err := creds.Create(ctx, ports.Write{Actor: cliActor()}, spec)
				if err != nil {
					return err
				}
				printIssuedCredential(os.Stdout, issued)
				return nil
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&name, "name", "", "who this credential is; appears in the audit log as admin:<name>")
	f.StringVar(&role, "role", string(domains.RoleAdmin), "admin (everything) or readonly (GET and HEAD only)")
	f.StringVar(&expiresIn, "expires-in", "", "lifetime, as 90d, 24h or 90m (default: never expires)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// credentialSpec turns the flags into what the use case mints, before any
// database connection is opened.
func credentialSpec(name, role, expiresIn string, now time.Time) (ports.NewAdminCredential, error) {
	spec := ports.NewAdminCredential{Name: name, Role: domains.Role(role)}
	if !spec.Role.Valid() {
		return spec, fmt.Errorf("--role: %q is neither admin nor readonly", role)
	}
	if expiresIn == "" {
		return spec, nil
	}

	d, err := clitime.ParseDuration(expiresIn)
	if err != nil {
		return spec, fmt.Errorf("--expires-in: %w", err)
	}
	if d <= 0 {
		return spec, fmt.Errorf("--expires-in: %s is not a lifetime; leave the flag off for a credential that never expires", d)
	}

	at := now.Add(d)
	spec.ExpiresAt = &at
	return spec, nil
}

func listTokensCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "list-tokens",
		Short: "List admin credentials",
		Long: `Revoked and expired credentials are listed too: they are what old
audit rows name. Expired and revoked are shown in separate columns
because they are different facts -- one ran out, the other was taken
away -- even though the API refuses both with the same 401.`,
		Args:    cobra.NoArgs,
		Example: "  go-route-admin list-tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCredentials(cmd.Context(), func(ctx context.Context, creds *admin.Credentials) error {
				list, err := creds.List(ctx)
				if err != nil {
					return err
				}
				if jsonOut {
					return printJSON(list)
				}
				printCredentials(os.Stdout, list)
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the rows as JSON")
	return cmd
}

func revokeTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke-token <name-or-id>",
		Short: "Revoke an admin credential",
		Long: `The row stays, so the audit rows this credential wrote remain
attributable to the name that wrote them. The token stops working on the
next request: authentication reads the row every time, so there is no
cache anywhere still honouring it.`,
		Args:    cobra.ExactArgs(1),
		Example: "  go-route-admin revoke-token billing-sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCredentials(cmd.Context(), func(ctx context.Context, creds *admin.Credentials) error {
				cred, err := creds.Revoke(ctx, ports.Write{Actor: cliActor()}, args[0])
				if err != nil {
					return err
				}
				fmt.Println(revokedLine(cred))
				return nil
			})
		},
	}
}

// withCredentials resolves the DSN, opens a pool for the one command, and hands
// over the use case.
func withCredentials(ctx context.Context, fn func(context.Context, *admin.Credentials) error) error {
	dsn, err := resolveDSN()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, credentialOpTimeout)
	defer cancel()

	pool, err := postgresql.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Minting the first credential happens before any service has run, so the
	// CLI has to make sure its audit row has a partition to land in.
	if err := postgresql.NewPartitions(pool).Ensure(ctx); err != nil {
		return err
	}

	return fn(ctx, admin.NewCredentials(repositories.NewAdminCredentialRepo(pool), time.Now, nil))
}

// cliActor is what a credential mutation made from the CLI is audited as. It
// must not be admin:<name>: nobody presented a credential to run this.
func cliActor() string {
	u, err := user.Current()
	if err != nil {
		return "cli"
	}
	return osUserActor(u.Username)
}

func osUserActor(username string) string {
	if username == "" {
		return "cli"
	}
	return "cli:" + actorSafe.ReplaceAllString(username, "_")
}

// actorSafe folds away what a Windows domain prefix or an unusual login would
// otherwise put into audit_log.actor.
var actorSafe = regexp.MustCompile(`[^A-Za-z0-9._@+-]`)

// printIssuedCredential is the one and only time the secret is shown.
func printIssuedCredential(out io.Writer, issued ports.IssuedAdminCredential) {
	cred := issued.Credential
	fmt.Fprintf(out, "\n  %s\n", cred.Name)
	fmt.Fprintf(out, "  id %s\n", cred.ID)
	// Role and expiry are printed back because both have a default, and this
	// output exists to catch one minted with more power than was meant.
	fmt.Fprintf(out, "  role %s (%s)\n", cred.Role, roleSummary(cred.Role))
	fmt.Fprintf(out, "  expires %s\n\n", expiryPhrase(cred.ExpiresAt))
	fmt.Fprintf(out, "  %s\n\n", issued.Secret)
	fmt.Fprint(out, "  This token is shown once and cannot be shown again: only its\n")
	fmt.Fprint(out, "  SHA-256 is stored. Lose it and you revoke this credential and\n")
	fmt.Fprint(out, "  mint another.\n\n")
	fmt.Fprint(out, "  Use it as:  Authorization: Bearer <token>\n\n")
}

func printCredentials(out io.Writer, list []domains.AdminCredential) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no admin credentials; mint one with: go-route-admin create-token --name <name>")
		return
	}

	now := time.Now()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTOKEN\tROLE\tCREATED\tLAST USED\tEXPIRES\tREVOKED")
	for _, c := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Name, display(c.Prefix), c.Role, c.CreatedAt.UTC().Format(time.DateOnly),
			whenOrNever(c.LastUsedAt), expiryCell(c, now), whenOrNever(c.RevokedAt))
	}
	_ = w.Flush()
}

func roleSummary(r domains.Role) string {
	if r == domains.RoleReadonly {
		return "reads only; any change is refused with 403"
	}
	return "the whole API"
}

func expiryPhrase(at *time.Time) string {
	if at == nil {
		return "never"
	}
	return at.UTC().Format("2006-01-02 15:04") + " UTC"
}

// expiryCell marks a credential whose time has run out, rather than printing a
// date and leaving the reader to do the arithmetic.
func expiryCell(c domains.AdminCredential, now time.Time) string {
	cell := whenOrNever(c.ExpiresAt)
	if c.Expired(now) {
		cell += " (expired)"
	}
	return cell
}

func revokedLine(c domains.AdminCredential) string {
	return fmt.Sprintf("revoked %s (%s), last used %s", c.Name, display(c.Prefix), whenOrNever(c.LastUsedAt))
}

// display marks a token prefix as the head of something longer, so it is never
// mistaken for a token that could be presented.
func display(prefix string) string { return prefix + "…" }

func whenOrNever(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/drivers/postgresql"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/admin"
)

func createUserCmd() *cobra.Command {
	var email, role, plain string

	cmd := &cobra.Command{
		Use:   "create-user",
		Short: "Create a person who can log in",
		Long: `Prints the password once. It is stored only as an argon2id hash,
so there is no way to show it again: if it is lost, set another with
set-password.

Without --password a strong one is generated, which is the way to use
this. --password exists for a deployment that pipes the account into
something else; there is no interactive prompt, because a command that
blocks on a terminal is a command no provisioning script can run.

A password given that way must satisfy the policy: 12 to 128 characters,
not entirely whitespace, and not containing the address it belongs to or
the name of this product.

--role says what the person may do, with the same two values a machine
credential carries. admin may do everything; readonly may only read --
GET and HEAD -- which is what a dashboard viewer who must not change a
quota should hold.`,
		Args: cobra.NoArgs,
		Example: `  go-route-admin create-user --email ops@example.com --role admin
  go-route-admin create-user --email viewer@example.com --role readonly`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !domains.Role(role).Valid() {
				return fmt.Errorf("--role: %q is neither admin nor readonly", role)
			}
			return withUsers(cmd.Context(), func(ctx context.Context, users *admin.Users) error {
				created, err := users.Create(ctx, ports.Write{Actor: cliActor()}, ports.NewAdminUser{
					Email: email, Role: domains.Role(role), Password: plain,
				})
				if err != nil {
					return err
				}
				printCreatedUser(os.Stdout, created)
				return nil
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&email, "email", "", "the person's address; appears in the audit log as user:<email>")
	f.StringVar(&role, "role", string(domains.RoleAdmin), "admin (everything) or readonly (GET and HEAD only)")
	f.StringVar(&plain, "password", "", "set this password instead of generating one (12-128 characters)")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

func listUsersCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "list-users",
		Short: "List the people who can log in",
		Long: `Disabled people are listed too: they are what old audit rows
name. Nobody is ever deleted.`,
		Args:    cobra.NoArgs,
		Example: "  go-route-admin list-users",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withUsers(cmd.Context(), func(ctx context.Context, users *admin.Users) error {
				list, err := users.List(ctx)
				if err != nil {
					return err
				}
				if jsonOut {
					return printJSON(list)
				}
				printUsers(os.Stdout, list)
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the rows as JSON")
	return cmd
}

func disableUserCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable-user <email>",
		Short: "Lock a person out",
		Long: `The row stays, so the audit rows this person wrote remain
attributable to the address that wrote them. Every refresh token they
hold is revoked, and their access token stops working on their next
request: the user row is read on every call, so there is no window in
which a disabled person is still served.`,
		Args:    cobra.ExactArgs(1),
		Example: "  go-route-admin disable-user ops@example.com",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withUsers(cmd.Context(), func(ctx context.Context, users *admin.Users) error {
				user, err := users.Disable(ctx, ports.Write{Actor: cliActor()}, args[0])
				if err != nil {
					return err
				}
				fmt.Printf("disabled %s (%s), last login %s\n",
					user.Email, user.Role, whenOrNever(user.LastLoginAt))
				return nil
			})
		},
	}
}

func setPasswordCmd() *cobra.Command {
	var email, plain string

	cmd := &cobra.Command{
		Use:   "set-password",
		Short: "Replace a person's password",
		Long: `Prints the new password once, as create-user does, and generates
one unless --password is given; a password given that way is held to the
same policy create-user holds it to.

Every session the person had is ended: a password changed after a leak
that left the old sessions alive would not have changed anything.`,
		Args:    cobra.NoArgs,
		Example: "  go-route-admin set-password --email ops@example.com",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withUsers(cmd.Context(), func(ctx context.Context, users *admin.Users) error {
				changed, err := users.SetPassword(ctx, ports.Write{Actor: cliActor()}, email, plain)
				if err != nil {
					return err
				}
				printCreatedUser(os.Stdout, changed)
				return nil
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&email, "email", "", "whose password to replace")
	f.StringVar(&plain, "password", "", "set this password instead of generating one (12-128 characters)")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

// withUsers resolves the DSN, opens a pool for the one command, and hands over
// the use case. It passes no token issuer: nothing here issues a session, so
// the CLI never needs the signing secret.
func withUsers(ctx context.Context, fn func(context.Context, *admin.Users) error) error {
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

	// Creating the first person happens before any service has run, so the CLI
	// has to make sure its audit row has a partition to land in.
	if err := postgresql.NewPartitions(pool).Ensure(ctx); err != nil {
		return err
	}

	return fn(ctx, admin.NewUsers(repositories.NewAdminUserRepo(pool), nil, 0, time.Now, nil))
}

func printCreatedUser(out io.Writer, created ports.CreatedAdminUser) {
	user := created.User
	fmt.Fprintf(out, "\n  %s\n", user.Email)
	fmt.Fprintf(out, "  id %s\n", user.ID)
	fmt.Fprintf(out, "  role %s (%s)\n\n", user.Role, roleSummary(user.Role))

	if created.Password == "" {
		fmt.Fprint(out, "  Password set to the one you supplied; it is not echoed here.\n\n")
		return
	}
	fmt.Fprintf(out, "  %s\n\n", created.Password)
	fmt.Fprint(out, "  This password is shown once and cannot be shown again: only an\n")
	fmt.Fprint(out, "  argon2id hash of it is stored. Lose it and set another with\n")
	fmt.Fprint(out, "  set-password.\n\n")
	fmt.Fprint(out, "  Log in at:  POST /admin/v1/auth/login\n\n")
}

func printUsers(out io.Writer, list []domains.AdminUser) {
	if len(list) == 0 {
		fmt.Fprintln(out, "no admin users; create one with: go-route-admin create-user --email <email>")
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tROLE\tCREATED\tLAST LOGIN\tPASSWORD SET\tDISABLED")
	for _, u := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			u.Email, u.Role, u.CreatedAt.UTC().Format(time.DateOnly),
			whenOrNever(u.LastLoginAt), u.PasswordChangedAt.UTC().Format(time.DateOnly),
			whenOrNever(u.DisabledAt))
	}
	_ = w.Flush()
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Break-glass recovery (Sprint 22 Phase T; ADR-029, Decision Record D-26
// note; Sprint 27 G1, D-75):
//
//	zecurity-controller provider recover-admin --email <super-admin email> --reason "<why>"
//
// Run on the controller host with the controller's DATABASE_URL. It is the
// only break-glass path (no standing account, no console or API route). For
// an active super-admin it sets a new temporary password (printed once, to
// stdout only), clears their TOTP and ends their sessions; they change the
// password and enrol TOTP again at next sign-in. --reason is required: without
// it nothing changes. Audited as provider_user.recover_admin by system:cli
// (with the reason and host). The same transaction records a break-glass
// event, which the running controller turns into a critical
// provider.break_glass_used alert, an email to every active super-admin and a
// console banner until acknowledged. It never reads or prints a TOTP secret.

func isRecoverAdminCommand(args []string) bool {
	return len(args) >= 3 && args[1] == "provider" && args[2] == "recover-admin"
}

func parseRecoverAdminArgs(args []string, stderr io.Writer) (email, reason string, err error) {
	fs := flag.NewFlagSet("provider recover-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	e := fs.String("email", "", "email of the super-admin to recover")
	r := fs.String("reason", "", "why break-glass is used (required; recorded, alerted and emailed to super-admins)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: zecurity-controller provider recover-admin --email <super-admin email> --reason "<why>"`)
	}
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*e) == "" || strings.TrimSpace(*r) == "" {
		fs.Usage()
		return "", "", errors.New("recover-admin: --email and --reason are required and it takes no other arguments")
	}
	return *e, *r, nil
}

// runRecoverAdmin returns the process exit code.
func runRecoverAdmin(args []string) int {
	email, reason, err := parseRecoverAdminArgs(args, os.Stderr)
	if err != nil {
		return 2
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Init(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "recover-admin: database: %v\n", err)
		return 1
	}
	defer db.Close()

	password, err := provider.RecoverAdmin(ctx, provider.NewStore(db.Pool), email, reason, host)
	switch {
	case errors.Is(err, provider.ErrBreakGlassReason):
		fmt.Fprintln(os.Stderr, "recover-admin: --reason must be 1-500 characters without control characters; nothing changed")
		return 2
	case errors.Is(err, provider.ErrProviderUserNotFound):
		fmt.Fprintln(os.Stderr, "recover-admin: no provider user with that email")
		return 1
	case errors.Is(err, provider.ErrNotSuperAdmin):
		fmt.Fprintln(os.Stderr, "recover-admin: that operator is not a super-admin")
		return 1
	case errors.Is(err, provider.ErrAccountDisabled):
		fmt.Fprintln(os.Stderr, "recover-admin: that super-admin is disabled; enable it from another super-admin or recover a different one")
		return 1
	case err != nil:
		fmt.Fprintf(os.Stderr, "recover-admin: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "recover-admin: done. The temporary password is printed once below; share it securely.")
	fmt.Fprintln(os.Stderr, "At next sign-in the operator must change it and enrol TOTP again.")
	fmt.Fprintln(os.Stderr, "Every active super-admin is alerted and emailed (reason and host, never the password).")
	fmt.Println(password)
	return 0
}

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

// Break-glass recovery (Sprint 22 Phase T; ADR-029, Decision Record D-26 note):
//
//	zecurity-controller provider recover-admin --email <super-admin email>
//
// Run on the controller host with the controller's DATABASE_URL. For an active
// super-admin it sets a new temporary password (printed once, to stdout only),
// clears their TOTP and ends their sessions; they change the password and
// enrol TOTP again at next sign-in. Audited as provider_user.recover_admin by
// system:cli. It never reads or prints a TOTP secret.

func isRecoverAdminCommand(args []string) bool {
	return len(args) >= 3 && args[1] == "provider" && args[2] == "recover-admin"
}

func parseRecoverAdminArgs(args []string, stderr io.Writer) (string, error) {
	fs := flag.NewFlagSet("provider recover-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "email of the super-admin to recover")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: zecurity-controller provider recover-admin --email <super-admin email>")
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*email) == "" {
		fs.Usage()
		return "", errors.New("recover-admin: --email is required and takes no other arguments")
	}
	return *email, nil
}

// runRecoverAdmin returns the process exit code.
func runRecoverAdmin(args []string) int {
	email, err := parseRecoverAdminArgs(args, os.Stderr)
	if err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Init(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "recover-admin: database: %v\n", err)
		return 1
	}
	defer db.Close()

	password, err := provider.RecoverAdmin(ctx, provider.NewStore(db.Pool), email)
	switch {
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
	fmt.Println(password)
	return 0
}

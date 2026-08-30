package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cleanunicorn/dispatch/internal/config"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/store/sqlite"
	trweb "github.com/cleanunicorn/dispatch/internal/transport/web"
)

// runUser manages the web UI's accounts, which live in the store:
//
//	dispatch user add <name> [password]     make an account; without a password one is generated and printed
//	dispatch user passwd <name> [password]  set a new password (and end the user's sessions)
//	dispatch user rm <name>                 remove the account
//	dispatch user list
//
// A password given on the command line lands in the shell history; leave
// it out and use the generated one, which the user changes in the UI.
// The running dispatch sees the change at once — the store is shared.
func runUser(cfgPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: dispatch user add|passwd|rm|list [name] [password]")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := sqlite.Open(cfg.Server.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	name := ""
	if len(args) > 1 {
		name = strings.ToLower(strings.TrimSpace(args[1]))
	}
	needName := func() error {
		if name == "" {
			return fmt.Errorf("usage: dispatch user %s <name>", args[0])
		}
		if !trweb.ValidName.MatchString(name) {
			return fmt.Errorf("user name %q: lowercase letters, digits, . _ -, up to 32 characters", name)
		}
		return nil
	}
	switch args[0] {
	case "add", "passwd":
		if err := needName(); err != nil {
			return err
		}
		_, err := st.GetUser(ctx, name)
		exists := err == nil
		if args[0] == "add" && exists {
			return fmt.Errorf("user %q exists — `dispatch user passwd %s` changes the password", name, name)
		}
		if args[0] == "passwd" && !exists {
			return fmt.Errorf("no user %q", name)
		}
		password, generated := "", false
		if len(args) > 2 {
			password = args[2]
		} else {
			if password, err = trweb.GeneratePassword(); err != nil {
				return err
			}
			generated = true
		}
		hash, err := trweb.HashPassword(password)
		if err != nil {
			return err
		}
		if err := st.PutUser(ctx, store.User{Name: name, Password: hash}); err != nil {
			return err
		}
		if err := st.DeleteUserSessions(ctx, name); err != nil {
			return err
		}
		if generated {
			fmt.Printf("user %s — password: %s\n(change it in the UI: your name at the bottom left)\n", name, password)
		} else {
			fmt.Printf("user %s — password set\n", name)
		}
		fmt.Fprintf(os.Stderr, "web UI: http://%s\n", cfg.Web.Listen)
		return nil
	case "rm", "remove", "delete":
		if err := needName(); err != nil {
			return err
		}
		if _, err := st.GetUser(ctx, name); err != nil {
			return fmt.Errorf("no user %q", name)
		}
		if err := st.DeleteUser(ctx, name); err != nil {
			return err
		}
		fmt.Printf("user %s removed\n", name)
		return nil
	case "list", "ls":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Println("no users — `dispatch user add <name>` makes one")
			return nil
		}
		for _, u := range users {
			fmt.Printf("%-20s since %s\n", u.Name, u.CreatedAt.Local().Format("2006-01-02"))
		}
		return nil
	}
	return fmt.Errorf("unknown user command %q (add|passwd|rm|list)", args[0])
}

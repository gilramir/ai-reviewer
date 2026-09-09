// Command ai-reviewer serves a Markdown review session in a browser, backed by
// a Claude Code process per document.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gilramir/argparse/v2"
	"golang.org/x/term"

	"github.com/gilramir/ai-reviewer/internal/config"
	"github.com/gilramir/ai-reviewer/internal/review"
	"github.com/gilramir/ai-reviewer/internal/server"
)

// version is reported by --version.
const version = "ai-reviewer 0.1.0"

// serveOptions holds the flags for the serve subcommand. argparse fills these
// in by deriving each field name from its switch, so --max-budget-usd lands in
// MaxBudgetUsd; the parser fails at startup if a switch has no matching field.
type serveOptions struct {
	Root         string
	Listen       string
	Branch       string
	NoAuth       bool
	Model        string
	Claude       string
	MaxBudgetUsd float64
	IdleTimeout  time.Duration
	MaxLive      int
}

// passwordOptions has no flags of its own, but argparse wants a value struct
// for every command.
type passwordOptions struct{}

func main() {
	ap := argparse.New(&argparse.Command{
		Name:        "ai-reviewer",
		Description: "Review Markdown documents in a browser, with Claude Code editing them.",
		Epilog: `Each document under --root gets its own long-lived Claude Code process, so
follow-up comments on the same file stay in one conversation. Every turn that
changes a file is committed to the task branch.

Authentication is on by default. --no-auth is refused unless --listen is a
loopback address, since that combination would otherwise publish the documents
to the network without a word.`,
	})
	ap.Version = version

	addServeCommand(ap)
	addPasswordCommand(ap)

	// With no subcommand the root has no Function, so argparse prints the help
	// and exits non-zero, which is the behaviour we want for a bare invocation.
	ap.ParseAndExit()
}

func addServeCommand(ap *argparse.ArgumentParser) {
	opts := &serveOptions{
		Root:        ".",
		Listen:      "127.0.0.1:8080",
		Claude:      "claude",
		IdleTimeout: 30 * time.Minute,
		MaxLive:     6,
	}

	cmd := ap.New(&argparse.Command{
		Name:        "serve",
		Description: "Serve the documents under --root for review",
		Function:    runServe,
		Values:      opts,
	})

	cmd.Add(&argparse.Argument{
		Switches: []string{"--root"},
		MetaVar:  "DIR",
		Help:     "Directory of documents to review",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--listen"},
		MetaVar:  "ADDR",
		Help:     "Address to bind; use 0.0.0.0:8080 to reach it from the LAN",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--branch"},
		MetaVar:  "NAME",
		Help:     "Task branch for review commits; prompted for if omitted",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--no-auth"},
		Help:     "Serve without a password; refused unless --listen is loopback",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--model"},
		MetaVar:  "NAME",
		Help:     "Model alias or name passed to claude",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--claude"},
		MetaVar:  "PATH",
		Help:     "Path to the claude executable",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--max-budget-usd"},
		MetaVar:  "AMOUNT",
		Help:     "Per-process spend cap; 0 for none",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--idle-timeout"},
		MetaVar:  "#(h|m|s)",
		Help:     "Stop a document's claude process after this long idle",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--max-live"},
		MetaVar:  "N",
		Help:     "Maximum concurrent claude processes",
	})
}

func addPasswordCommand(ap *argparse.ArgumentParser) {
	ap.New(&argparse.Command{
		Name:        "password",
		Description: "Store a password that survives restarts",
		Function:    runPassword,
		Values:      &passwordOptions{},
	})
}

func runServe(_ *argparse.Command, values argparse.Values) error {
	opts := values.(*serveOptions)

	// Authentication is the default. Disabling it is only coherent when nothing
	// off this machine can reach the port, and getting that combination wrong
	// silently publishes the documents to the network.
	if opts.NoAuth && !server.IsLoopback(opts.Listen) {
		return fmt.Errorf("--no-auth requires a loopback address; %q is reachable from the network", opts.Listen)
	}

	if _, err := exec.LookPath(opts.Claude); err != nil {
		return fmt.Errorf("cannot find the claude executable %q: %w", opts.Claude, err)
	}

	branchName, err := resolveBranch(opts.Root, opts.Branch)
	if err != nil {
		return err
	}

	rev, err := review.New(review.Options{
		Root:         opts.Root,
		Branch:       branchName,
		Model:        opts.Model,
		ClaudeBinary: opts.Claude,
		MaxBudgetUSD: opts.MaxBudgetUsd,
		IdleTimeout:  opts.IdleTimeout,
		MaxLive:      opts.MaxLive,
	})
	if err != nil {
		return err
	}
	defer rev.Close()

	auth, secret, err := buildAuth(opts.NoAuth)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              opts.Listen,
		Handler:           server.New(server.Options{Review: rev, Auth: auth}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := rev.Watch(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "watch:", err)
		}
	}()

	announce(opts.Listen, branchName, rev.Root(), secret, auth == nil)

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	// A clean shutdown is not a failure, and argparse would otherwise print
	// ErrServerClosed and exit non-zero.
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// buildAuth chooses between a stored password and a freshly generated secret.
// The returned secret is empty when a stored password is in use, since the
// daemon does not know it.
func buildAuth(disabled bool) (*server.Auth, string, error) {
	if disabled {
		return nil, "", nil
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, "", fmt.Errorf("reading config: %w", err)
	}
	if cfg.PasswordHash != "" {
		auth, err := server.NewPasswordAuth(cfg.PasswordHash)
		if err != nil {
			return nil, "", fmt.Errorf("stored password is unusable: %w; re-run `ai-reviewer password`", err)
		}
		return auth, "", nil
	}

	secret, err := server.GenerateSecret()
	if err != nil {
		return nil, "", err
	}
	return server.NewTokenAuth(secret), secret, nil
}

func announce(listen, branch, root, secret string, noAuth bool) {
	fmt.Printf("\n  ai-reviewer\n\n")
	fmt.Printf("    reviewing  %s\n", root)
	if branch != "" {
		fmt.Printf("    branch     %s\n", branch)
	}
	fmt.Printf("    open       %s\n", server.DisplayURL(listen, false))

	switch {
	case noAuth:
		fmt.Printf("    password   (disabled; loopback only)\n")
	case secret != "":
		fmt.Printf("    password   %s\n", secret)
	default:
		fmt.Printf("    password   (the one you set with `ai-reviewer password`)\n")
	}
	fmt.Println()
}

// resolveBranch settles the task branch every review commit lands on.
//
// The branch is per review session rather than per document: commits from
// several documents interleaving on one branch is the point, since the session
// is the unit of work being reviewed.
func resolveBranch(root, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	if !insideGitRepo(root) {
		// Outside a repository there is no branch to be on; the review falls
		// back to directory snapshots.
		return "", nil
	}

	suggestion := suggestBranch(root)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return suggestion, nil
	}

	fmt.Printf("Task branch for this review [%s]: ", suggestion)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return suggestion, nil
	}
	if answer := strings.TrimSpace(line); answer != "" {
		return answer, nil
	}
	return suggestion, nil
}

func insideGitRepo(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = dir
	return cmd.Run() == nil
}

func suggestBranch(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	name := slug(filepath.Base(abs))
	if name == "" {
		name = "docs"
	}
	return fmt.Sprintf("review/%s-%s", name, time.Now().Format("2006-01-02"))
}

// slug reduces a directory name to something git will accept in a ref.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune('-')
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// runPassword stores a password that survives restarts, in the style of
// `jupyter notebook password`.
func runPassword(_ *argparse.Command, _ argparse.Values) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("`ai-reviewer password` needs a terminal")
	}

	fmt.Print("New password: ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(first))) < 8 {
		return errors.New("password must be at least 8 characters")
	}

	fmt.Print("Repeat password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return err
	}
	if string(first) != string(second) {
		return errors.New("passwords did not match")
	}

	hash, err := server.HashPassword(string(first))
	if err != nil {
		return err
	}
	if err := config.Save(config.Config{PasswordHash: hash}); err != nil {
		return err
	}

	path, _ := config.Path()
	fmt.Printf("Password stored in %s\n", path)
	return nil
}

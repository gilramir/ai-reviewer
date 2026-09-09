// Command ai-reviewer serves a Markdown review session in a browser, backed by
// a Claude Code process per document.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gilramir/ai-reviewer/internal/config"
	"github.com/gilramir/ai-reviewer/internal/review"
	"github.com/gilramir/ai-reviewer/internal/server"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "password" {
		if err := setPassword(); err != nil {
			fail(err)
		}
		return
	}

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if err := serve(args); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ai-reviewer:", err)
	os.Exit(1)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("ai-reviewer", flag.ExitOnError)

	root := fs.String("root", ".", "directory of documents to review")
	listen := fs.String("listen", "127.0.0.1:8080", "address to bind; use 0.0.0.0:8080 to reach it from the LAN")
	branch := fs.String("branch", "", "task branch for review commits (prompted for if omitted)")
	noAuth := fs.Bool("no-auth", false, "serve without a password; refused unless the bind address is loopback")
	model := fs.String("model", "", "model alias or name passed to claude")
	claudeBin := fs.String("claude", "claude", "path to the claude executable")
	budget := fs.Float64("max-budget-usd", 0, "per-process spend cap, 0 for none")
	idle := fs.Duration("idle-timeout", 30*time.Minute, "stop a document's claude process after this long idle")
	maxLive := fs.Int("max-live", 6, "maximum concurrent claude processes")

	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: ai-reviewer [serve] [flags]
       ai-reviewer password

Serves the Markdown files under -root for review in a browser. Each document
gets its own long-lived Claude Code process; each turn that changes a file is
committed to the task branch.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Authentication is the default. Disabling it is only coherent when nothing
	// off this machine can reach the port, and getting that combination wrong
	// silently publishes the documents to the network.
	if *noAuth && !server.IsLoopback(*listen) {
		return fmt.Errorf("-no-auth requires a loopback address; %q is reachable from the network", *listen)
	}

	if _, err := exec.LookPath(*claudeBin); err != nil {
		return fmt.Errorf("cannot find the claude executable %q: %w", *claudeBin, err)
	}

	branchName, err := resolveBranch(*root, *branch)
	if err != nil {
		return err
	}

	rev, err := review.New(review.Options{
		Root:         *root,
		Branch:       branchName,
		Model:        *model,
		ClaudeBinary: *claudeBin,
		MaxBudgetUSD: *budget,
		IdleTimeout:  *idle,
		MaxLive:      *maxLive,
	})
	if err != nil {
		return err
	}
	defer rev.Close()

	auth, secret, err := buildAuth(*noAuth)
	if err != nil {
		return err
	}

	handler := server.New(server.Options{Review: rev, Auth: auth})
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := rev.Watch(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "watch:", err)
		}
	}()

	announce(*listen, branchName, rev.Root(), secret, auth == nil)

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	return httpServer.ListenAndServe()
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

// setPassword stores a password that survives restarts, in the style of
// `jupyter notebook password`.
func setPassword() error {
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

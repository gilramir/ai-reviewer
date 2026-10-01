// Command ai-reviewer serves a Markdown review session in a browser, backed by
// a Claude Code process per document.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gilramir/argparse/v2"
	"golang.org/x/term"

	"github.com/gilramir/ai-reviewer/internal/claudeproc"
	"github.com/gilramir/ai-reviewer/internal/config"
	"github.com/gilramir/ai-reviewer/internal/gitstore"
	"github.com/gilramir/ai-reviewer/internal/logview"
	"github.com/gilramir/ai-reviewer/internal/review"
	"github.com/gilramir/ai-reviewer/internal/server"
	"github.com/gilramir/ai-reviewer/internal/wirelog"
)

// version is reported by --version.
const version = "ai-reviewer 0.1.0"

// defaultListen is also how --listen tells "left alone" from "set to loopback
// on purpose", which is what makes it an error to pass --listen-all as well.
const defaultListen = "127.0.0.1:8080"

// serveOptions holds the flags for the serve subcommand. argparse fills these
// in by deriving each field name from its switch, so --max-budget-usd lands in
// MaxBudgetUsd; the parser fails at startup if a switch has no matching field.
type serveOptions struct {
	Root          string
	Listen        string
	ListenAll     string
	Branch        string
	NoAuth        bool
	Model         string
	Claude        string
	MaxBudgetUsd  float64
	IdleTimeout   time.Duration
	MaxLive       int
	ClaudeLog     bool
	ClaudeLogFile string
}

// logViewOptions holds the flags for the log-view subcommand.
type logViewOptions struct {
	File   string
	Listen string
}

// defaultLogListen sits beside the review's own default port, so a review and
// its log can be open at once without either flag.
const defaultLogListen = "127.0.0.1:8081"

// defaultLogName is where --claude-log writes: beside the review's state, which
// is already kept out of the repository.
const defaultLogName = "claude.db"

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

Authentication is on by default. --no-auth is refused unless the bind address is
a loopback one, since that combination would otherwise publish the documents to
the network without a word. --listen-all never is.`,
	})
	ap.Version = version

	addServeCommand(ap)
	addPasswordCommand(ap)
	addLogViewCommand(ap)

	// With no subcommand the root has no Function, so argparse prints the help
	// and exits non-zero, which is the behaviour we want for a bare invocation.
	ap.ParseAndExit()
}

func addServeCommand(ap *argparse.ArgumentParser) {
	opts := &serveOptions{
		Root:        ".",
		Listen:      defaultListen,
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
		Help:     "Address to bind",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--listen-all"},
		MetaVar:  "PORT",
		Help:     "Reach it from the LAN: shorthand for --listen 0.0.0.0:PORT",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--branch"},
		MetaVar:  "NAME",
		Help:     "Task branch for review commits; prompted for if omitted",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--no-auth"},
		Help:     "Serve without a password; refused unless the bind address is loopback",
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
	// Two switches rather than one taking an optional value, which argparse
	// does not offer for a switch: the common case needs no path, and naming
	// one is enough to say the log is wanted.
	cmd.Add(&argparse.Argument{
		Switches: []string{"--claude-log"},
		Help: "Record every frame exchanged with claude in a SQLite file, " +
			".ai-reviewer/" + defaultLogName + " at the workspace root",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--claude-log-file"},
		MetaVar:  "FILE",
		Help:     "Like --claude-log, writing to FILE instead",
	})
}

func addLogViewCommand(ap *argparse.ArgumentParser) {
	opts := &logViewOptions{Listen: defaultLogListen}
	cmd := ap.New(&argparse.Command{
		Name:        "log-view",
		Description: "Serve a page for reading a log written by serve --claude-log",
		Function:    runLogView,
		Values:      opts,
	})
	cmd.Add(&argparse.Argument{
		Name:    "file",
		MetaVar: "FILE",
		Help:    "The log to read",
	})
	cmd.Add(&argparse.Argument{
		Switches: []string{"--listen"},
		MetaVar:  "ADDR",
		Help:     "Address to bind; loopback only",
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

	listen, err := listenAddress(opts.Listen, opts.ListenAll)
	if err != nil {
		return err
	}

	// Authentication is the default. Disabling it is only coherent when nothing
	// off this machine can reach the port, and getting that combination wrong
	// silently publishes the documents to the network.
	if opts.NoAuth && !server.IsLoopback(listen) {
		return fmt.Errorf("--no-auth requires a loopback address; %q is reachable from the network", listen)
	}

	if _, err := exec.LookPath(opts.Claude); err != nil {
		return fmt.Errorf("cannot find the claude executable %q: %w", opts.Claude, err)
	}

	branchName, err := resolveBranch(opts.Root, opts.Branch)
	if err != nil {
		return err
	}

	var tap claudeproc.Tap
	var logPath string
	if opts.ClaudeLog || opts.ClaudeLogFile != "" {
		log, path, err := openClaudeLog(opts.Root, opts.ClaudeLogFile)
		if err != nil {
			return err
		}
		defer log.Close()
		tap, logPath = log, path
	}

	rev, err := review.New(review.Options{
		Root:         opts.Root,
		Branch:       branchName,
		Model:        opts.Model,
		ClaudeBinary: opts.Claude,
		MaxBudgetUSD: opts.MaxBudgetUsd,
		IdleTimeout:  opts.IdleTimeout,
		MaxLive:      opts.MaxLive,
		Tap:          tap,
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
		Addr:              listen,
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

	announce(listen, branchName, rev.Root(), rev.WorkRoot(), secret, auth == nil)
	if logPath != "" {
		fmt.Printf("    claude log %s\n      read it  ai-reviewer log-view %s\n\n", logPath, logPath)
	}

	// Printed after the banner so it is the last thing on screen, not the
	// first thing scrolled away by it.
	for _, notice := range rev.Notices() {
		fmt.Fprintf(os.Stderr, "  warning: %s\n\n", notice)
	}

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

	// Closed here as well as by the deferred call, so that a turn which
	// committed while the daemon was stopping is in the count below.
	_ = rev.Close()
	fmt.Print(landing(rev.Settings()))
	return nil
}

// openClaudeLog creates the wire log, at the default place when no file was
// named. The default is under the workspace rather than --root because that is
// where the review keeps everything else it writes, and what .gitignore covers.
func openClaudeLog(root, path string) (*wirelog.Log, string, error) {
	if path == "" {
		work, err := gitstore.Workspace(root)
		if err != nil {
			return nil, "", err
		}
		dir := filepath.Join(work, ".ai-reviewer")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, "", err
		}
		path = filepath.Join(dir, defaultLogName)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	log, err := wirelog.Create(path, wirelog.Run{Root: absRoot, Version: version})
	if err != nil {
		return nil, "", fmt.Errorf("claude log: %w", err)
	}
	return log, path, nil
}

// runLogView serves the viewer for a wire log. It can run while the serve that
// writes the log is still going, and the page follows along.
func runLogView(_ *argparse.Command, values argparse.Values) error {
	opts := values.(*logViewOptions)

	if !server.IsLoopback(opts.Listen) {
		return fmt.Errorf("log-view serves without a password, so only on a loopback address; %q is reachable from the network. Use an SSH tunnel to read it from elsewhere", opts.Listen)
	}

	log, err := wirelog.OpenReader(opts.File)
	if err != nil {
		return err
	}
	defer log.Close()

	httpServer := &http.Server{
		Addr:              opts.Listen,
		Handler:           logview.Handler(log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()

	fmt.Printf("\n  ai-reviewer log-view\n\n    reading  %s\n    open     %s\n\n", opts.File, server.DisplayURL(opts.Listen, false))

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listenAddress settles which address to bind.
//
// --listen-all exists because the address people actually want on a LAN is
// 0.0.0.0:PORT, and typing it correctly matters: a typo in the host half is
// either a bind error or, worse, a daemon listening somewhere other than where
// they think. A port on its own cannot be mistyped into a different meaning.
func listenAddress(listen, listenAll string) (string, error) {
	if listenAll == "" {
		return listen, nil
	}
	if listen != defaultListen {
		return "", fmt.Errorf("--listen %q and --listen-all %q both name an address; pass one of them", listen, listenAll)
	}

	port, err := strconv.Atoi(listenAll)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("--listen-all takes a port number from 1 to 65535, not %q; --listen takes the whole address", listenAll)
	}
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(port)), nil
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

func announce(listen, branch, root, workRoot, secret string, noAuth bool) {
	fmt.Printf("\n  ai-reviewer\n\n")
	fmt.Printf("    reviewing  %s\n", root)
	// Claude runs at the repository root so it can read what the documents
	// reference. That is wider than the directory under review, so say so
	// whenever the two differ rather than leaving it to be discovered.
	if workRoot != "" && workRoot != root {
		fmt.Printf("    claude in  %s\n", workRoot)
	}
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

// landing says how to merge the review branch, printed as the daemon stops.
//
// The end of the session is where this belongs rather than the banner: at
// startup there is nothing to land and no count to give, and by the time there
// is, the banner is far up the scrollback. Stopping the daemon is also the
// moment the question is actually asked, and the reviewer is already at a
// prompt to answer it in.
//
// It spells both merges out because the reviewer this tool is for is not
// necessarily fluent in git, and every one of these commands is one they can
// undo: nothing here rewrites history.
//
// Each merge ends in its own delete rather than the two sharing one, because
// they cannot share one: -d refuses after a squash. A single -d printed under
// both, however carefully the paragraph after it explains itself, is the
// command that fails for whoever squashed.
func landing(s review.Settings) string {
	// Outside a repository there is no branch to merge. The review kept
	// snapshots of each file instead, and those are not going anywhere.
	if s.Branch == "" {
		return ""
	}

	var b strings.Builder
	if s.Commits == 0 {
		fmt.Fprintf(&b, "\n  Nothing is waiting on %s.\n\n", s.Branch)
		fmt.Fprintf(&b, "  Delete it if you are finished with this review:\n\n")
		fmt.Fprintf(&b, "    git branch -d %s\n\n", s.Branch)
		return b.String()
	}

	// The base is only unknown when the branch this was cut from has since
	// gone, which leaves the reviewer to name the one they want instead.
	base := s.BaseBranch
	if base == "" {
		base = "<the branch you want them on>"
	}

	// A squash of one commit is still a squash -- what it offers there is a
	// message of your own rather than fewer commits.
	squash := fmt.Sprintf("  or, to land the whole review as one commit rather than %d:\n\n", s.Commits)
	if s.Commits == 1 {
		squash = "  or, to land it with a commit message of your own:\n\n"
	}

	fmt.Fprintf(&b, "\n  %s on %s.\n", countedCommits(s.Commits), s.Branch)
	fmt.Fprintf(&b, "  Nothing reaches %s until you merge %s:\n\n", base, them(s.Commits))
	fmt.Fprintf(&b, "    git switch %s\n", base)
	fmt.Fprintf(&b, "    git merge %s\n", s.Branch)
	fmt.Fprintf(&b, "    git branch -d %s\n\n", s.Branch)
	b.WriteString(squash)
	fmt.Fprintf(&b, "    git switch %s\n", base)
	fmt.Fprintf(&b, "    git merge --squash %s\n", s.Branch)
	fmt.Fprintf(&b, "    git commit\n")
	fmt.Fprintf(&b, "    git branch -D %s\n\n", s.Branch)
	fmt.Fprintf(&b, "  The last line of each is only if you want the branch gone, and the\n")
	fmt.Fprintf(&b, "  two are not the same command. After a squash the single commit is\n")
	fmt.Fprintf(&b, "  not the commits on the branch, so git cannot tell they landed and\n")
	fmt.Fprintf(&b, "  refuses the lowercase -d. Look at what you merged, then use -D,\n")
	fmt.Fprintf(&b, "  which deletes without checking.\n\n")
	return b.String()
}

func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func countedCommits(n int) string {
	if n == 1 {
		return "1 commit is waiting"
	}
	return fmt.Sprintf("%d commits are waiting", n)
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

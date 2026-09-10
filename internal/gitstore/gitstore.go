// Package gitstore records one commit per review turn.
//
// A snapshot per turn is what separates review from hoping: every comment gets
// a diff you can read and a change you can undo, and `git log` on the review
// branch becomes the record of the session.
//
// Every mutation goes through a single writer. Two documents under review are
// two Claude processes editing two files concurrently, and without
// serialisation their commits race on .git/index.lock.
package gitstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Trailer is a machine-readable key/value line at the foot of a commit message.
type Trailer struct {
	Key   string
	Value string
}

// Commit describes one turn's worth of change.
type Commit struct {
	// Paths are the files to stage, relative to the repository root.
	Paths []string
	// Subject is the first line: normally the reviewer's own comment.
	Subject string
	// Body explains the change; the quoted anchor lives here.
	Body string
	// Trailers join the commit back to the thread that caused it.
	Trailers []Trailer
}

// History records review turns. Implementations are safe for concurrent use.
type History interface {
	// Root is the directory changes are recorded relative to: the repository
	// top level under git, or the review root outside one. Paths handed to
	// Record are relative to it, and it is the natural workspace root for
	// anything else that needs one.
	Root() string
	// EnsureBranch puts the working tree on the named branch, creating it from
	// the current HEAD if it does not exist.
	EnsureBranch(name string) error
	// Record stages and commits the given paths, returning a reference that
	// identifies the change. It returns an empty reference when nothing
	// changed, which is the normal outcome for a question the model answered
	// without editing.
	Record(c Commit) (string, error)
	// Revert undoes a previously recorded change.
	Revert(ref string) error
	// Describe returns a short human-readable summary of a reference.
	Describe(ref string) string
	// Branch is the branch the working tree is on, empty when that is not a
	// question this history can answer.
	Branch() string
	// CommitsSince counts the commits on the current branch that base does not
	// have. It is how many changes a review is holding that have not been
	// merged anywhere, and it falls to zero of its own accord once they have.
	CommitsSince(base string) int
}

// Open returns a History for root. A git repository gets real commits; anything
// else falls back to directory snapshots so a document outside version control
// is still reviewable and still undoable.
func Open(root string) (History, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("review root: %w", err)
	}

	top, err := gitTopLevel(abs)
	if err != nil {
		return newSnapshots(abs)
	}
	return &gitHistory{root: top}, nil
}

func gitTopLevel(dir string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// --- git --------------------------------------------------------------------

type gitHistory struct {
	root string
	mu   sync.Mutex // the single-writer discipline; see package doc
}

// Root is the repository top level, which is where every git command runs and
// what the paths in a Commit are relative to.
func (g *gitHistory) Root() string { return g.root }

func (g *gitHistory) EnsureBranch(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if name == "" {
		return errors.New("gitstore: empty branch name")
	}
	if err := g.run("check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("%q is not a usable branch name", name)
	}

	current, err := g.output("rev-parse", "--abbrev-ref", "HEAD")
	if err == nil && current == name {
		return nil
	}

	// Refuse to move a dirty tree: switching branches under uncommitted work is
	// how a review session eats someone's unrelated edits.
	dirty, err := g.output("status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("working tree has uncommitted changes; commit or stash before starting a review on %q", name)
	}

	if err := g.run("rev-parse", "--verify", "--quiet", "refs/heads/"+name); err == nil {
		return g.run("checkout", name)
	}
	return g.run("checkout", "-b", name)
}

// Branch reports the checked-out branch, or "" on a detached HEAD.
func (g *gitHistory) Branch() string {
	name, err := g.output("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || name == "HEAD" {
		return ""
	}
	return name
}

func (g *gitHistory) CommitsSince(base string) int {
	if base == "" {
		return 0
	}
	out, err := g.output("rev-list", "--count", base+"..HEAD")
	if err != nil {
		// The base branch is gone, or was never there. Nothing to say.
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

func (g *gitHistory) Record(c Commit) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(c.Paths) == 0 {
		return "", nil
	}

	args := append([]string{"add", "--"}, c.Paths...)
	if err := g.run(args...); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	staged, err := g.output("diff", "--cached", "--name-only")
	if err != nil {
		return "", fmt.Errorf("git diff --cached: %w", err)
	}
	if strings.TrimSpace(staged) == "" {
		// The model answered without editing, or rewrote a file to what it
		// already contained. Either way there is nothing to record.
		return "", nil
	}

	if err := g.run("commit", "--no-verify", "-m", c.Subject, "-m", messageBody(c)); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	return g.output("rev-parse", "HEAD")
}

// Revert restores the files a commit touched to their state in its parent,
// leaving the revert itself as a new commit so the history stays append-only.
func (g *gitHistory) Revert(ref string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if ref == "" {
		return errors.New("gitstore: empty reference")
	}
	changed, err := g.output("show", "--pretty=format:", "--name-only", ref)
	if err != nil {
		return fmt.Errorf("git show: %w", err)
	}
	paths := strings.Fields(changed)
	if len(paths) == 0 {
		return nil
	}

	args := append([]string{"checkout", ref + "^", "--"}, paths...)
	if err := g.run(args...); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	subject := fmt.Sprintf("Revert review change %s", short(ref))
	return g.run("commit", "--no-verify", "-m", subject)
}

func (g *gitHistory) Describe(ref string) string {
	if ref == "" {
		return ""
	}
	return short(ref)
}

func (g *gitHistory) run(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

func (g *gitHistory) output(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.root
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func messageBody(c Commit) string {
	var b strings.Builder
	if c.Body != "" {
		b.WriteString(c.Body)
	}
	if len(c.Trailers) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		for _, t := range c.Trailers {
			fmt.Fprintf(&b, "%s: %s\n", t.Key, t.Value)
		}
	}
	return b.String()
}

func short(ref string) string {
	if len(ref) > 8 {
		return ref[:8]
	}
	return ref
}

// --- snapshots --------------------------------------------------------------

// snapshots is the fallback for documents outside a git repository. It keeps a
// copy of every file before each turn changes it, which is enough to show a
// diff and to undo.
type snapshots struct {
	root string
	dir  string
	mu   sync.Mutex
}

func newSnapshots(root string) (*snapshots, error) {
	dir := filepath.Join(root, ".ai-reviewer", "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot dir: %w", err)
	}
	return &snapshots{root: root, dir: dir}, nil
}

// Root is the reviewed directory itself: outside a repository there is nothing
// wider to anchor to.
func (s *snapshots) Root() string { return s.root }

// EnsureBranch is a no-op outside git: there is no branch to be on, and
// refusing to run would make documents outside a repository unreviewable.
func (s *snapshots) EnsureBranch(string) error { return nil }

func (s *snapshots) Record(c Commit) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(c.Paths) == 0 {
		return "", nil
	}

	stamp := time.Now().UTC().Format("20060102T150405.000")
	dest := filepath.Join(s.dir, stamp)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}

	copied := 0
	for _, rel := range c.Paths {
		src := filepath.Join(s.root, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return "", err
		}
		copied++
	}
	if copied == 0 {
		_ = os.RemoveAll(dest)
		return "", nil
	}

	note := c.Subject + "\n\n" + messageBody(c)
	if err := os.WriteFile(filepath.Join(dest, ".message"), []byte(note), 0o644); err != nil {
		return "", err
	}
	return stamp, nil
}

// Revert copies a snapshot's files back over the working copies.
func (s *snapshots) Revert(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.dir, filepath.Base(ref))
	entries, err := collectFiles(dir)
	if err != nil {
		return err
	}
	sort.Strings(entries)
	for _, rel := range entries {
		if rel == ".message" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.root, rel), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (s *snapshots) Describe(ref string) string { return ref }

// Branch and CommitsSince have no meaning outside a repository: there is no
// branch to be on and nothing to merge the snapshots into.
func (s *snapshots) Branch() string { return "" }

func (s *snapshots) CommitsSince(string) int { return 0 }

func collectFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

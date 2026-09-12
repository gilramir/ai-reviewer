package main

import (
	"strings"
	"testing"

	"github.com/gilramir/ai-reviewer/internal/review"
	"github.com/gilramir/ai-reviewer/internal/server"
)

func TestListenAddress(t *testing.T) {
	tests := []struct {
		name      string
		listen    string
		listenAll string
		want      string
		wantErr   bool
	}{
		{name: "neither", listen: defaultListen, want: defaultListen},
		{name: "just a port", listen: defaultListen, listenAll: "9000", want: "0.0.0.0:9000"},
		{name: "an address of its own", listen: "127.0.0.1:9000", want: "127.0.0.1:9000"},
		{name: "both", listen: "127.0.0.1:9000", listenAll: "8080", wantErr: true},
		// A whole address here would otherwise become "0.0.0.0:0.0.0.0:8080".
		{name: "an address where a port belongs", listen: defaultListen, listenAll: "0.0.0.0:8080", wantErr: true},
		{name: "a colon and a port", listen: defaultListen, listenAll: ":8080", wantErr: true},
		{name: "not a number", listen: defaultListen, listenAll: "eighty", wantErr: true},
		// Port 0 asks the kernel for whichever port is free, which nobody can
		// then type into a browser — and the banner has already been printed.
		{name: "port zero", listen: defaultListen, listenAll: "0", wantErr: true},
		{name: "above the range", listen: defaultListen, listenAll: "65536", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := listenAddress(tt.listen, tt.listenAll)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("listenAddress(%q, %q) = %q, want an error", tt.listen, tt.listenAll, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("listenAddress(%q, %q): %v", tt.listen, tt.listenAll, err)
			}
			if got != tt.want {
				t.Errorf("listenAddress(%q, %q) = %q, want %q", tt.listen, tt.listenAll, got, tt.want)
			}
		})
	}
}

// The flag is only worth anything if what it produces is what the auth check
// reads as "reachable from the network".
func TestListenAllIsNotLoopback(t *testing.T) {
	addr, err := listenAddress(defaultListen, "8080")
	if err != nil {
		t.Fatal(err)
	}
	if server.IsLoopback(addr) {
		t.Errorf("%q reads as loopback, so --no-auth would be allowed with it", addr)
	}
}

// The advice at shutdown is the only place many reviewers will be told how to
// land a review, so everything it has to say has to be in it: both merges, and
// under each the delete that works after that one.
func TestLanding(t *testing.T) {
	tests := []struct {
		name     string
		settings review.Settings
		want     []string
		notWant  []string
	}{
		{
			name:     "commits waiting",
			settings: review.Settings{Branch: "review/docs", BaseBranch: "main", Commits: 4},
			want: []string{
				"4 commits are waiting on review/docs",
				"git switch main",
				"git merge review/docs",
				"git merge --squash review/docs",
				"git commit",
				"git branch -d review/docs",
				"git branch -D review/docs",
			},
		},
		{
			name:     "one commit is not asked to be rather than one",
			settings: review.Settings{Branch: "review/docs", BaseBranch: "main", Commits: 1},
			want:     []string{"1 commit is waiting", "until you merge it", "message of your own"},
			notWant:  []string{"rather than 1"},
		},
		{
			// The branch it was cut from is gone, so the reviewer names one.
			name:     "no base branch",
			settings: review.Settings{Branch: "review/docs", Commits: 2},
			want:     []string{"git merge review/docs", "git merge --squash review/docs"},
		},
		{
			name:     "nothing to land",
			settings: review.Settings{Branch: "review/docs", BaseBranch: "main"},
			want:     []string{"Nothing is waiting", "git branch -d review/docs"},
			notWant:  []string{"git merge"},
		},
		{
			// Outside a repository the review keeps snapshots, and there is no
			// branch to merge or to advise about.
			name:     "outside a repository",
			settings: review.Settings{},
			notWant:  []string{"git"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := landing(tt.settings)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("the advice does not mention %q:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.notWant {
				if strings.Contains(got, unwanted) {
					t.Errorf("the advice mentions %q, and should not:\n%s", unwanted, got)
				}
			}
		})
	}
}

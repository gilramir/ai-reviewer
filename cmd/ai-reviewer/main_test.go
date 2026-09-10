package main

import (
	"testing"

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

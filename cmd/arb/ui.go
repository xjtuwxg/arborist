package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/xjtuwxg/arborist/internal/ui"
)

func runUI(args []string) {
	fl := flag.NewFlagSet("ui", flag.ExitOnError)
	listen := fl.String("listen", "127.0.0.1:7878", "address to listen on")
	tailscale := fl.Bool("tailscale", false, "listen on this machine's Tailscale address instead")
	refsFile := fl.String("refs", "", "load a full scan made by `arb refs --json`")
	root, rest := splitRoot(args)
	fl.Parse(rest)
	if root == "" {
		root = fl.Arg(0)
	}
	if root == "" {
		root = "."
	}
	if *tailscale {
		bin := "tailscale"
		if _, err := exec.LookPath(bin); err != nil { // the macOS app keeps its command line tool inside the bundle
			bin = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		}
		out, err := exec.Command(bin, "ip", "-4").Output()
		ip := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if err != nil || net.ParseIP(ip) == nil {
			fmt.Fprintln(os.Stderr, "arb ui: could not get the Tailscale address (is tailscale running?)")
			os.Exit(1)
		}
		_, port, _ := net.SplitHostPort(*listen)
		*listen = net.JoinHostPort(ip, port)
	}
	srv, err := ui.New(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "arb ui:", err)
		os.Exit(1)
	}
	srv.CheckSignIns()
	if *refsFile != "" {
		if err := srv.LoadRefs(*refsFile); err != nil {
			fmt.Fprintln(os.Stderr, "arb ui:", err)
			os.Exit(1)
		}
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "arb ui:", err)
		os.Exit(1)
	}
	fmt.Printf("Arborist is serving %s\n\n  http://%s/?token=%s\n\n", srv.Root, ln.Addr(), srv.Token)
	if err := http.Serve(ln, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "arb ui:", err)
		os.Exit(1)
	}
}

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Round 59 extracted the census helpers and left the wiring exactly as unpinned
// as it found it.
//
// The reason it gives for the extraction is explicit: "the wiring was three
// mutations deep in unpinned code, and nothing failed when the hook was
// deleted, silenced to Debug, or given a constant surface". After the fix, two
// of those three still pass `go test ./...`:
//
//	if wantCensus(*explain, *dryRun) {   ->   if wantCensus(*explain, false) {
//	if wantCensus(*explain, *dryRun) { st.OnEvent(censusHook(log)) }   ->   (deleted)
//
// TestCensusIsWiredForBothFlags tests `wantCensus` and
// TestCensusHookWritesAGreppableLine tests `censusHook`, and nothing tests that
// `cmdProxy` calls either. The two functions are now provably correct and
// provably unreached.
//
// It matters at the moment `ddev hostshift check` refuses a start on a test-28
// leak, because what it tells the developer to do next is: add the flag,
// restart, `| grep census`. If the wiring is gone, or if `--dry-run` no longer
// reaches it, that instruction returns nothing on a proxy that is leaking — and
// PLAN §5.8 makes `--dry-run` the mode you point at a live canonical checkout
// to decide whether a site needs hostshift at all.
//
// Through the real command, because the wiring is the thing under test:
// `--dry-run` alone, since that is the half the call site can drop while both
// unit tests stay green.
func TestR60TheProxyCommandActuallyInstallsTheCensus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A Tier 1 response header carrying a canonical origin: one rewrite,
		// one census event, and no body to depend on.
		w.Header().Set("Content-Location", "https://www.example.fi/x")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	// A port of our own, so the census can be read off a real request.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	dir := t.TempDir()
	type result struct {
		code   int
		errOut string
	}
	done := make(chan result, 1)
	go func() {
		code, _, errOut := run(t, "", cmdProxy,
			"-C", dir,
			"--upstream", up.URL,
			"--map", "https://www.example.fi=https://wt-a--example.ddev.site",
			"--listen", addr,
			"--dry-run")
		done <- result{code, errOut}
	}()

	// Wait for it to be listening. Only then is the signal handler registered,
	// which is what makes the SIGTERM below a drain rather than a kill.
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the proxy never started listening")
		}
		time.Sleep(20 * time.Millisecond)
	}

	req, err := http.NewRequest("GET", "http://"+addr+"/p", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "wt-a--example.ddev.site"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the proxy did not drain")
	}
	if got.code != exitOK {
		t.Fatalf("proxy exited %d:\n%s", got.code, got.errOut)
	}
	if !strings.Contains(got.errOut, "msg=census") {
		t.Errorf("`--dry-run` rewrote a Tier 1 response header and wrote no census\n"+
			"line, so `check`'s instruction at a test-28 refusal — add the flag,\n"+
			"restart, `| grep census` — returns nothing on a proxy that is\n"+
			"leaking. wantCensus and censusHook are both tested; nothing tests\n"+
			"that cmdProxy calls them. stderr was:\n%s", got.errOut)
	}
	// And the marker that says nothing was applied. Round 60 added it because a
	// dry-run census line was indistinguishable from a real rewrite, and this
	// test asserted only that *a* census line appeared — so passing `false`
	// where `*dryRun` belongs survived the whole suite.
	if !strings.Contains(got.errOut, "dry-run=true") {
		t.Errorf("this proxy is running with --dry-run and its census does not say\n"+
			"so, so a developer grepping it cannot tell a proxy that is rewriting\n"+
			"from one that is only reporting. stderr was:\n%s", got.errOut)
	}
}

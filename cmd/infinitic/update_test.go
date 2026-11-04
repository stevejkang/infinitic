package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stevejkang/infinitic/internal/service"
	"github.com/stevejkang/infinitic/internal/ui"
	"github.com/stevejkang/infinitic/internal/update"
)

// releases serves the given versions, the first one as latest.
func releases(t *testing.T, versions ...string) update.Source {
	t.Helper()
	files := map[string][]byte{}
	for _, v := range versions {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		body := "binary " + v
		must(t, tw.WriteHeader(&tar.Header{Name: "infinitic", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		must(t, err)
		tw.Close()
		gz.Close()
		name := fmt.Sprintf("infinitic_%s_%s_%s.tar.gz", v, runtime.GOOS, runtime.GOARCH)
		sum := sha256.Sum256(buf.Bytes())
		files["/dl/v"+v+"/"+name] = buf.Bytes()
		files["/dl/v"+v+"/infinitic_"+v+"_checksums.txt"] = []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/releases/latest" {
			fmt.Fprintf(w, `{"tag_name":"v%s"}`, versions[0])
			return
		}
		if data, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return update.Source{API: srv.URL + "/api", Download: srv.URL + "/dl", Client: srv.Client()}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

// updateApp returns an app whose output goes to the returned function's result.
func updateApp(t *testing.T, src update.Source, exe string, mgr service.Manager) (*app, func() string) {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	a := &app{p: ui.NewPrinter(out), mgr: mgr, wait: 2 * time.Second, upd: updater{src: src, exe: exe, brew: "brew"}}
	return a, func() string { data, _ := os.ReadFile(out.Name()); return string(data) }
}

func TestUpdateReplacesTheBinaryAndRestartsTheWatchAsItWas(t *testing.T) {
	setVersion(t, "0.4.0")
	home := t.TempDir()
	t.Setenv("INFINITIC_HOME", home)
	exe := filepath.Join(t.TempDir(), "infinitic")
	must(t, os.WriteFile(exe, []byte("binary 0.4.0"), 0o755))

	running := settings{Interval: time.Minute, Session: 80, Weekly: 98}
	mgr := &fakeManager{status: service.Status{Installed: true, Running: true, PID: 100}}
	mgr.onRestart = func() {
		mgr.status.PID = 200
		if _, err := recordWatch(home, runningWatch{PID: 200, Version: "0.5.0", Settings: running}); err != nil {
			t.Error(err)
		}
	}
	a, output := updateApp(t, releases(t, "0.5.0"), exe, mgr)
	if err := a.runUpdate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "binary 0.5.0" {
		t.Fatalf("binary is %q", got)
	}
	if mgr.restarts != 1 || len(mgr.installed) != 0 {
		t.Fatalf("restarts %d, installs %d: the watch must be restarted as registered, not reinstalled", mgr.restarts, len(mgr.installed))
	}
	for _, want := range []string{"updated infinitic 0.4.0 -> 0.5.0", "Restarted the background watch on 0.5.0 (pid 200) with the same settings: interval 1m, session 80, weekly 98."} {
		if !strings.Contains(output(), want) {
			t.Errorf("output lacks %q:\n%s", want, output())
		}
	}
}

func TestUpdateInstallsAGivenPrereleaseAndLeavesAStoppedWatchAlone(t *testing.T) {
	setVersion(t, "0.4.0")
	t.Setenv("INFINITIC_HOME", t.TempDir())
	exe := filepath.Join(t.TempDir(), "infinitic")
	must(t, os.WriteFile(exe, []byte("binary 0.4.0"), 0o755))
	mgr := &fakeManager{}
	a, output := updateApp(t, releases(t, "0.5.0", "0.6.0-rc1"), exe, mgr)
	if err := a.runUpdate(context.Background(), "v0.6.0-rc1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "binary 0.6.0-rc1" || mgr.restarts != 0 {
		t.Fatalf("binary %q, restarts %d", got, mgr.restarts)
	}
	if err := a.runUpdate(context.Background(), "0.4.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output(), "infinitic 0.4.0 is already installed") {
		t.Fatalf("output:\n%s", output())
	}
}

func TestUpdateOnHomebrewRunsBrewAndRefusesAVersion(t *testing.T) {
	setVersion(t, "0.4.0")
	t.Setenv("INFINITIC_HOME", t.TempDir())
	dir := t.TempDir()
	exe := filepath.Join(dir, "Caskroom", "infinitic", "0.4.0", "infinitic")
	must(t, os.MkdirAll(filepath.Dir(exe), 0o755))
	must(t, os.WriteFile(exe, nil, 0o755))
	log := filepath.Join(dir, "brew.log")
	brew := filepath.Join(dir, "brew")
	must(t, os.WriteFile(brew, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n[ \"$1\" = list ] && echo infinitic 0.5.0\nexit 0\n"), 0o755))

	a, output := updateApp(t, update.Source{}, exe, &fakeManager{})
	a.upd.brew = brew
	if err := a.runUpdate(context.Background(), "0.5.0-rc1"); err == nil || !strings.Contains(err.Error(), "managed by Homebrew, which has only the latest release") {
		t.Fatalf("pinned version on Homebrew: %v", err)
	}
	if err := a.runUpdate(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "upgrade --cask stevejkang/tap/infinitic\nlist --cask --versions infinitic\n" {
		t.Fatalf("brew ran:\n%s", calls)
	}
	if !strings.Contains(output(), "updated infinitic 0.4.0 -> 0.5.0") {
		t.Fatalf("output:\n%s", output())
	}
}

func TestUpdateNoticeComesFromTheCacheAndADailyCheck(t *testing.T) {
	setVersion(t, "0.4.0-rc1")
	home := t.TempDir()
	t.Setenv("INFINITIC_HOME", home)
	cache := update.Cache{Path: filepath.Join(home, "update.json")}

	a, output := updateApp(t, releases(t, "0.4.0"), "", nil)
	a.check = a.newUpdateCheck()
	time.Sleep(500 * time.Millisecond) // the daily check runs alongside the command
	a.notifyUpdate()
	if !strings.Contains(output(), "infinitic 0.4.0 is available. You have 0.4.0-rc1.") {
		t.Fatalf("output:\n%s", output())
	}
	if latest, at := cache.Load(); latest != "0.4.0" || time.Since(at) > time.Minute {
		t.Fatalf("cache %q %v", latest, at)
	}

	setVersion(t, "0.4.0")
	b, quiet := updateApp(t, update.Source{API: "http://127.0.0.1:1"}, "", nil)
	b.check = b.newUpdateCheck()
	if b.check.fresh != nil {
		t.Fatal("a recent cache must not ask GitHub again")
	}
	b.notifyUpdate()
	if quiet() != "" {
		t.Fatalf("up to date, yet printed:\n%s", quiet())
	}
}

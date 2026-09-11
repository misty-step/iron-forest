package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doctorToolStubs writes stub mise/go/pi/gh executables into bin. gh dispatches
// on its first argument so the auth and capability probes can both succeed.
func doctorToolStubs(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"mise", "go", "pi"} {
		writeDoctorStub(t, bin, name, "#!/bin/sh\nexit 0\n")
	}
	writeDoctorStub(t, bin, "gh", `#!/bin/sh
case "$1" in
  auth) exit 0 ;;
  api) echo true ;;
  *) exit 0 ;;
esac
`)
	return bin
}

func writeDoctorStub(t *testing.T, bin, name, body string) {
	t.Helper()
	path := filepath.Join(bin, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeDoctorEnvironment(t *testing.T, root string, content string) {
	t.Helper()
	path, err := serviceEnvPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctorOpenRouterServer returns a test server that accepts the key probe,
// records the request path, and reports back the Authorization header it
// received, so tests can assert the endpoint and that the key travels only in
// the header and never in evidence.
func doctorOpenRouterServer(t *testing.T, status int) (*httptest.Server, *string, *string) {
	t.Helper()
	var seenAuth string
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, &seenAuth, &seenPath
}

func TestCLIDoctorReportsHealthyChecks(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	bin := doctorToolStubs(t)
	home := t.TempDir()
	server, seenAuth, seenPath := doctorOpenRouterServer(t, http.StatusOK)
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("OPENROUTER_API_BASE", server.URL)
	writeDoctorEnvironment(t, root, "OPENROUTER_API_KEY=test\n")

	code, envelope, stderr := decodeEnvelope(t, "doctor", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
	}
	var payload doctorPayload
	decodePayload(t, envelope, &payload)
	if !payload.Healthy {
		t.Fatalf("healthy=%t, want true (checks=%+v)", payload.Healthy, payload.Checks)
	}
	if payload.Repo != "owner/name" {
		t.Fatalf("repo=%q, want owner/name", payload.Repo)
	}
	byName := map[string]doctorCheck{}
	for _, check := range payload.Checks {
		byName[check.Name] = check
	}
	for _, name := range []string{"mise", "go", "pi"} {
		if check := byName[name]; !check.OK || check.Result != doctorObserved {
			t.Fatalf("%s check=%+v, want observed ok", name, check)
		}
	}
	if check := byName["gh_auth"]; !check.OK || check.Result != doctorEvidenced {
		t.Fatalf("gh_auth check=%+v, want evidenced ok", check)
	}
	if check := byName["credential_file"]; !check.OK || check.Result != doctorObserved {
		t.Fatalf("credential_file check=%+v, want observed ok", check)
	}
	if check := byName["forge_capability"]; !check.OK || check.Result != doctorEvidenced {
		t.Fatalf("forge_capability check=%+v, want evidenced ok", check)
	}
	if check := byName["openrouter_key"]; !check.OK || check.Result != doctorEvidenced {
		t.Fatalf("openrouter_key check=%+v, want evidenced ok", check)
	}
	if *seenAuth != "Bearer test" {
		t.Fatalf("openrouter probe Authorization=%q, want Bearer test", *seenAuth)
	}
	if *seenPath != "/key" {
		t.Fatalf("openrouter probe path=%q, want /key", *seenPath)
	}
}

func TestCLIDoctorReportsMissingCredentialFile(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	bin := doctorToolStubs(t)
	home := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)

	code, envelope, stderr := decodeEnvelope(t, "doctor", "--json", "--root", root)
	if code != exitError {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitError, stderr)
	}
	var payload doctorPayload
	decodePayload(t, envelope, &payload)
	if payload.Healthy {
		t.Fatalf("healthy=%t, want false", payload.Healthy)
	}
	for _, check := range payload.Checks {
		if check.Name == "credential_file" && check.OK {
			t.Fatalf("credential_file check=%+v, want not ok", check)
		}
	}
}

func TestCLIDoctorNeverPrintsCredentialValue(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	bin := doctorToolStubs(t)
	home := t.TempDir()
	server, _, _ := doctorOpenRouterServer(t, http.StatusOK)
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("OPENROUTER_API_BASE", server.URL)
	const secret = "sk-openrouter-secret-value"
	writeDoctorEnvironment(t, root, "OPENROUTER_API_KEY="+secret+"\n")

	code, _, stderr := captureCLIOutput(t, func() int {
		return runSurfaceCommand([]string{"doctor", "--root", root})
	})
	if code != exitOK {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
	}
	if strings.Contains(stderr, secret) {
		t.Fatalf("doctor leaked credential value on stderr: %q", stderr)
	}
	_, envelope, _ := decodeEnvelope(t, "doctor", "--json", "--root", root)
	var payload doctorPayload
	decodePayload(t, envelope, &payload)
	for _, check := range payload.Checks {
		if strings.Contains(check.Evidence, secret) || strings.Contains(check.Reason, secret) {
			t.Fatalf("doctor leaked credential value in check %q: %+v", check.Name, check)
		}
	}
}

func TestCLIDoctorReportsOpenRouterKeyRejected(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	bin := doctorToolStubs(t)
	home := t.TempDir()
	server, _, _ := doctorOpenRouterServer(t, http.StatusUnauthorized)
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("OPENROUTER_API_BASE", server.URL)
	writeDoctorEnvironment(t, root, "OPENROUTER_API_KEY=test\n")

	code, envelope, stderr := decodeEnvelope(t, "doctor", "--json", "--root", root)
	if code != exitError {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitError, stderr)
	}
	var payload doctorPayload
	decodePayload(t, envelope, &payload)
	byName := map[string]doctorCheck{}
	for _, check := range payload.Checks {
		byName[check.Name] = check
	}
	check := byName["openrouter_key"]
	if check.OK || check.Result != doctorEvidenced || check.Evidence == "" {
		t.Fatalf("openrouter_key check=%+v, want evidenced not ok", check)
	}
}

func TestCLIDoctorReportsOpenRouterKeyProbeUnknown(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	bin := doctorToolStubs(t)
	home := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	// A closed test server makes the transport fail rather than answer, so the
	// check is unknown, not a diagnosed key fault.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	t.Setenv("OPENROUTER_API_BASE", server.URL)
	writeDoctorEnvironment(t, root, "OPENROUTER_API_KEY=test\n")

	code, envelope, stderr := decodeEnvelope(t, "doctor", "--json", "--root", root)
	if code != exitError {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitError, stderr)
	}
	var payload doctorPayload
	decodePayload(t, envelope, &payload)
	byName := map[string]doctorCheck{}
	for _, check := range payload.Checks {
		byName[check.Name] = check
	}
	check := byName["openrouter_key"]
	if check.OK || check.Result != doctorUnknown || check.Reason == "" {
		t.Fatalf("openrouter_key check=%+v, want unknown with reason", check)
	}
}

func TestDoctorOpenRouterKeyRoleScoped(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	bin := doctorToolStubs(t)
	home := t.TempDir()
	var probes int
	auths := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes++
		auths[r.Header.Get("Authorization")]++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("OPENROUTER_API_BASE", server.URL)
	writeDoctorEnvironment(t, root, "OPENROUTER_API_KEY_BUILDER=builder-key\nOPENROUTER_API_KEY_VERIFIER=verifier-key\n")

	code, envelope, stderr := decodeEnvelope(t, "doctor", "--json", "--root", root)
	if code != exitOK {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitOK, stderr)
	}
	var payload doctorPayload
	decodePayload(t, envelope, &payload)
	if !payload.Healthy {
		t.Fatalf("healthy=%t, want true (checks=%+v)", payload.Healthy, payload.Checks)
	}
	byName := map[string]doctorCheck{}
	for _, check := range payload.Checks {
		byName[check.Name] = check
		if strings.Contains(check.Evidence, "builder-key") || strings.Contains(check.Reason, "builder-key") || strings.Contains(check.Evidence, "verifier-key") || strings.Contains(check.Reason, "verifier-key") {
			t.Fatalf("doctor leaked credential value in check %q: %+v", check.Name, check)
		}
	}
	check := byName["openrouter_key"]
	if !check.OK || check.Result != doctorEvidenced {
		t.Fatalf("openrouter_key check=%+v, want evidenced ok", check)
	}
	if probes != 2 {
		t.Fatalf("openrouter probes=%d, want 2 distinct role keys", probes)
	}
	if auths["Bearer builder-key"] != 1 || auths["Bearer verifier-key"] != 1 {
		t.Fatalf("openrouter probe Authorization=%v, want both role keys once", auths)
	}
}

func TestDoctorOpenRouterKeyMissing(t *testing.T) {
	root := t.TempDir()
	writeCLIConfig(t, root, "exit 1")
	bin := doctorToolStubs(t)
	home := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	writeDoctorEnvironment(t, root, "OPENROUTER_API_KEY=\n")

	code, envelope, stderr := decodeEnvelope(t, "doctor", "--json", "--root", root)
	if code != exitError {
		t.Fatalf("code=%d, want %d (stderr=%q)", code, exitError, stderr)
	}
	var payload doctorPayload
	decodePayload(t, envelope, &payload)
	if payload.Healthy {
		t.Fatalf("healthy=%t, want false", payload.Healthy)
	}
	byName := map[string]doctorCheck{}
	for _, check := range payload.Checks {
		byName[check.Name] = check
	}
	check := byName["openrouter_key"]
	if check.OK || check.Result != doctorObserved || !strings.Contains(check.Evidence, "no OpenRouter key is configured") {
		t.Fatalf("openrouter_key check=%+v, want observed fail with no key configured", check)
	}
}

func TestDoctorSubprocessProbeBounded(t *testing.T) {
	oldTimeout := doctorProbeTimeout
	doctorProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { doctorProbeTimeout = oldTimeout })

	root := t.TempDir()
	bin := t.TempDir()
	writeDoctorStub(t, bin, "gh", "#!/bin/sh\n/bin/sleep 5\n")
	t.Setenv("PATH", bin)

	start := time.Now()
	_, _, err := doctorProbe(context.Background(), root, "gh", "auth", "status")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("doctor took %v, want subprocess probe bounded", elapsed)
	}
	if err == nil {
		t.Fatal("hung probe returned success")
	}
}

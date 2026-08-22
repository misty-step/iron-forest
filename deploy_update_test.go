package main

import (
	"os"
	"strings"
	"testing"
)

// The fenced-update shell script is the executable deployment contract, so a
// regression here is caught by asserting the guards the rejected Revision got
// wrong. These are content checks, but they pin the two blocking behaviors:
// an idle window must not require the Kernel to be stopped, and a fresh audit
// must be forced before restart instead of passively waited for afterward.
func TestDeployUpdateFenceGuards(t *testing.T) {
	script, err := os.ReadFile("deploy/install-service.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(script)

	if strings.Contains(content, ".data.kernel.running == false") {
		t.Fatal("update must not require the Kernel to be stopped; it must accept a running Kernel with no live Run")
	}
	if !strings.Contains(content, "./forest audit show --rescan --json") {
		t.Fatal("update must force a fresh audit with `forest audit show --rescan` before restart")
	}
	if strings.Contains(content, "audit_timeout_seconds") {
		t.Fatal("update must not passively wait for an audit pass after restart")
	}
	if !strings.Contains(content, `rm -f -- "$prev_binary"`) {
		t.Fatal("rollback must remove forest.prev so the restored pre-change .gitignore keeps the next clean-tree check clean")
	}
}

//go:build !windows

package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimeoutKillsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	child := fmt.Sprintf("import time;time.sleep(2);open(%q,'w').write('alive')", marker)
	fn := pythonFunction(t, fmt.Sprintf("import subprocess,sys,time\ndef handler(e,c):\n"+
		" subprocess.Popen([sys.executable,'-c',%q])\n print('child started',flush=True)\n time.sleep(30)\n", child))
	fn.Timeout = time.Second
	result := invoke(t, testRuntime(t), fn)
	if !result.Error || !strings.Contains(result.Log, "child started") {
		t.Fatalf("timeout result: %+v", result)
	}

	time.Sleep(2100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived: %v", err)
	}
}

func TestReleaseBusyAndIdleExpiry(t *testing.T) {
	r := testRuntime(t)
	fn := pythonFunction(t, "import time\ndef handler(e,c): time.sleep(30)")
	done := make(chan struct{})
	go func() { _, _ = r.Invoke(context.Background(), fn, []byte(`{}`)); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	var root string
	for root == "" {
		r.mu.Lock()
		if p := r.pools[fn.ARN]; p != nil {
			for e := range p.environments {
				root = e.root
			}
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("environment did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r.Release(fn.ARN)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("busy invocation did not end after Release")
	}

	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("busy environment remains: %v", err)
	}
	fn.Code = packageOf(t, map[string]string{"app.py": "def handler(e,c): return 'idle'"})
	invoke(t, r, fn)
	r.mu.Lock()
	for e := range r.pools[fn.ARN].environments {
		root = e.root
		e.expiry.Reset(time.Millisecond)
	}
	r.mu.Unlock()
	deadline = time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle environment did not expire")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

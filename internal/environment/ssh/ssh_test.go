package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/environment"
)

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`it's "x" $y`); got != `'it'\''s "x" $y'` {
		t.Fatalf("quote = %s", got)
	}
}

func TestPrefixDeterministic(t *testing.T) {
	e := &Env{spec: environment.Spec{Workdir: "/w", Env: map[string]string{"B": "2", "A": "1"}}}
	if got := e.prefix(); got != "cd '/w' && export A='1' && export B='2' && " {
		t.Fatalf("prefix = %q", got)
	}
}

// startSSHD launches a throwaway sshd on a free port with a temp host key
// and a temp client key, so the test never touches ~/.ssh.
func startSSHD(t *testing.T) (host string, port int, keyPath string) {
	t.Helper()
	sshd := "/usr/sbin/sshd"
	if _, err := os.Stat(sshd); err != nil {
		t.Skip("sshd not installed")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	hostKey := filepath.Join(dir, "host_key")
	keyPath = filepath.Join(dir, "client_key")
	for _, k := range []string{hostKey, keyPath} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", k).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v: %s", err, out)
		}
	}
	pub, _ := os.ReadFile(keyPath + ".pub")
	authKeys := filepath.Join(dir, "authorized_keys")
	os.WriteFile(authKeys, pub, 0o600)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = l.Addr().(*net.TCPAddr).Port
	l.Close()

	cmd := exec.Command(sshd, "-D", "-e", "-p", fmt.Sprint(port), "-h", hostKey,
		"-o", "ListenAddress=127.0.0.1",
		"-o", "AuthorizedKeysFile="+authKeys,
		"-o", "StrictModes=no", "-o", "UsePAM=no", "-o", "PasswordAuthentication=no",
		"-o", "PidFile=none", "-o", "LogLevel=ERROR")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sshd: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 50; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return "127.0.0.1", port, keyPath
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("sshd did not come up")
	return
}

func TestRoundTripAgainstLocalSSHD(t *testing.T) {
	host, port, key := startSSHD(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "remote")
	f := Factory{ExtraArgs: []string{"-p", fmt.Sprint(port), "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=no", "-o", "LogLevel=ERROR"}}
	env, err := f.New(environment.Spec{Kind: environment.KindSSH, Host: host, KeyPath: key, Workdir: dir, Env: map[string]string{"DISPATCH_TEST": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.CopyIn(ctx, strings.NewReader("hello\n"), "sub/in.txt"); err != nil {
		t.Fatal(err)
	}
	p, err := env.Exec(ctx, "sh", "-c", "cat sub/in.txt; echo $DISPATCH_TEST; pwd; cat > out.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(p.Stdin(), "piped\n")
	p.Stdin().Close()
	out, _ := io.ReadAll(p.Stdout())
	errOut, _ := io.ReadAll(p.Stderr())
	if code, err := p.Wait(); err != nil || code != 0 {
		t.Fatalf("wait code=%d err=%v stderr=%s", code, err, errOut)
	}
	if got := string(out); got != "hello\nyes\n"+dir+"\n" {
		t.Fatalf("stdout = %q", got)
	}
	r, err := env.CopyOut(ctx, "out.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "piped\n" {
		t.Fatalf("out.txt = %q", b)
	}
}

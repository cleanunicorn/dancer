package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dancer/internal/environment"
)

// TestContainerRoundTrip needs a working docker daemon; skipped otherwise.
func TestContainerRoundTrip(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	env, err := Factory{}.New(environment.Spec{Kind: environment.KindDocker, Image: "alpine:3.20", Workdir: dir, Env: map[string]string{"DANCER_TEST": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer env.Stop(context.Background())
	if env.(*Env).ContainerID() == "" {
		t.Fatal("no container id")
	}

	if err := env.CopyIn(ctx, strings.NewReader("hello\n"), "sub/in.txt"); err != nil {
		t.Fatal(err)
	}
	p, err := env.Exec(ctx, "sh", "-c", "cat sub/in.txt; echo $DANCER_TEST; pwd; echo $HOME; id -u; cat > out.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(p.Stdin(), "piped\n")
	p.Stdin().Close()
	out, _ := io.ReadAll(p.Stdout())
	if code, err := p.Wait(); err != nil || code != 0 {
		t.Fatalf("wait code=%d err=%v", code, err)
	}
	if got, want := string(out), fmt.Sprintf("hello\nyes\n/work\n/tmp\n%d\n", os.Getuid()); got != want {
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
	if err := env.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Exec(ctx, "true"); err == nil {
		t.Fatal("exec after stop should fail")
	}
}

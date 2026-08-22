package local

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cleanunicorn/dancer/internal/environment"
)

func TestExecCopyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	env, err := Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: dir, Env: map[string]string{"DANCER_TEST": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := env.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.CopyIn(ctx, strings.NewReader("hello\n"), "sub/in.txt"); err != nil {
		t.Fatal(err)
	}

	p, err := env.Exec(ctx, "sh", "-c", "cat sub/in.txt; echo $DANCER_TEST; cat > out.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(p.Stdin(), "piped\n")
	p.Stdin().Close()
	out, _ := io.ReadAll(p.Stdout())
	code, err := p.Wait()
	if err != nil || code != 0 {
		t.Fatalf("wait: code=%d err=%v", code, err)
	}
	if got := string(out); got != "hello\nyes\n" {
		t.Fatalf("stdout = %q", got)
	}

	r, err := env.CopyOut(ctx, "out.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	if string(b) != "piped\n" {
		t.Fatalf("out.txt = %q", b)
	}
}

func TestExitCode(t *testing.T) {
	env, _ := Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()})
	p, err := env.Exec(context.Background(), "sh", "-c", "exit 3")
	if err != nil {
		t.Fatal(err)
	}
	p.Stdin().Close()
	code, err := p.Wait()
	if err != nil || code != 3 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

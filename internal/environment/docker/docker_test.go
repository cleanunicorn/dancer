package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestSlugIsDockerSafe(t *testing.T) {
	cases := map[string]string{
		"C0123ABC/1700000000.123456": "c0123abc-1700000000.1234",
		"coder":                      "coder",
		"///":                        "env",
		"  spaced  name  ":           "spaced-name",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"C0123ABC/1700000000.123456", "Weird//Name!!", "x"} {
		got := slug(in)
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") || strings.Contains(got, "--") {
			t.Errorf("slug(%q) = %q is not a clean container name part", in, got)
		}
	}
}

func TestReuseKeyOnlyWhenScoped(t *testing.T) {
	for _, tc := range []struct {
		spec environment.Spec
		want string
	}{
		{environment.Spec{Reuse: environment.ReuseThread, ReuseKey: "t1"}, "t1"},
		{environment.Spec{Reuse: environment.ReuseDefinition, ReuseKey: "coder"}, "coder"},
		{environment.Spec{Reuse: environment.ReuseTask, ReuseKey: "t1"}, ""},
		{environment.Spec{ReuseKey: "t1"}, ""},
		{environment.Spec{Reuse: environment.ReuseThread}, ""},
	} {
		if got := reuseKey(tc.spec); got != tc.want {
			t.Errorf("reuseKey(%+v) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// TestContainerNaming checks the two halves of the naming rule that make
// reuse work: the container name changes when the spec changes (so an edited
// config gets a fresh container), and the home volume name does not (so the
// agent's login survives an image upgrade).
func TestContainerNaming(t *testing.T) {
	dir := t.TempDir()
	base := environment.Spec{Kind: environment.KindDocker, Image: "alpine:3.20", Workdir: dir,
		Reuse: environment.ReuseThread, ReuseKey: "C1/17.5"}
	mk := func(s environment.Spec) *Env {
		e, err := Factory{}.New(s)
		if err != nil {
			t.Fatal(err)
		}
		return e.(*Env)
	}
	a := mk(base)
	if a.nameFor("alpine:3.20") == "" || a.volume == "" {
		t.Fatal("a reused environment needs a container and volume name")
	}
	if b := mk(base); b.nameFor("alpine:3.20") != a.nameFor("alpine:3.20") || b.volume != a.volume {
		t.Fatal("same spec gave different names")
	}

	// Provisioning rebuilding the image must not adopt the container still
	// running the old build.
	if a.nameFor("dancer-env:aaaa") == a.nameFor("dancer-env:bbbb") {
		t.Error("a rebuilt image reused the container name")
	}

	changed := base
	changed.Image = "alpine:3.19"
	c := mk(changed)
	if c.volume != a.volume {
		t.Error("changing the image threw away the home volume")
	}

	other := base
	other.ReuseKey = "C1/99.9"
	d := mk(other)
	if d.nameFor("alpine:3.20") == a.nameFor("alpine:3.20") || d.volume == a.volume {
		t.Error("a different thread shared the container or volume")
	}

	throwaway := base
	throwaway.Reuse = environment.ReuseTask
	if e := mk(throwaway); e.nameFor("alpine:3.20") != "" || e.volume != "" {
		t.Errorf("a per-task environment should not be named: %q/%q", e.nameFor("alpine:3.20"), e.volume)
	}
}

// TestNewCreatesWorkdir: docker would create a missing bind-mount source as
// root, which then locks the agent out of its own working directory.
func TestNewCreatesWorkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "work")
	if _, err := (Factory{}).New(environment.Spec{Kind: environment.KindDocker, Image: "alpine:3.20", Workdir: dir}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("workdir was not created: %v", err)
	}
}

// TestReusedContainerSurvivesStop is the promise reuse makes: Stop leaves the
// container alone, the next task gets the same one, and what the agent wrote
// in $HOME (its session history, its login) is still there.
func TestReusedContainerSurvivesStop(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	state := t.TempDir()
	f := Factory{StateDir: state}
	spec := environment.Spec{
		Kind: environment.KindDocker, Image: "alpine:3.20", Workdir: t.TempDir(),
		Reuse: environment.ReuseThread, ReuseKey: "test/" + fmt.Sprint(os.Getpid()),
	}
	first, err := f.New(spec)
	if err != nil {
		t.Fatal(err)
	}
	e1 := first.(*Env)
	if err := e1.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Whatever happens, do not leave a named container behind.
	defer func() {
		_, _ = run(context.Background(), "docker", nil, "rm", "-f", e1.ContainerName())
		_, _ = run(context.Background(), "docker", nil, "volume", "rm", "-f", e1.volume)
	}()
	id1 := e1.ContainerID()

	p, err := e1.Exec(ctx, "sh", "-c", `mkdir -p "$HOME" && echo session > "$HOME/marker"`)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(); err != nil || code != 0 {
		t.Fatalf("write marker: code=%d err=%v", code, err)
	}

	if err := e1.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := run(ctx, "docker", nil, "inspect", "-f", "{{.State.Running}}", id1)
	if err != nil || strings.TrimSpace(out) != "true" {
		t.Fatalf("Stop removed or stopped a reused container: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(state, "containers", e1.ContainerName())); err != nil {
		t.Fatalf("no last-used stamp for the reused container: %v", err)
	}

	second, err := f.New(spec)
	if err != nil {
		t.Fatal(err)
	}
	e2 := second.(*Env)
	if err := e2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer e2.Stop(context.Background())
	if e2.ContainerID() != id1 {
		t.Fatalf("second task got container %s, want the shared %s", e2.ContainerID(), id1)
	}
	p2, err := e2.Exec(ctx, "sh", "-c", `cat "$HOME/marker"`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(p2.Stdout())
	if code, err := p2.Wait(); err != nil || code != 0 {
		t.Fatalf("read marker: code=%d err=%v", code, err)
	}
	if strings.TrimSpace(string(got)) != "session" {
		t.Fatalf("$HOME did not survive: %q", got)
	}
}

// TestReapRemovesIdleContainers: a reused container nobody has touched for
// longer than the ttl is retired, and one still in use is not.
func TestReapRemovesIdleContainers(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	state := t.TempDir()
	f := Factory{StateDir: state}
	spec := environment.Spec{
		Kind: environment.KindDocker, Image: "alpine:3.20", Workdir: t.TempDir(),
		Reuse: environment.ReuseThread, ReuseKey: "reap/" + fmt.Sprint(os.Getpid()),
	}
	env, err := f.New(spec)
	if err != nil {
		t.Fatal(err)
	}
	e := env.(*Env)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	name := e.ContainerName()
	defer func() {
		_, _ = run(context.Background(), "docker", nil, "rm", "-f", name)
		_, _ = run(context.Background(), "docker", nil, "volume", "rm", "-f", e.volume)
	}()

	// Still held by a live task: reaping must leave it alone even though
	// the stamp is ancient.
	old := time.Now().Add(-48 * time.Hour)
	marker := filepath.Join(state, "containers", name)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := f.Reap(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, "docker", nil, "inspect", "-f", "{{.Id}}", name); err != nil {
		t.Fatal("reap removed a container a task is using")
	}

	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := f.Reap(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, "docker", nil, "inspect", "-f", "{{.Id}}", name); err == nil {
		t.Fatal("idle container was not reaped")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("last-used stamp outlived its container")
	}
}

// TestProvisionRealImage builds a real derived image from a plain base. It
// downloads packages, so it only runs when asked for.
func TestProvisionRealImage(t *testing.T) {
	if os.Getenv("DANCER_DOCKER_PROVISION") == "" {
		t.Skip("set DANCER_DOCKER_PROVISION=1 to build a provisioned image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	spec := environment.Spec{
		Kind: environment.KindDocker, Image: "ubuntu:24.04", Workdir: t.TempDir(),
		Provision: &environment.Provision{Agents: []string{"claude"}},
	}
	env, err := Factory{}.New(spec)
	if err != nil {
		t.Fatal(err)
	}
	e := env.(*Env)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer e.Stop(context.Background())

	if e.Image() == spec.Image {
		t.Fatal("ubuntu was used unprovisioned")
	}
	if e.Home() != ProvisionedHome {
		t.Fatalf("home = %q, want %q", e.Home(), ProvisionedHome)
	}
	p, err := e.Exec(ctx, "sh", "-c", `command -v claude && command -v git && touch "$HOME/writable" && echo ok`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p.Stdout())
	if code, err := p.Wait(); err != nil || code != 0 {
		t.Fatalf("provisioned image is not usable: code=%d err=%v out=%s", code, err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected output %q", out)
	}
}

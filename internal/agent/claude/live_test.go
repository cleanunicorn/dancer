package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/environment"
	"github.com/cleanunicorn/dispatch/internal/environment/docker"
	"github.com/cleanunicorn/dispatch/internal/environment/local"
)

// TestLivePermissionRoundTrip drives the real claude CLI. Run with
// DISPATCH_LIVE=1; it costs a few cents (haiku) and needs `claude` logged in.
func TestLivePermissionRoundTrip(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") == "" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real claude CLI")
	}
	dir := t.TempDir()
	env, err := local.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	def := agent.Definition{Kind: agent.KindClaude, Model: "haiku", PermissionMode: agent.PermissionManual}
	run, err := New().Start(ctx, env, def, "Using Bash, run `touch created.txt`. Then reply with exactly: DONE")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()

	var session string
	var sawPermission, sawResult bool
	var decided bool
	for ev := range run.Events() {
		t.Logf("event %-16s tool=%s text=%.60q", ev.Type, ev.Tool, ev.Text)
		switch ev.Type {
		case agent.EventInit:
			session = ev.Session
		case agent.EventNeedsPermission:
			sawPermission = true
			if err := run.Decide(ctx, agent.PermissionDecision{ToolID: ev.ToolID, Allow: true}); err != nil {
				t.Fatalf("decide: %v", err)
			}
			decided = true
		case agent.EventError:
			t.Fatalf("agent error: %s", ev.Text)
		case agent.EventResult:
			sawResult = true
			if ev.Billing == agent.BillingUnknown {
				t.Errorf("result: billing unknown; init should have set it")
			}
		}
		if sawResult {
			break
		}
	}
	if session == "" || !sawPermission || !decided || !sawResult {
		t.Fatalf("session=%q permission=%v decided=%v result=%v", session, sawPermission, decided, sawResult)
	}
	if _, err := os.Stat(filepath.Join(dir, "created.txt")); err != nil {
		t.Fatalf("created.txt missing: %v", err)
	}

	// Follow-up turn on the same live process.
	if err := run.Send(ctx, "Reply with exactly: SECOND"); err != nil {
		t.Fatal(err)
	}
	second := false
	for ev := range run.Events() {
		if ev.Type == agent.EventResult {
			second = true
			break
		}
		if ev.Type == agent.EventError {
			t.Fatalf("agent error: %s", ev.Text)
		}
	}
	if !second {
		t.Fatal("no result for second turn")
	}
	if err := run.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Resume in a fresh process with the recorded session id.
	run2, err := New().Resume(ctx, env, def, session, "What was the exact filename you created earlier? Reply with just the name.")
	if err != nil {
		t.Fatal(err)
	}
	defer run2.Stop()
	var answer string
	for ev := range run2.Events() {
		if ev.Type == agent.EventResult {
			answer = ev.Text
			break
		}
		if ev.Type == agent.EventError {
			t.Fatalf("resume error: %s", ev.Text)
		}
	}
	if answer == "" || !strings.Contains(answer, "created.txt") {
		t.Fatalf("resume answer = %q", answer)
	}
}

// TestLiveQuestion checks AskUserQuestion answers travel back through
// updatedInput.answers. Run with DISPATCH_LIVE=1.
func TestLiveQuestion(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") == "" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real claude CLI")
	}
	env, _ := local.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	def := agent.Definition{Kind: agent.KindClaude, Model: "haiku", PermissionMode: agent.PermissionManual}
	run, err := New().Start(ctx, env, def, "Use the AskUserQuestion tool to ask whether I prefer Apple or Banana (header Fruit). After I answer, reply with exactly: CHOSEN=<label>")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	var answer string
	for ev := range run.Events() {
		switch ev.Type {
		case agent.EventQuestion:
			if len(ev.Questions) == 0 || len(ev.Questions[0].Options) < 2 {
				t.Fatalf("questions = %+v", ev.Questions)
			}
			if err := run.Decide(ctx, agent.PermissionDecision{ToolID: ev.ToolID, Allow: true, Answers: map[string]string{ev.Questions[0].Text: "Banana"}}); err != nil {
				t.Fatal(err)
			}
		case agent.EventError:
			t.Fatalf("agent error: %s", ev.Text)
		case agent.EventResult:
			answer = ev.Text
		}
		if answer != "" {
			break
		}
	}
	if !strings.Contains(answer, "Banana") {
		t.Fatalf("answer = %q", answer)
	}
}

// TestLiveUsage checks a subscription login's result is followed by the
// plan's usage. Run with DISPATCH_LIVE=1; skips on an API key.
func TestLiveUsage(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") == "" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real claude CLI")
	}
	env, _ := local.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	def := agent.Definition{Kind: agent.KindClaude, Model: "haiku", PermissionMode: agent.PermissionManual}
	run, err := New().Start(ctx, env, def, "Reply with exactly: hi")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	done := false
	for ev := range run.Events() {
		switch ev.Type {
		case agent.EventError:
			t.Fatalf("agent error: %s", ev.Text)
		case agent.EventResult:
			if ev.Billing != agent.BillingSubscription {
				t.Skipf("billing %q: usage is a subscription thing", ev.Billing)
			}
			done = true
		case agent.EventUsage:
			t.Logf("usage = %+v", ev.Usage)
			if !done {
				t.Fatal("usage came before the result")
			}
			if ev.Usage == nil || len(ev.Usage.Windows) < 2 || ev.Usage.Windows[0].Name != "5h" || ev.Usage.Windows[1].Name != "7d" {
				t.Fatalf("usage = %+v", ev.Usage)
			}
			if ev.Usage.Windows[0].ResetsAt.IsZero() || ev.Usage.Plan == "" {
				t.Fatalf("usage = %+v", ev.Usage)
			}
			return
		}
	}
	t.Fatal("no usage event")
}

// TestLiveSubAgentOneResult spawns a real sub-agent. The CLI ends the
// model's turn while the sub-agent runs and starts another when it is
// done; dispatch must report one result, after the sub-agent's answer.
func TestLiveSubAgentOneResult(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") == "" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real claude CLI")
	}
	env, err := local.Factory{}.New(environment.Spec{Kind: environment.KindLocal, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	def := agent.Definition{Kind: agent.KindClaude, Model: "haiku", PermissionMode: agent.PermissionManual, AllowedTools: []string{"Agent", "Bash(sleep:*)"}}
	run, err := New().Start(ctx, env, def, "Launch one Agent (subagent_type Explore) with the prompt: run the shell command sleep 15 then reply with the single word PONG. Do not wait for it and do not sleep yourself. Immediately reply with exactly: LAUNCHED. When the agent's completion notification arrives, reply with exactly the word it returned.")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()

	var results []string
	var subAgentSpoke bool
	deadline := time.After(3 * time.Minute)
	var quiet <-chan time.Time // armed by the result after the sub-agent spoke: a second one would follow soon
loop:
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				break loop
			}
			t.Logf("event %-16s parent=%.8s tool=%s text=%.60q", ev.Type, ev.ParentID, ev.Tool, ev.Text)
			switch ev.Type {
			case agent.EventText:
				if ev.ParentID != "" {
					subAgentSpoke = true
				}
			case agent.EventError:
				t.Fatalf("agent error: %s", ev.Text)
			case agent.EventResult:
				results = append(results, ev.Text)
				if subAgentSpoke && quiet == nil {
					quiet = time.After(10 * time.Second)
				}
			}
		case <-quiet:
			break loop
		case <-deadline:
			t.Fatal("no final result in time")
		}
	}
	if !subAgentSpoke || len(results) != 1 || !strings.Contains(strings.ToUpper(results[0]), "PONG") {
		t.Fatalf("sub-agent spoke=%v results=%q, want one result carrying the sub-agent's answer", subAgentSpoke, results)
	}
}

// TestLiveDockerLogin runs a turn inside a container provisioned from
// ubuntu:24.04 with nothing but the host's lent login, once as a throwaway
// container and once as a reused one whose $HOME is a volume. Run with
// DISPATCH_LIVE=1 and a docker daemon; the first run builds the image (~60s).
func TestLiveDockerLogin(t *testing.T) {
	if os.Getenv("DISPATCH_LIVE") == "" {
		t.Skip("set DISPATCH_LIVE=1 to run against the real claude CLI")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available")
	}
	if _, err := os.Stat(hostCredentials()); err != nil {
		t.Skip("no host login to lend")
	}
	for _, reuse := range []environment.Reuse{environment.ReuseTask, environment.ReuseThread} {
		t.Run(string(reuse), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			spec := environment.Spec{Kind: environment.KindDocker, Image: "ubuntu:24.04", Workdir: t.TempDir(), Provision: &environment.Provision{Agents: []string{"claude"}}}
			if reuse == environment.ReuseThread {
				spec.Reuse, spec.ReuseKey = reuse, "livelogin/"+filepath.Base(spec.Workdir)
			}
			env, err := docker.Factory{}.New(spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := env.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer env.Stop(context.Background())
			if name := env.(*docker.Env).ContainerName(); name != "" {
				defer func() {
					_ = exec.Command("docker", "rm", "-f", name).Run()
					out, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "name=dispatch-home-livelogin").Output()
					for _, v := range strings.Fields(string(out)) {
						_ = exec.Command("docker", "volume", "rm", v).Run()
					}
				}()
			}

			def := agent.Definition{Kind: agent.KindClaude, Model: "haiku", PermissionMode: agent.PermissionManual, Environment: spec}
			run, err := New().Start(ctx, env, def, "Reply with exactly: hi")
			if err != nil {
				t.Fatal(err)
			}
			defer run.Stop()
			var result string
			for ev := range run.Events() {
				t.Logf("event %-16s text=%.80q", ev.Type, ev.Text)
				switch ev.Type {
				case agent.EventError:
					t.Fatalf("agent error: %s", ev.Text)
				case agent.EventResult:
					result = ev.Text
				}
				if result != "" {
					break
				}
			}
			if !strings.Contains(result, "hi") {
				t.Fatalf("result = %q", result)
			}
		})
	}
}

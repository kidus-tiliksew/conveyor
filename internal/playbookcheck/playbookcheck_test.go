package playbookcheck

import (
	"flag"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func testTree() *cobra.Command {
	root := &cobra.Command{Use: "conveyor", Version: "v1"}
	root.PersistentFlags().String("server", "", "")
	run := func(*cobra.Command, []string) {}
	skills := &cobra.Command{Use: "skills"}
	install := &cobra.Command{Use: "install", Run: run}
	install.Flags().Bool("project", false, "")
	install.Flags().String("tool", "", "")
	skills.AddCommand(install)
	setup := &cobra.Command{Use: "setup"}
	setup.PersistentFlags().String("config", "", "")
	setup.AddCommand(&cobra.Command{Use: "create <name>", Run: run}, &cobra.Command{Use: "edit <name>", Run: run})
	root.AddCommand(skills, setup, &cobra.Command{Use: "run <task-id>", Run: run})
	return root
}

func TestExtractFindsShellAndInlineInvocations(t *testing.T) {
	markdown := "Run `conveyor skills install --project` first, then\n" +
		"`conveyor setup\ncreate main`.\n\n" +
		"The `conveyor` CLI and `command -v conveyor` are not commands to check.\n\n" +
		"```sh\n" +
		"export PATH=\"$HOME/.local/bin:$PATH\" # comment conveyor run\n" +
		"command -v conveyor conveyord\n" +
		"token=$(conveyor --server <server-url> run <task-id> | sed -n 's/x//p')\n" +
		"CONVEYOR_CONFIG=/tmp/c.yaml nohup conveyord -config \"$HOME/conveyor.yaml\" > log 2>&1 < /dev/null &\n" +
		"conveyor setup create main \\\n" +
		"  --config \"$HOME/.conveyor/client/conveyor.yaml\"\n" +
		"```\n\n" +
		"```yaml\ncommand: conveyor nothing\n```\n"
	got := Extract([]byte(markdown))
	want := []string{
		"line 1: conveyor skills install --project",
		"line 1: conveyor setup create main",
		"line 5: conveyor",
		"line 10: conveyor --server <server-url> run <task-id>",
		"line 11: conveyord -config $HOME/conveyor.yaml",
		"line 12: conveyor setup create main --config $HOME/.conveyor/client/conveyor.yaml",
	}
	var rendered []string
	for _, invocation := range got {
		rendered = append(rendered, invocation.String())
	}
	if strings.Join(rendered, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Extract =\n%s\nwant\n%s", strings.Join(rendered, "\n"), strings.Join(want, "\n"))
	}
}

func TestCheckCobraAcceptsRegisteredCommandsAndFlags(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--version"},
		{"--server", "<server-url>", "skills", "install", "--project", "--tool", "claude"},
		{"skills", "install", "--tool=codex"},
		{"setup", "create|edit", "main", "--config", "c.yaml"},
		{"run", "<task-id>"},
		{"skills", "install", "--help"},
	} {
		if err := CheckCobra(testTree(), args); err != nil {
			t.Errorf("CheckCobra(%q) = %v", args, err)
		}
	}
}

func TestCheckCobraRefusesUnregisteredCommandsAndFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"skils", "install"}, want: `unknown command "skils" for "conveyor"`},
		{args: []string{"skills", "instal"}, want: `unknown command "instal" for "conveyor skills"`},
		{args: []string{"skills", "install", "--projet"}, want: `unknown flag "--projet"`},
		{args: []string{"setup", "create|delete", "main"}, want: `unknown command "create|delete"`},
		{args: []string{"--sever", "x", "run"}, want: `unknown flag "--sever"`},
	} {
		err := CheckCobra(testTree(), test.args)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("CheckCobra(%q) = %v, want %s", test.args, err, test.want)
		}
	}
}

func TestCheckFlagSurface(t *testing.T) {
	daemon := flag.NewFlagSet("conveyord", flag.ContinueOnError)
	daemon.String("config", "", "")
	daemon.Bool("debug-addr-allow-remote", false, "")
	install := flag.NewFlagSet("conveyord install", flag.ContinueOnError)
	install.String("config", "", "")
	surface := FlagSurface{Name: "conveyord", Default: daemon, Verbs: map[string]*flag.FlagSet{
		"install": install, "version": flag.NewFlagSet("conveyord version", flag.ContinueOnError),
	}}
	for _, args := range [][]string{nil, {"version"}, {"-config", "c.yaml", "-debug-addr-allow-remote"}, {"--config=c.yaml"}, {"install", "--config", "c.yaml"}} {
		if err := CheckFlagSurface(surface, args); err != nil {
			t.Errorf("CheckFlagSurface(%q) = %v", args, err)
		}
	}
	for _, args := range [][]string{{"instal"}, {"install", "--env-file", "x"}, {"-confg", "c.yaml"}, {"version", "extra"}} {
		if err := CheckFlagSurface(surface, args); err == nil {
			t.Errorf("CheckFlagSurface(%q) accepted an unregistered command or flag", args)
		}
	}
}

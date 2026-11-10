package agent

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider/auth"
)

func TestParseAuthSetOptions(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		stdin bool
	}{
		{[]string{"set", "anthropic"}, false},
		{[]string{"set", "openai", "--stdin"}, true},
	} {
		opts, err := parseAuthSetOptions(tc.args)
		if err != nil || opts.provider != tc.args[1] || opts.stdin != tc.stdin {
			t.Fatalf("options=%+v error=%v", opts, err)
		}
	}
	for _, args := range [][]string{
		nil, {"set"}, {"login", "anthropic"}, {"set", ""}, {"set", "--stdin"},
		{"set", "anthropic", "synthetic-secret-key"},
		{"set", "anthropic", "--stdin", "synthetic-secret-key"},
		{"set", "anthropic", "--unknown"},
	} {
		_, err := parseAuthSetOptions(args)
		if err == nil {
			t.Fatal("invalid arguments accepted")
		}
		if strings.Contains(err.Error(), "synthetic-secret-key") {
			t.Fatal("argument error leaked a key")
		}
	}
}

func TestReadAuthKeyHiddenPrompt(t *testing.T) {
	oldTerminal, oldRead := authIsTerminal, authReadPassword
	t.Cleanup(func() { authIsTerminal, authReadPassword = oldTerminal, oldRead })
	in, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	authIsTerminal = func(int) bool { return true }
	authReadPassword = func(file *os.File, out io.Writer) ([]byte, error) {
		if file != in {
			t.Fatal("wrong terminal descriptor")
		}
		return readAuthTerminalPassword(strings.NewReader("synthetic-secret-key\r"), out)
	}
	var prompt bytes.Buffer
	key, err := readAuthKey(in, &prompt, false)
	if err != nil || key != "synthetic-secret-key" {
		t.Fatal("hidden input failed")
	}
	if strings.Contains(prompt.String(), "synthetic-secret-key") || !strings.Contains(prompt.String(), "hidden") {
		t.Fatal("prompt echoed secret or omitted hidden-input hint")
	}
	if _, err := readAuthKey(in, &prompt, true); err == nil {
		t.Fatal("--stdin accepted a visible terminal input")
	}
	authIsTerminal = func(int) bool { return false }
	if _, err := readAuthKey(in, &prompt, false); err == nil || !strings.Contains(err.Error(), "--stdin") {
		t.Fatal("non-terminal input did not require --stdin")
	}
}

func TestReadAuthTerminalPasswordCancel(t *testing.T) {
	var out bytes.Buffer
	if _, err := readAuthTerminalPassword(strings.NewReader("synthetic-secret-key\x03"), &out); err == nil {
		t.Fatal("Ctrl+C accepted a key")
	}
	if strings.Contains(out.String(), "synthetic-secret-key") {
		t.Fatal("canceled password echoed the key")
	}
}

func TestReadAuthKeyStdinBounded(t *testing.T) {
	key, err := readAuthKeyStdin(strings.NewReader("synthetic-secret-key\n"))
	if err != nil || key != "synthetic-secret-key\n" {
		t.Fatal("stdin read failed")
	}
	if _, err := readAuthKeyStdin(strings.NewReader(strings.Repeat("x", 64*1024+1))); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestAuthCommandHelperProcess(t *testing.T) {
	if os.Getenv("ZOT_AUTH_TEST_HELPER") != "1" {
		return
	}
	var args []string
	for index, arg := range os.Args {
		if arg == "--" {
			args = os.Args[index+1:]
			break
		}
	}
	if err := Run(args, "test"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestAuthCommandSetFromStdin(t *testing.T) {
	home := t.TempDir()
	models := `{"providers":{"test-headless":{"baseUrl":"https://invalid.example/v1","api":"openai","models":[{"id":"test-model"}]}}}`
	if err := os.WriteFile(filepath.Join(home, "models.json"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		input string
		ok    bool
	}{
		{"valid", []string{"auth", "set", "test-headless", "--stdin"}, "synthetic-secret-key\n", true},
		{"empty", []string{"auth", "set", "test-headless", "--stdin"}, "\n", false},
		{"multiline", []string{"auth", "set", "test-headless", "--stdin"}, "synthetic-secret-key\nsecond-key", false},
		{"no stdin flag", []string{"auth", "set", "test-headless"}, "synthetic-secret-key\n", false},
		{"positional secret", []string{"auth", "set", "test-headless", "synthetic-secret-key"}, "", false},
		{"unknown provider", []string{"auth", "set", "unknown-test-provider", "--stdin"}, "synthetic-secret-key\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestAuthCommandHelperProcess$", "--"}, tc.args...)...)
			cmd.Env = append(os.Environ(), "ZOT_AUTH_TEST_HELPER=1", "ZOT_HOME="+home)
			cmd.Stdin = strings.NewReader(tc.input)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("command error=%v, output=%s", err, output)
			}
			if bytes.Contains(output, []byte("synthetic-secret-key")) {
				t.Fatal("command output leaked key")
			}
		})
	}
	creds, err := auth.NewStore(filepath.Join(home, "auth.json")).Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.AdditionalAPIKeyCreds["test-headless"].APIKey != "synthetic-secret-key" {
		t.Fatal("command did not save key or invalid input replaced it")
	}
	for _, path := range []string{"config.json", "sessions", "models-cache.json"} {
		if _, err := os.Stat(filepath.Join(home, path)); !os.IsNotExist(err) {
			t.Fatalf("auth command created unrelated runtime state %s", path)
		}
	}
}

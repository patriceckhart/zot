package matrix

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	home := t.TempDir()
	c := Config{
		Homeserver:  "https://matrix.org",
		UserID:      "@zot:matrix.org",
		AccessToken: "syt_secret",
		DeviceID:    "ABCDEF",
		AutoJoin:    true,
	}
	if err := SaveConfig(home, c); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(ConfigPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("matrix.json must be 0600, got %v", fi.Mode().Perm())
	}
	got, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("round trip mismatch: %#v vs %#v", got, c)
	}
}

func TestLoadConfigMissingIsZero(t *testing.T) {
	got, err := LoadConfig(t.TempDir())
	if err != nil || got != (Config{}) {
		t.Fatalf("want zero config, got %#v err %v", got, err)
	}
}

func TestPaths(t *testing.T) {
	h := "/home/x/.zot"
	if ConfigPath(h) != filepath.Join(h, "matrix.json") {
		t.Fatal("config path")
	}
	if PIDPath(h) != filepath.Join(h, "matrix.pid") {
		t.Fatal("pid path")
	}
	if LogPath(h) != filepath.Join(h, "logs", "matrix.log") {
		t.Fatal("log path")
	}
	if CryptoStorePath(h) != filepath.Join(h, "matrix-crypto", "store.db") {
		t.Fatal("crypto path")
	}
}

func TestMaskToken(t *testing.T) {
	if MaskToken("short") != "<hidden>" {
		t.Fatal("short tokens fully hidden")
	}
	m := MaskToken("syt_averylongaccesstoken_tail")
	if m == "syt_averylongaccesstoken_tail" || len(m) < 8 {
		t.Fatalf("mask too weak: %q", m)
	}
}

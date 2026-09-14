package main

import "testing"

func TestBrowserCommand(t *testing.T) {
 const u = "https://example.test/authorize?state=a&redirect_uri=http%3A%2F%2F127.0.0.1%3A1234%2Fcallback"
 for platform, want := range map[string]string{"darwin":"open", "windows":"rundll32", "linux":"xdg-open"} {
  cmd, args, err := browserCommand(platform, u)
  if err != nil || cmd != want || args[len(args)-1] != u { t.Fatalf("%s: incorrect browser command", platform) }
 }
 for _, u := range []string{"http://example.test", "file:///tmp/test", "--help", "https://", "https://user:secret@example.test"} {
  if _, _, err := browserCommand("darwin", u); err == nil { t.Fatal("accepted invalid URL") }
 }
}

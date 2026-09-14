package main

import (
 "context"
 "errors"
 "net/url"
 "os/exec"
 "runtime"
 "time"
)

func browserCommand(platform, rawURL string) (string, []string, error) {
 u, err := url.Parse(rawURL)
 if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
  return "", nil, errors.New("authorization URL must be HTTPS")
 }
 switch platform {
 case "darwin": return "open", []string{rawURL}, nil
 case "windows": return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}, nil
 default: return "xdg-open", []string{rawURL}, nil
 }
}

// Explicit /mcp login is the user action; background discovery never calls this.
func openAuthorizationURL(rawURL string) error {
 program, args, err := browserCommand(runtime.GOOS, rawURL)
 if err != nil { return err }
 ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
 defer cancel()
 return exec.CommandContext(ctx, program, args...).Run()
}

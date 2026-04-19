package extensions

import "encoding/base64"

// base64DecodeStd is a one-line indirection around the stdlib so the
// tool wrapper can call decodeBase64 without growing its imports.
func base64DecodeStd(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

package telegram

import (
	"io"
	"os"
)

func defaultOpen(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

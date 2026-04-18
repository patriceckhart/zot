package telegram

import (
	"io"
	"os"
)

// stderr is a tiny hook so tests can redirect bot logging.
var stderr = func() io.Writer { return os.Stderr }

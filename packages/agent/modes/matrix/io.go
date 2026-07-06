package matrix

import (
	"io"
	"os"
)

// stderr is a tiny hook so tests can redirect bridge logging.
var stderr = func() io.Writer { return os.Stderr }

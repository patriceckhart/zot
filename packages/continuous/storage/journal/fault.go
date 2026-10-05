package journal

// faultHook is a private test seam. Production opens never install a hook.
// Checkpoints surround each persistence operation so tests can terminate a
// process or return an error without replacing the filesystem implementation.
type faultHook func(point string) error

func (hook faultHook) step(name string, perform func() error) error {
	if hook != nil {
		if err := hook("before." + name); err != nil {
			return err
		}
	}
	if err := perform(); err != nil {
		return err
	}
	if hook != nil {
		return hook("after." + name)
	}
	return nil
}

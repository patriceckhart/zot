package continuous

import (
	"encoding/json"
	"fmt"
)

// RequireAttachmentSupport checks the host's advertised capability before
// sending attachment fields that older hosts would otherwise silently ignore.
func RequireAttachmentSupport(status json.RawMessage) error {
	var decoded struct {
		Capabilities struct {
			Attachments bool `json:"attachments"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(status, &decoded); err != nil {
		return err
	}
	if !decoded.Capabilities.Attachments {
		return fmt.Errorf("%w: host does not support attachments, upgrade the host before submitting files or images", ErrUnsupported)
	}
	return nil
}

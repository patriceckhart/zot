package agent

type configSettingsStore struct{}

func (configSettingsStore) SetInlineImages(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.InlineImagesEnabled = &enabled
	return SaveConfig(cfg)
}

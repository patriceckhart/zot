package modes

import "testing"

func TestSnapViewportStartToImageBlock(t *testing.T) {
	chat := []string{
		"before",
		"    \x1b_Ga=T,f=100,c=60;AAAA\x1b\\",
		"",
		"",
		"    image - image/png - 100x100 - 1 KB",
		"after",
	}

	if got := snapViewportStartToImageBlock(chat, 2); got != 1 {
		t.Fatalf("start in first reserved row snapped to %d, want 1", got)
	}
	if got := snapViewportStartToImageBlock(chat, 3); got != 1 {
		t.Fatalf("start in middle reserved row snapped to %d, want 1", got)
	}
	if got := snapViewportStartToImageBlock(chat, 1); got != 1 {
		t.Fatalf("start on image row snapped to %d, want 1", got)
	}
	if got := snapViewportStartToImageBlock(chat, 4); got != 4 {
		t.Fatalf("start on metadata row snapped to %d, want 4", got)
	}
	if got := snapViewportStartToImageBlock(chat, 5); got != 5 {
		t.Fatalf("start after image snapped to %d, want 5", got)
	}
}

func TestSnapViewportStartToImageBlockNoopsOnPlainBlank(t *testing.T) {
	chat := []string{"before", "", "after"}
	if got := snapViewportStartToImageBlock(chat, 1); got != 1 {
		t.Fatalf("plain blank snapped to %d, want 1", got)
	}
}

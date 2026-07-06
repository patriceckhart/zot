package matrix

import "testing"

func TestBridgeInactiveByDefault(t *testing.T) {
	var b *Bridge
	if b.Active() {
		t.Fatal("nil bridge must report inactive")
	}
	b = &Bridge{}
	if b.Active() {
		t.Fatal("fresh bridge must report inactive")
	}
	if st := b.State(); st.Running {
		t.Fatal("state must be zero before Start")
	}
}

func TestBridgeSendersFailWhenStopped(t *testing.T) {
	b := &Bridge{}
	if err := b.SendImage(t.Context(), "x.png", ""); err == nil {
		t.Fatal("SendImage on a stopped bridge must error")
	}
	if err := b.SendDocument(t.Context(), "x.pdf", ""); err == nil {
		t.Fatal("SendDocument on a stopped bridge must error")
	}
}

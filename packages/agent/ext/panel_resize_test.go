package ext

import (
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestOnPanelResize(t *testing.T) {
	h := newHarness("panel-ext")
	t.Cleanup(func() { _ = h.hostW.Close() })
	sizes := make(chan [2]int, 2)
	h.ext.OnPanelResize("list", func(width, height int) { sizes <- [2]int{width, height} })
	go h.ext.Run()
	h.handshake(t)

	h.sendToExt(t, extproto.PanelResizeFromHost{Type: "panel_resize", PanelID: "other", Width: 99})
	h.sendToExt(t, extproto.PanelResizeFromHost{Type: "panel_resize", PanelID: "list", Width: -1})
	for _, want := range [][2]int{{96, 30}, {60, 24}} {
		h.sendToExt(t, extproto.PanelResizeFromHost{Type: "panel_resize", PanelID: "list", Width: want[0], Height: want[1]})
		select {
		case got := <-sizes:
			if got != want {
				t.Fatalf("size = %v, want %v", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("resize callback not called")
		}
	}
}

package agent

import "testing"

func TestMatrixSpecRegistered(t *testing.T) {
	if s := specFor("matrix-bot"); s == nil || s.name != "matrix" {
		t.Fatal("matrix-bot not registered")
	}
	if s := specFor("mx"); s == nil || s.name != "matrix" {
		t.Fatal("alias mx not registered")
	}
}

func TestSpecForSubcommand(t *testing.T) {
	if s := specFor("telegram-bot"); s == nil || s.name != "telegram" {
		t.Fatalf("telegram-bot not matched: %#v", s)
	}
	if s := specFor("tg"); s == nil || s.name != "telegram" {
		t.Fatal("alias tg not matched")
	}
	if specFor("nonsense") != nil {
		t.Fatal("unknown subcommand must not match")
	}
}

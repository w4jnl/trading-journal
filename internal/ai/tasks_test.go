package ai

import (
	"context"
	"errors"
	"testing"
)

func TestTasks(t *testing.T) {
	tk := NewTasks(nil, 1)
	release := make(chan struct{})
	if !tk.Start(7, "7:strategy:1", func(context.Context) error { <-release; return errors.New("no key") }) {
		t.Fatal("refused")
	}
	if tk.Start(7, "7:strategy:1", func(context.Context) error { return nil }) {
		t.Error("second start of a pending key accepted")
	}
	if !tk.Pending("7:strategy:1") || tk.Pending("7:strategy:2") {
		t.Error("pending state wrong")
	}
	close(release)
	if err := tk.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tk.Pending("7:strategy:1") {
		t.Error("still pending")
	}
	if tk.TakeError(8, "7:strategy:1") != "" || tk.TakeError(7, "7:strategy:1") != "no key" || tk.TakeError(7, "7:strategy:1") != "" {
		t.Error("error not read once by its owner")
	}
	var nilT *Tasks
	if nilT.Pending("x") || nilT.TakeError(1, "x") != "" {
		t.Error("nil tracker reported state")
	}
}

func TestDecodeJSON(t *testing.T) {
	var v struct {
		Name  string   `json:"name"`
		Rules []string `json:"rules"`
	}
	for _, in := range []string{
		`{"name":"ORB","rules":["a"]}`,
		"Here you go:\n```json\n{\"name\":\"ORB\",\"rules\":[\"a\"]}\n```",
		"<think>hmm {not json}</think>{\"name\":\"ORB\",\"rules\":[\"a\"]}",
	} {
		v.Name, v.Rules = "", nil
		if err := DecodeJSON(in, &v); err != nil || v.Name != "ORB" || len(v.Rules) != 1 {
			t.Errorf("DecodeJSON(%q) = %+v, %v", in, v, err)
		}
	}
	if err := DecodeJSON("no json here", &v); !errors.Is(err, ErrNotJSON) {
		t.Errorf("err = %v", err)
	}
	if err := DecodeJSON("{broken", &v); !errors.Is(err, ErrNotJSON) {
		t.Errorf("err = %v", err)
	}
}

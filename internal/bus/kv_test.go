package bus

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/those-who-run/ecu/internal/bus/bustest"
)

// paddock#88: the KV handle puts, gets and deletes a key in a bucket the
// bootstrap made, and a bucket that is missing is an error, not created.
func TestTheKVHandle(t *testing.T) {
	r := newRig(t, "live")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if !r.pub.Connected() {
		t.Fatal("not connected")
	}

	missing, err := r.pub.KV("live")
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Put(ctx, "ecu.status.live", []byte("{}")); err == nil {
		t.Fatal("a put to a bucket not yet made worked")
	}

	theirs := bustest.AddKV(t, r.js, "live")
	kv := missing // bound on first use: the bucket is there now
	if _, err := kv.Get(ctx, "ecu.status.live"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("get of a key never set: %v, want ErrNoKey", err)
	}
	if err := kv.Put(ctx, "ecu.status.live", []byte(`{"idle":true}`)); err != nil {
		t.Fatal(err)
	}
	got, err := kv.Get(ctx, "ecu.status.live")
	if err != nil || !bytes.Equal(got, []byte(`{"idle":true}`)) {
		t.Fatalf("get %q, %v", got, err)
	}
	if e, err := theirs.Get(ctx, "ecu.status.live"); err != nil || string(e.Value()) != `{"idle":true}` {
		t.Fatalf("a reader gets %v, %v", e, err)
	}
	if err := kv.Delete(ctx, "ecu.status.live"); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get(ctx, "ecu.status.live"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("get after delete: %v, want ErrNoKey", err)
	}
	if err := kv.Delete(ctx, "ecu.pit.live"); err != nil {
		t.Errorf("delete of a key with no value: %v", err)
	}
	if _, err := bustest.AddKV(t, r.srv.JetStream(), "ecu").Status(ctx); err != nil {
		t.Errorf("the ecu bucket: %v", err)
	}
}

package bus

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

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

// paddock#76, the guard against a second session.ended (AC5, as cut): clearing
// sessions.open with a revision check works once. A run that read the value
// before another changed or cleared it cannot clear it.
func TestDeleteIfIsACompareAndSet(t *testing.T) {
	r := newRig(t, "live")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bustest.AddKV(t, r.js, "ecu")
	kv, err := r.pub.KV("ecu")
	if err != nil {
		t.Fatal(err)
	}
	const key = "sessions.open.live"
	rev, err := kv.PutRev(ctx, key, []byte(`{"session_id":"A"}`))
	if err != nil {
		t.Fatal(err)
	}
	value, got, err := kv.Entry(ctx, key)
	if err != nil || got != rev || string(value) != `{"session_id":"A"}` {
		t.Fatalf("entry %q at %d (%v), want A at %d", value, got, err, rev)
	}
	// Two runs read it at rev; the first clears it, the second cannot.
	if err := kv.DeleteIf(ctx, key, rev); err != nil {
		t.Fatalf("the first clear: %v", err)
	}
	if err := kv.DeleteIf(ctx, key, rev); !errors.Is(err, ErrRevision) {
		t.Fatalf("the second clear: %v, want ErrRevision", err)
	}
	if _, _, err := kv.Entry(ctx, key); !errors.Is(err, ErrNoKey) {
		t.Fatalf("entry after the clear: %v, want ErrNoKey", err)
	}
	// A value written since is not cleared on an old revision.
	rev2, err := kv.PutRev(ctx, key, []byte(`{"session_id":"B"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.DeleteIf(ctx, key, rev); !errors.Is(err, ErrRevision) {
		t.Fatalf("a clear on an old revision: %v, want ErrRevision", err)
	}
	if err := kv.DeleteIf(ctx, key, rev2); err != nil {
		t.Fatalf("a clear on the current revision: %v", err)
	}
}

// Connect returns before the first connection is made, and the client buffers
// nothing until then, so a KV call before it fails at once with the client's
// "outbound buffer limit exceeded", however small: what live's status writer
// logged at each start (2026-10-31). Once connected, the same handle works.
func TestAKVCallBeforeTheFirstConnectionFails(t *testing.T) {
	srv := bustest.NewServer(t)
	bustest.AddKV(t, srv.JetStream(), "live")
	srv.Stop()
	pub, err := Connect(Config{URL: srv.URL(), Src: "live"}, (&logs{}).logger(), Options{ReconnectWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pub.nc.Close() })
	kv, err := pub.KV("live")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := kv.Put(ctx, "ecu.status.live", []byte("{}")); !errors.Is(err, nats.ErrReconnectBufExceeded) {
		t.Fatalf("a put before the first connection: %v, want %v", err, nats.ErrReconnectBufExceeded)
	}

	srv.Start()
	if err := pub.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, "ecu.status.live", []byte("{}")); err != nil {
		t.Fatalf("a put once connected: %v", err)
	}
}

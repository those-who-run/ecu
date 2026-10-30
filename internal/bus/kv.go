package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
)

// KV is one JetStream key-value bucket on the publisher's connection (paddock#88):
// the `live` bucket for ecu's status, and `ecu` for its own state (paddock#76).
// Each call waits on the network, within its ctx, so it is never made from the
// frame path: the caller's own goroutine makes it (internal/status's writer). The
// bucket is the bootstrap's to create (paddock's deploy/nats): ecu never creates
// one, and a bucket that is missing is an error on each call until it is there.
// A KV is safe for concurrent use.
type KV struct {
	js     jetstream.JetStream
	bucket string

	mu sync.Mutex
	kv jetstream.KeyValue // bound on first use
}

// ErrNoKey is Get's error for a key with no value.
var ErrNoKey = errors.New("no such key")

// KV is the bucket of that name on the publisher's connection.
func (p *Publisher) KV(bucket string) (*KV, error) {
	// A JetStream context of its own: the publisher's allows one async publish in
	// flight, for the order of EVENTS, which a KV's synchronous Put has no use for.
	js, err := jetstream.New(p.nc)
	if err != nil {
		return nil, err
	}
	return &KV{js: js, bucket: bucket}, nil
}

// Connected says the connection to NATS is up now (/healthz, paddock#88 R4).
func (p *Publisher) Connected() bool { return p.nc.IsConnected() }

func (k *KV) bind(ctx context.Context) (jetstream.KeyValue, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.kv != nil {
		return k.kv, nil
	}
	kv, err := k.js.KeyValue(ctx, k.bucket)
	if err != nil {
		return nil, fmt.Errorf("bucket %s: %w", k.bucket, err)
	}
	k.kv = kv
	return kv, nil
}

// Put sets key to value.
func (k *KV) Put(ctx context.Context, key string, value []byte) error {
	kv, err := k.bind(ctx)
	if err == nil {
		_, err = kv.Put(ctx, key, value)
	}
	return err
}

// Delete removes key's value, so a reader sees none; a key with none already is
// no error.
func (k *KV) Delete(ctx context.Context, key string) error {
	kv, err := k.bind(ctx)
	if err == nil {
		err = kv.Delete(ctx, key)
	}
	return err
}

// Get is key's value, or ErrNoKey when it has none.
func (k *KV) Get(ctx context.Context, key string) ([]byte, error) {
	kv, err := k.bind(ctx)
	if err != nil {
		return nil, err
	}
	e, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	return e.Value(), nil
}
